package jobs

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/uptrace/bun"
)

// attempts.go 实现 core.AttemptLedger（TASK-RE02）：job_attempts 表上的逐次尝试账本。
// 与 jobs 表同库，任务快照的终态淘汰连带删账本行（db.go 的 trimTerminal 级联）；
// begin/finish 是 upsert，同 (job_id, seq) 重放幂等——崩溃重放不产生重复行。

// 编译期钉住接口实现：core.AttemptLedger 的方法集一旦改动，这里立刻报错。
var _ core.AttemptLedger = (*Store)(nil)

// attemptRow 是 job_attempts 表的 bun 模型。表名、列名是冻结契约（schema.go 迁移 v2），
// tag 全部显式给出，不依赖 bun 的命名推断。可空列用 sql.Null*：
// "没有退出码可言"与"退出码为 0"必须是两回事（core.Attempt 的 Set 标记互转见
// attemptRecord/attemptOf）。
type attemptRow struct {
	bun.BaseModel `bun:"table:job_attempts"`

	JobID         string         `bun:"column:job_id,pk"`
	Seq           int64          `bun:"column:seq,pk"`
	Status        string         `bun:"column:status"`
	TriggerSource string         `bun:"column:trigger_source"`
	StartedAt     int64          `bun:"column:started_at"`
	FinishedAt    sql.NullInt64  `bun:"column:finished_at"`
	DurationMs    int64          `bun:"column:duration_ms"`
	Class         sql.NullString `bun:"column:class"`
	// Retryable 列与全仓布尔列同一先例（observe 的 truncated）：INTEGER 0/1，
	// 模型用 NullInt64 承载，bool↔int 互转在 attemptRecord/attemptOf——
	// sql.NullBool 直绑会在 PG 上报"integer 列收到 boolean 参数"。
	Retryable  sql.NullInt64  `bun:"column:retryable"`
	ExitCode   sql.NullInt64  `bun:"column:exit_code"`
	Signal     sql.NullString `bun:"column:signal"`
	HTTPStatus sql.NullInt64  `bun:"column:http_status"`
	ErrorText  sql.NullString `bun:"column:error_text"`
}

// BeginAttempt 记一次尝试的开始：status=running、trigger/started_at 以 rec 为准。
// upsert：调度器对同一 (job_id, seq) 的重放（理论上不该发生，防御性幂等）覆盖旧行。
func (s *Store) BeginAttempt(jobID string, rec core.Attempt) error {
	if err := s.upsertAttempt(rec); err != nil {
		return fmt.Errorf("jobs: begin attempt %s#%d: %w", jobID, rec.Seq, err)
	}
	return nil
}

// FinishAttempt 收口一次尝试：status/结论字段以 rec 为准（FinishedAt/DurationMs
// 由调度器填好再传入）。收口一个没 begin 过的 seq 会整行插入（upsert 语义）：
// 账本不因 begin 的单点失败而永远缺一行。
func (s *Store) FinishAttempt(jobID string, seq int, rec core.Attempt) error {
	if err := s.upsertAttempt(rec); err != nil {
		return fmt.Errorf("jobs: finish attempt %s#%d: %w", jobID, seq, err)
	}
	return nil
}

// upsertAttempt 把一行账本写进表：冲突目标 (job_id, seq)，更新全部业务列。
func (s *Store) upsertAttempt(rec core.Attempt) error {
	row := attemptRecord(rec)
	if _, err := s.h.NewInsert().Model(&row).
		On("CONFLICT (job_id, seq) DO UPDATE").
		Set("status = EXCLUDED.status").
		Set("trigger_source = EXCLUDED.trigger_source").
		Set("started_at = EXCLUDED.started_at").
		Set("finished_at = EXCLUDED.finished_at").
		Set("duration_ms = EXCLUDED.duration_ms").
		Set("class = EXCLUDED.class").
		Set("retryable = EXCLUDED.retryable").
		Set("exit_code = EXCLUDED.exit_code").
		Set("signal = EXCLUDED.signal").
		Set("http_status = EXCLUDED.http_status").
		Set("error_text = EXCLUDED.error_text").
		Exec(bgCtx); err != nil {
		return fmt.Errorf("jobs: upsert attempt %s#%d: %w", rec.JobID, rec.Seq, err)
	}
	return nil
}

// AbandonAttempts 把 jobID 上 seq <= uptoSeq、仍处于 running 的尝试收口为 abandoned。
// 只动 running 行：finished 的历史行无论多旧都不是本次恢复的对象。
// duration 按 started_at 补算——那一次尝试实际跑了多久没有准确答案，
// "从开始到确认放弃"是账本上唯一诚实又可算的值。
func (s *Store) AbandonAttempts(jobID string, uptoSeq int, at time.Time) error {
	now := at.UnixNano()
	if _, err := s.h.NewUpdate().Model((*attemptRow)(nil)).
		Set("status = ?", core.AttemptAbandoned.String()).
		Set("finished_at = ?", now).
		Set("duration_ms = ? - started_at", now).
		Where("job_id = ?", jobID).
		Where("seq <= ?", uptoSeq).
		Where("status = ?", core.AttemptRunning.String()).
		Exec(bgCtx); err != nil {
		return fmt.Errorf("jobs: abandon attempts %s<=%d: %w", jobID, uptoSeq, err)
	}
	return nil
}

