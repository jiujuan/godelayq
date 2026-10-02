package core

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var ErrJobNotFound = errors.New("job not found")

// DefaultFlushInterval 是合并落盘的周期：期间内的多次 Save/Update/Delete 只在
// 周期到来时写一次文件，避免每次变更都全量重写 JSON。
// 代价是崩溃时最多丢失一个周期的状态，需要更强保证的调用方可显式 Flush。
const DefaultFlushInterval = 200 * time.Millisecond

// DefaultHistoryLimit 是终态（成功/失败/取消）快照的默认保留条数。
const DefaultHistoryLimit = 1000

// Store 持久化接口
type Store interface {
	Save(job *Job) error
	Update(snapshot JobSnapshot) error
	Delete(jobID string) error
	// LoadAll 返回存储中的全部快照，包含已结束的终态记录；
	// 只想恢复未完成任务的调用方需自行按状态过滤。
	LoadAll() ([]JobSnapshot, error)
	// Flush 立即把内存状态写入存储（无变更时不产生写入）
	Flush() error
	// Close 停止后台合并写入并落盘当前状态，幂等
	Close() error
	// SetHistoryRetention 运行期替换终态留痕的条数与时长上限，下一次写入触发的 trim 用新值。
	// 两项必须一起给：分开调会出现"新条数配旧时长"的中间态。取值口径与 StoreOptions 同一条
	// （limit==0 用 DefaultHistoryLimit、limit<0 不留痕、ttl<=0 不按时间淘汰）。
	SetHistoryRetention(limit int, ttl time.Duration)
}

// StoreOptions 存储构造参数，零值即使用各项默认。
type StoreOptions struct {
	// Interval 合并落盘周期，<=0 使用 DefaultFlushInterval
	Interval time.Duration
	// HistoryLimit 终态快照保留条数：0 使用 DefaultHistoryLimit，
	// 负数表示不保留终态记录（写入即删除）
	HistoryLimit int
	// HistoryTTL 终态快照保留时长，<=0 表示不按时间淘汰
	HistoryTTL time.Duration
	// Logger 后台合并落盘失败时的日志器，nil 表示 slog.Default()
	Logger *slog.Logger
}

