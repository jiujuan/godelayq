package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/uptrace/bun"
)

// outbox.go 实现事务性事件出口与其中继器（TASK-RE07，设计文档 §7.7）。
//
// 写侧：UpdateWithEvents 把任务快照与出箱事件放进同一个事务（本接口存在的意义）；
// AppendOutbox 只入箱不写快照（emit 通道）。实现方只写不发——投递由 OutboxRelay 接管，
// 双发禁令由 core/state.go 与本文件共同守住。
//
// 读侧：中继器按 seq 批量取 pending → 回填 OutboxSeq 后 Publish 进事件总线 →
// 同事务标记 dispatched。崩溃重放按 at-least-once 语义可能重复投递，
// 读侧（observe.EventLog）按 outbox_seq 去重。

// outboxRow 是 job_outbox 表的 bun 模型。表名、列名是冻结契约（schema.go 迁移 v4）。
type outboxRow struct {
	bun.BaseModel `bun:"table:job_outbox"`

	Seq          int64          `bun:"column:seq,autoincrement"`
	JobID        string         `bun:"column:job_id"`
	Type         string         `bun:"column:type"`
	PayloadJSON  string         `bun:"column:payload_json"`
	Status       string         `bun:"column:status"`
	CreatedAt    int64          `bun:"column:created_at"`
	DispatchedAt sql.NullInt64  `bun:"column:dispatched_at"`
}

// 编译期钉住接口实现：core.EventOutbox 的方法集一旦改动，这里立刻报错。
var _ core.EventOutbox = (*Store)(nil)

// 出箱行的封闭状态词表。
const (
	outboxStatusPending    = "pending"
	outboxStatusDispatched = "dispatched"
)

// UpdateWithEvents 在同一事务里落任务快照与出箱事件（core.EventOutbox）。
//
// 事务内容：快照 upsert（与直写路径共享同一条冲突更新链）+ 出箱批量 INSERT +
// 终态时的留痕淘汰（含账本级联，trimTerminalTx 复用）。任何一步失败整体回滚——
// "状态落了事件没了"的窗口由本方法关闭。
//
// 刻意不含 attempt 行：账本的 begin/finish 由 worker 协程单独调用，崩溃遗留的
// running 行由 Restore 收口为 abandoned（自愈），不进本事务。
func (s *Store) UpdateWithEvents(snapshot core.JobSnapshot, events []core.Event) error {
	err := s.h.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := upsertSnapshot(tx, snapshot); err != nil {
			return err
		}
		if len(events) > 0 {
			if err := insertOutboxEvents(ctx, tx, events); err != nil {
				return err
			}
		}
		if core.JobStatus(snapshot.Status).IsTerminal() {
			return trimTerminalTx(ctx, tx, s.historyLimit.Load(), time.Duration(s.historyTTL.Load()))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("jobs: update with events %s: %w", snapshot.ID, err)
	}
	return nil
}

// AppendOutbox 只入箱不写快照（core.EventOutbox）：emit 通道的事件没有自己的
// 状态写，独立事务——只承担"不丢"，与状态同事务的原子性由 UpdateWithEvents 承担。
func (s *Store) AppendOutbox(events []core.Event) error {
	if len(events) == 0 {
		return nil
	}
	err := s.h.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		return insertOutboxEvents(ctx, tx, events)
	})
	if err != nil {
		return fmt.Errorf("jobs: append outbox: %w", err)
	}
	return nil
}

// insertOutboxEvents 把一批事件折成出箱行插入。调用方已持事务。
func insertOutboxEvents(ctx context.Context, tx bun.Tx, events []core.Event) error {
	rows := make([]outboxRow, 0, len(events))
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			// core.Event 的字段全是可编码标量，这里到不了；到不了也必须显式失败
			// 而不是落一行坏 payload。
			return fmt.Errorf("jobs: marshal outbox event %s/%s: %w", event.JobID, event.Type, err)
		}
		rows = append(rows, outboxRow{
			JobID:       event.JobID,
			Type:        string(event.Type),
			PayloadJSON: string(payload),
			Status:      outboxStatusPending,
			CreatedAt:   time.Now().UnixNano(),
		})
	}
	if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("jobs: insert outbox batch: %w", err)
	}
	return nil
}