// Attempts 按 seq 降序读某任务最近 limit 条（limit <= 0 用 core.DefaultAttemptsKeepLast）。
func (s *Store) Attempts(jobID string, limit int) ([]core.Attempt, error) {
	if limit <= 0 {
		limit = core.DefaultAttemptsKeepLast
	}
	var rows []attemptRow
	if err := s.h.NewSelect().Model(&rows).
		Where("job_id = ?", jobID).
		OrderExpr("seq DESC").
		Limit(int64(limit)).
		Scan(bgCtx); err != nil {
		return nil, fmt.Errorf("jobs: query attempts %s: %w", jobID, err)
	}

	out := make([]core.Attempt, 0, len(rows))
	for _, row := range rows {
		rec, err := attemptOf(row)
		if err != nil {
			return nil, fmt.Errorf("jobs: scan attempt %s#%d: %w", row.JobID, row.Seq, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// PruneAttempts 把单个任务的账本裁到最近 keepLast 条（按 seq 保留最新的）。
// 主键 (job_id, seq) 前缀使这条 DELETE 走索引，代价与被删行数成正比；
// 表里没有该任务的行时子查询返回 NULL，COALESCE 成 0 后条件恒假，天然幂等。
func (s *Store) PruneAttempts(jobID string, keepLast int) error {
	if keepLast <= 0 {
		keepLast = core.DefaultAttemptsKeepLast
	}
	if _, err := s.h.NewDelete().Model((*attemptRow)(nil)).
		Where("job_id = ?", jobID).
		Where("seq <= (SELECT COALESCE(MAX(seq), 0) - ? FROM job_attempts WHERE job_id = ?)", keepLast, jobID).
		Exec(bgCtx); err != nil {
		return fmt.Errorf("jobs: prune attempts %s: %w", jobID, err)
	}
	return nil
}

// attemptRecord 把账本词汇折成一行；可空列按 Set 标记落 NULL。
func attemptRecord(rec core.Attempt) attemptRow {
	row := attemptRow{
		JobID:         rec.JobID,
		Seq:           int64(rec.Seq),
		Status:        rec.Status.String(),
		TriggerSource: rec.Trigger.String(),
		StartedAt:     rec.StartedAt.UnixNano(),
		DurationMs:    rec.DurationMs,
		Class:         nullableString(rec.Class),
		Signal:        nullableString(rec.Signal),
		ErrorText:     nullableString(rec.Error),
	}
	if !rec.FinishedAt.IsZero() {
		row.FinishedAt = sql.NullInt64{Int64: rec.FinishedAt.UnixNano(), Valid: true}
	}
	if rec.RetryableSet {
		retryable := int64(0)
		if rec.Retryable {
			retryable = 1
		}
		row.Retryable = sql.NullInt64{Int64: retryable, Valid: true}
	}
	if rec.ExitCodeSet {
		row.ExitCode = sql.NullInt64{Int64: int64(rec.ExitCode), Valid: true}
	}
	if rec.HTTPStatusSet {
		row.HTTPStatus = sql.NullInt64{Int64: int64(rec.HTTPStatus), Valid: true}
	}
	return row
}

// attemptOf 把一行还原成账本词汇。状态与触发来源是封闭词表，
// 库里出现未知取值按读取失败处理（与 trimTerminal 对未知任务状态的口径一致）——
// 静默猜一个状态等于账本说谎。
func attemptOf(row attemptRow) (core.Attempt, error) {
	status, ok := core.ParseAttemptStatus(row.Status)
	if !ok {
		return core.Attempt{}, fmt.Errorf("unknown attempt status %q", row.Status)
	}
	trigger, ok := core.ParseAttemptTrigger(row.TriggerSource)
	if !ok {
		return core.Attempt{}, fmt.Errorf("unknown attempt trigger %q", row.TriggerSource)
	}

	rec := core.Attempt{
		JobID:      row.JobID,
		Seq:        int(row.Seq),
		Status:     status,
		Trigger:    trigger,
		StartedAt:  time.Unix(0, row.StartedAt),
		DurationMs: row.DurationMs,
	}
	if row.FinishedAt.Valid {
		rec.FinishedAt = time.Unix(0, row.FinishedAt.Int64)
	}
	rec.Class = nullableStringValue(row.Class)
	rec.Signal = nullableStringValue(row.Signal)
	rec.Error = nullableStringValue(row.ErrorText)
	if row.Retryable.Valid {
		rec.Retryable, rec.RetryableSet = row.Retryable.Int64 != 0, true
	}
	if row.ExitCode.Valid {
		rec.ExitCode, rec.ExitCodeSet = int(row.ExitCode.Int64), true
	}
	if row.HTTPStatus.Valid {
		rec.HTTPStatus, rec.HTTPStatusSet = int(row.HTTPStatus.Int64), true
	}
	return rec, nil
}

// nullableString 非空串折成可空列值；空串是 NULL，不是 ""——
// "没有类别"与"类别为空串"在账本上必须是同一回事。
func nullableString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// nullableStringValue 可空列还原成串；NULL 与空串都读成 ""（与 nullableString 对称）。
func nullableStringValue(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}
