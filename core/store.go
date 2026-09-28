package core

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrJobNotFound = errors.New("job not found")

// DefaultFlushInterval 是合并落盘的周期：期间内的多次 Save/Update/Delete 只在
// 周期到来时写一次文件，避免每次变更都全量重写 JSON。
// 代价是崩溃时最多丢失一个周期的状态，需要更强保证的调用方可显式 Flush。
const DefaultFlushInterval = 200 * time.Millisecond

// Store 持久化接口
type Store interface {
	Save(job *Job) error
	Update(snapshot JobSnapshot) error
	Delete(jobID string) error
	LoadAll() ([]JobSnapshot, error)
	// Flush 立即把内存状态写入存储（无变更时不产生写入）
	Flush() error
	// Close 停止后台合并写入并落盘当前状态，幂等
	Close() error
}

// JSONFileStore 基于JSON文件的存储，写入按 DefaultFlushInterval 合并
type JSONFileStore struct {
	filePath string
	interval time.Duration
	mu       sync.RWMutex
	data     map[string]JobSnapshot // 内存缓存
	dirty    bool
	flushSeq int // 实际写盘次数，用于观测合并效果

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func NewJSONFileStore(path string) (*JSONFileStore, error) {
	return NewJSONFileStoreWithInterval(path, DefaultFlushInterval)
}

// NewJSONFileStoreWithInterval 自定义合并落盘周期；非正数回退到默认值。
func NewJSONFileStoreWithInterval(path string, interval time.Duration) (*JSONFileStore, error) {
	if interval <= 0 {
		interval = DefaultFlushInterval
	}

	s := &JSONFileStore{
		filePath: path,
		interval: interval,
		data:     make(map[string]JobSnapshot),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
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
	s.data[job.ID] = job.ToSnapshot()
	s.dirty = true
	return nil
}

func (s *JSONFileStore) Update(snapshot JobSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[snapshot.ID] = snapshot
	s.dirty = true
	return nil
}

func (s *JSONFileStore) Delete(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, jobID)
	s.dirty = true
	return nil
}

func (s *JSONFileStore) LoadAll() ([]JobSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobs := make([]JobSnapshot, 0, len(s.data))
	for _, v := range s.data {
		// 只加载待处理的任务
		if v.Status == int(StatusPending) || v.Status == int(StatusRunning) {
			jobs = append(jobs, v)
		}
	}
	return jobs, nil
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
