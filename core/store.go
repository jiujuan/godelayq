package core

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
}

// JSONFileStore 基于JSON文件的存储，写入按 Interval 合并
type JSONFileStore struct {
	filePath string
	interval time.Duration

	// 终态留痕策略
	historyLimit int
	historyTTL   time.Duration

	mu       sync.RWMutex
	data     map[string]JobSnapshot // 内存缓存
	dirty    bool
	flushSeq int // 实际写盘次数，用于观测合并效果

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func NewJSONFileStore(path string) (*JSONFileStore, error) {
	return NewJSONFileStoreWithOptions(path, StoreOptions{})
}

// NewJSONFileStoreWithInterval 自定义合并落盘周期；非正数回退到默认值。
func NewJSONFileStoreWithInterval(path string, interval time.Duration) (*JSONFileStore, error) {
	return NewJSONFileStoreWithOptions(path, StoreOptions{Interval: interval})
}

// NewJSONFileStoreWithOptions 按完整选项创建存储。
func NewJSONFileStoreWithOptions(path string, opts StoreOptions) (*JSONFileStore, error) {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	historyLimit := opts.HistoryLimit
	if historyLimit == 0 {
		historyLimit = DefaultHistoryLimit
	}

	s := &JSONFileStore{
		filePath:     path,
		interval:     interval,
		historyLimit: historyLimit,
		historyTTL:   opts.HistoryTTL,
		data:         make(map[string]JobSnapshot),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}

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
// historyLimit < 0 表示不留痕；0 已在构造时替换为 DefaultHistoryLimit。
func (s *JSONFileStore) trimTerminalLocked() {
	now := time.Now()
	terminal := make([]JobSnapshot, 0, len(s.data))

	for id, snap := range s.data {
		if !JobStatus(snap.Status).IsTerminal() {
			continue
		}
		if s.historyLimit < 0 || (s.historyTTL > 0 && now.Sub(snap.UpdatedAt) > s.historyTTL) {
			delete(s.data, id)
			continue
		}
		terminal = append(terminal, snap)
	}

	if s.historyLimit >= 0 && len(terminal) > s.historyLimit {
		sort.Slice(terminal, func(i, j int) bool {
			if !terminal[i].UpdatedAt.Equal(terminal[j].UpdatedAt) {
				return terminal[i].UpdatedAt.After(terminal[j].UpdatedAt)
			}
			// 同一时刻写入时按 ID 稳定排序，避免淘汰结果随遍历顺序漂移
			return terminal[i].ID < terminal[j].ID
		})
		for _, snap := range terminal[s.historyLimit:] {
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
				log.Printf("JSON store flush failed: %v", err)
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
