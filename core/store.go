package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var ErrJobNotFound = errors.New("job not found")

// Store 持久化接口
type Store interface {
	Save(job *Job) error
	Update(snapshot JobSnapshot) error
	Delete(jobID string) error
	LoadAll() ([]JobSnapshot, error)
}

// JSONFileStore 基于JSON文件的存储
type JSONFileStore struct {
	filePath string
	mu       sync.RWMutex
	data     map[string]JobSnapshot // 内存缓存
	dirty    bool
}

func NewJSONFileStore(path string) (*JSONFileStore, error) {
	s := &JSONFileStore{
		filePath: path,
		data:     make(map[string]JobSnapshot),
	}
	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// 加载已有数据
	if err := s.loadFromDisk(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *JSONFileStore) Save(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[job.ID] = job.ToSnapshot()
	s.dirty = true
	return s.flush()
}

func (s *JSONFileStore) Update(snapshot JobSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[snapshot.ID] = snapshot
	s.dirty = true
	return s.flush()
}

func (s *JSONFileStore) Delete(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, jobID)
	s.dirty = true
	return s.flush()
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

func (s *JSONFileStore) flush() error {
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
	return nil
}