// OutboxRelayOptions 是中继器的参数。零值一律回到 core 的默认，
// 取值口径与观测层批量写入器一致（batcher 那一套）。
type OutboxRelayOptions struct {
	// FlushInterval 批量周期，也是事件到 WS/SSE 的可见延迟上界。
	// <=0 用 core.DefaultOutboxFlushInterval。
	FlushInterval time.Duration
	// BatchSize 单批投递条数，<=0 用 defaultOutboxBatchSize。
	BatchSize int
	// RetentionAge 已投递行的保留时长，<=0 用 core.DefaultOutboxRetentionAge。
	RetentionAge time.Duration
	// MaxPending pending 积压上限，<=0 用 core.DefaultOutboxMaxPending；
	// 超过按最旧截断并记 error（截断量进日志，不出现在读数里）。
	MaxPending int64
}

// defaultOutboxBatchSize 单批投递条数：够小（与单连接任务库的其他写入排队错峰），
// 够用（200ms 周期 × 256 条 = 每秒五万事件的消化上限，远超现实负载）。
const defaultOutboxBatchSize = 256

// OutboxRelay 把 job_outbox 的 pending 事件投递给事件总线。
//
// 单协程、按 seq 顺序投递（同一任务的事件单调有序）；投递成功（Publish 返回即
// 入了订阅缓冲，满则丢弃是订阅端既有契约）后同事务标记 dispatched；
// 启动先重放一轮残留 pending（崩溃补投）。Stop 幂等，等在途批处理完。
type OutboxRelay struct {
	h            *Handle
	bus          *core.EventBus
	interval     time.Duration
	batchSize    int
	retentionAge time.Duration
	maxPending   int64
	logger       *slog.Logger

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewOutboxRelay 用本库句柄建出箱中继器（TASK-RE07）。挂在 Store 上让装配方
// 不必摸 Handle 类型；迁移已由 Open 应用（v4），这里只做参数折算。
func (s *Store) NewOutboxRelay(bus *core.EventBus, opts OutboxRelayOptions, logger *slog.Logger) (*OutboxRelay, error) {
	if bus == nil {
		return nil, fmt.Errorf("jobs: NewOutboxRelay requires an event bus")
	}
	if logger == nil {
		logger = slog.Default()
	}
	interval := opts.FlushInterval
	if interval <= 0 {
		interval = core.DefaultOutboxFlushInterval
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultOutboxBatchSize
	}
	retentionAge := opts.RetentionAge
	if retentionAge <= 0 {
		retentionAge = core.DefaultOutboxRetentionAge
	}
	maxPending := opts.MaxPending
	if maxPending <= 0 {
		maxPending = core.DefaultOutboxMaxPending
	}
	return &OutboxRelay{
		h:            s.h,
		bus:          bus,
		interval:     interval,
		batchSize:    batchSize,
		retentionAge: retentionAge,
		maxPending:   maxPending,
		logger:       logger,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}, nil
}

// Start 起中继协程：先重放一轮残留 pending（崩溃补投），再进周期循环。
// 只许调用一次——重复 Start 会有两个协程抢同一批行，顺序保证失效。
func (r *OutboxRelay) Start() {
	go func() {
		defer close(r.doneCh)
		// 启动重放：崩溃前 publish 了没标记、或根本没来得及投的行，在这里补投。
		if err := r.dispatchBatch(); err != nil {
			r.logger.Error("outbox replay round failed", "error", err)
		}
		r.prune()
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stopCh:
				return
			case <-ticker.C:
				if err := r.dispatchBatch(); err != nil {
					r.logger.Error("outbox dispatch round failed", "error", err)
				}
				r.prune()
			}
		}
	}()
}

// Stop 停止中继并等在途批处理完。幂等：重复调用返回第一次的结果。
// 关停顺序上它必须先于事件总线的订阅者关闭——发布落在已关闭的订阅通道上会 panic
//（cmd/server 的 defer 位次守这条）。
func (r *OutboxRelay) Stop() error {
	r.stopOnce.Do(func() { close(r.stopCh) })
	<-r.doneCh
	return nil
}