// JSONFileStore 基于JSON文件的存储，写入按 Interval 合并
type JSONFileStore struct {
	filePath string
	interval time.Duration

	// 终态留痕策略。写成原子值而不是普通字段：重载链会在运行期写它们（SetHistoryRetention），
	// 而读它们的是持有 s.mu 的写入协程，普通字段的读写并发会被 -race 抓住。
	// historyTTL 存纳秒，与 time.Duration 之间只在读写两处转换。
	historyLimit atomic.Int64
	historyTTL   atomic.Int64

	// logger 后台协程使用的日志器，构造后不再变更
	logger *slog.Logger

	mu       sync.RWMutex
	data     map[string]JobSnapshot // 内存缓存
	dirty    bool
	flushSeq int // 实际写盘次数，用于观测合并效果

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewJSONFileStore 用默认选项创建 JSON 存储。
func NewJSONFileStore(path string) (*JSONFileStore, error) {
	return NewJSONFileStoreWithOptions(path, StoreOptions{})
}

// NewJSONFileStoreWithOptions 按完整选项创建存储。
func NewJSONFileStoreWithOptions(path string, opts StoreOptions) (*JSONFileStore, error) {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	historyLimit, historyTTL := historyRetentionValues(opts.HistoryLimit, opts.HistoryTTL)

	s := &JSONFileStore{
		filePath: path,
		interval: interval,
		logger:   resolveLogger(opts.Logger),
		data:     make(map[string]JobSnapshot),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	// 原子字段不能进结构体字面量，所以建好体之后一次性 Store。
	s.historyLimit.Store(historyLimit)
	s.historyTTL.Store(historyTTL)

	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// 加载已有数据
	if err := s.loadFromDisk(); err != nil {
		return nil, err
	}

	go s.flushLoop()
	return s, nil
}

// historyRetentionValues 把留痕的条数与时长折成入库的两个取值（条数、纳秒）。
//
// 口径只有一份，构造与 SetHistoryRetention 共用：limit==0 回 DefaultHistoryLimit
// （0 不解释成"不限量"，那会让一次漏配把留痕推成无界增长）、limit<0 不留痕、
// ttl<=0 不按时间淘汰。setter 里再抄一遍这条判断，就出现了第二套规则。
func historyRetentionValues(limit int, ttl time.Duration) (int64, int64) {
	if limit == 0 {
		limit = DefaultHistoryLimit
	}
	return int64(limit), int64(ttl)
}

// SetHistoryRetention 运行期调整终态快照的保留条数与时长。
// 两项必须一起给：分开调会出现"新条数配旧时长"的中间态，而 history_limit=-1（不留痕）
// 与 history_ttl 的组合语义只在成对时说得清。取值口径与 StoreOptions 完全同一条
// （见 historyRetentionValues）。生效时机是下一次写入触发的 trim，这里不主动补一次清理。
func (s *JSONFileStore) SetHistoryRetention(limit int, ttl time.Duration) {
	historyLimit, historyTTL := historyRetentionValues(limit, ttl)
	s.historyLimit.Store(historyLimit)
	s.historyTTL.Store(historyTTL)
}

func (s *JSONFileStore) Save(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := job.ToSnapshot()
	s.data[snapshot.ID] = snapshot
	s.dirty = true
	s.trimAfterWriteLocked(snapshot)
	return nil
}

func (s *JSONFileStore) Update(snapshot JobSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[snapshot.ID] = snapshot
	s.dirty = true
	s.trimAfterWriteLocked(snapshot)
	return nil
}

// trimAfterWriteLocked 在写入终态快照后套用留痕策略
func (s *JSONFileStore) trimAfterWriteLocked(snapshot JobSnapshot) {
	if JobStatus(snapshot.Status).IsTerminal() {
		s.trimTerminalLocked()
	}
}

func (s *JSONFileStore) Delete(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, jobID)
	s.dirty = true
	return nil
}

// LoadAll 返回全部快照，含已结束的终态记录（受保留策略约束）。
func (s *JSONFileStore) LoadAll() ([]JobSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobs := make([]JobSnapshot, 0, len(s.data))
	for _, v := range s.data {
		jobs = append(jobs, v)
	}
	return jobs, nil
}

// trimTerminalLocked 按保留策略清理终态快照，Pending/Running 永不淘汰。
// 条数上限 < 0 表示不留痕；0 已在构造与 SetHistoryRetention 里替换为 DefaultHistoryLimit。
// 两个原子值在开头各读一次并留在局部变量里：调用方持 s.mu，所以两次 Load 不会读到半个值，
// 但同一次淘汰的"按几条切"与"按多久切"必须来自同一代策略——分两次 Load 时中间插进一次
// SetHistoryRetention，就会出现条数用新值、时长用旧值的那种没人配置过的组合。
func (s *JSONFileStore) trimTerminalLocked() {
	now := time.Now()
	limit := int(s.historyLimit.Load())
	ttl := time.Duration(s.historyTTL.Load())

	terminal := make([]JobSnapshot, 0, len(s.data))

	for id, snap := range s.data {
		if !JobStatus(snap.Status).IsTerminal() {
			continue
		}
		if limit < 0 || (ttl > 0 && now.Sub(snap.UpdatedAt) > ttl) {
			delete(s.data, id)
			continue
		}
		terminal = append(terminal, snap)
	}

	if limit >= 0 && len(terminal) > limit {
		sort.Slice(terminal, func(i, j int) bool {
			if !terminal[i].UpdatedAt.Equal(terminal[j].UpdatedAt) {
				return terminal[i].UpdatedAt.After(terminal[j].UpdatedAt)
			}
			// 同一时刻写入时按 ID 稳定排序，避免淘汰结果随遍历顺序漂移
			return terminal[i].ID < terminal[j].ID
		})
		for _, snap := range terminal[limit:] {
			delete(s.data, snap.ID)
		}
	}
}

// Flush 强制落盘；脏标记未置位时不写文件
func (s *JSONFileStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

// Close 停止后台协程并写入最终状态，可安全重复调用
func (s *JSONFileStore) Close() error {
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh
	return s.Flush()
}

// flushLoop 周期性合并落盘，退出前不再额外等待周期
func (s *JSONFileStore) flushLoop() {
	defer close(s.doneCh)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			if err := s.Flush(); err != nil {
				s.logger.Error("json store flush failed", "error", err)
			}
		}
	}
}

// flushLocked 执行实际写入，调用方须持有 s.mu
func (s *JSONFileStore) flushLocked() error {
	if !s.dirty {
		return nil
	}

	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}

	// 写入临时文件后重命名，保证原子性
	tmpFile := s.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}

	if err := os.Rename(tmpFile, s.filePath); err != nil {
		return err
	}

	s.dirty = false
	s.flushSeq++
	return nil
}

func (s *JSONFileStore) loadFromDisk() error {
	if _, err := os.Stat(s.filePath); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return nil
	}

	return json.Unmarshal(data, &s.data)
}