// dispatchBatch 取一批 pending，按 seq 顺序投递，全批投完同事务标记 dispatched。
// 中途失败（取数/标记）：本轮中止，下轮重取同一批——at-least-once，读侧去重。
func (r *OutboxRelay) dispatchBatch() error {
	var rows []outboxRow
	if err := r.h.NewSelect().Model(&rows).
		Where("status = ?", outboxStatusPending).
		OrderExpr("seq ASC").
		Limit(int64(r.batchSize)).
		Scan(bgCtx); err != nil {
		return fmt.Errorf("jobs: select outbox pending: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	for i := range rows {
		var event core.Event
		if err := json.Unmarshal([]byte(rows[i].PayloadJSON), &event); err != nil {
			// 单行坏 payload 不挡整批：跳过并标记 dispatched（重放也修不好它），
			// error 级留痕是它得到的唯一仪式。
			r.logger.Error("outbox event payload is not decodable; skipping",
				"seq", rows[i].Seq, "job_id", rows[i].JobID, "error", err)
			rows[i].Status = outboxStatusDispatched
			continue
		}
		// OutboxSeq 在投递时回填：读侧（observe.EventLog）按它去重，
		// WS/SSE 的客户端拿它识别"这是重放的旧事件"。
		event.OutboxSeq = rows[i].Seq
		r.bus.Publish(event)
		rows[i].Status = outboxStatusDispatched
	}

	err := r.h.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		for i := range rows {
			if _, execErr := tx.NewUpdate().Model((*outboxRow)(nil)).
				Set("status = ?", outboxStatusDispatched).
				Set("dispatched_at = ?", time.Now().UnixNano()).
				Where("seq = ?", rows[i].Seq).
				Where("status = ?", outboxStatusPending).
				Exec(ctx); execErr != nil {
				return fmt.Errorf("jobs: mark outbox dispatched seq=%d: %w", rows[i].Seq, execErr)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 一批取满说明可能还有积压，立即再跑一轮而不是等下一个周期——
	// 崩溃恢复后的重放因此收敛得快，平时这行 rarely 触发。
	if len(rows) == r.batchSize {
		return r.dispatchBatch()
	}
	return nil
}

// pendingOutboxCount 统计出箱表中待投递行数。中继器的积压上限判定（prune）与
// 运维读数（Store.DBRuntime，G4/Q05）共用这一条查询——积压的语义只有一份，
// 截断判定与 /admin/runtime 读数不许分叉。
// 入参收 *Handle 而不是 bun.IDB：Handle 的 Dialect() 被 engine.DB 遮蔽成
// engine.Dialect，不再满足 bun.IDB，而这里也不需要更宽的入参。
func pendingOutboxCount(db *Handle) (int64, error) {
	count, err := db.NewSelect().Model((*outboxRow)(nil)).
		Where("status = ?", outboxStatusPending).
		Count(bgCtx)
	if err != nil {
		return 0, fmt.Errorf("jobs: count pending outbox rows: %w", err)
	}
	return int64(count), nil
}

// prune 做两件清理：dispatched 行按 TTL 淘汰；pending 超过积压上限时按最旧截断。
// 两者都在各自的小事务里；失败只记日志——清理是下限保障，不影响投递正确性。
func (r *OutboxRelay) prune() {
	cutoff := time.Now().Add(-r.retentionAge).UnixNano()
	if _, err := r.h.NewDelete().Model((*outboxRow)(nil)).
		Where("status = ?", outboxStatusDispatched).
		Where("dispatched_at < ?", cutoff).
		Exec(bgCtx); err != nil {
		r.logger.Error("failed to prune dispatched outbox rows", "error", err)
	}

	count, err := pendingOutboxCount(r.h)
	if err != nil {
		r.logger.Error("failed to count pending outbox rows", "error", err)
		return
	}
	if int64(count) <= r.maxPending {
		return
	}
	excess := int64(count) - r.maxPending
	res, err := r.h.NewDelete().Model((*outboxRow)(nil)).
		Where("status = ?", outboxStatusPending).
		Where("seq <= (SELECT MAX(seq) - ? FROM job_outbox WHERE status = ?)", r.maxPending, outboxStatusPending).
		Exec(bgCtx)
	if err != nil {
		r.logger.Error("failed to truncate pending outbox backlog", "error", err)
		return
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		r.logger.Error("pending outbox backlog exceeded the limit; oldest rows truncated",
			"pending", count, "limit", r.maxPending, "truncated", affected, "unaccounted", excess-affected)
	}
}
