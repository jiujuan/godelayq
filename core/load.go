package core

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// FileJobFormat 任务文件JSON格式
type FileJobFormat struct {
	// 基础字段
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"` // 使用RawMessage保持原始JSON

	// 时间设置（二选一）
	TriggerAt *time.Time `json:"trigger_at,omitempty"` // 绝对时间
	Delay     string     `json:"delay,omitempty"`      // 相对延迟，如 "10m", "1h30m"

	// Cron重复任务
	CronExpr string `json:"cron_expr,omitempty"`
	IsRepeat bool   `json:"is_repeat"`

	// 重试配置
	MaxRetries int    `json:"max_retries"`
	RetryDelay string `json:"retry_delay"` // 如 "30s", "5m"

	// 元数据
	Tags []string `json:"tags"`
	Desc string   `json:"description"`
}

// LoaderOptions 加载器配置选项
type LoaderOptions struct {
	// 目录路径
	Dir string

	// 文件匹配模式，默认 "*.json"
	Pattern string

	// 加载后处理策略
	PostLoadAction PostLoadAction

	// 归档目录（当PostLoadAction为Archive时使用）
	ArchiveDir string

	// 是否递归扫描子目录
	Recursive bool

	// 是否启用实时监控
	EnableWatcher bool

	// 解析失败文件的处理目录
	ErrorDir string

	// 任务名到Handler的映射（用于自动绑定）
	HandlerMap map[string]Handler
}

// PostLoadAction 加载后动作
type PostLoadAction int

const (
	// DeleteAfterLoad 加载后删除源文件
	DeleteAfterLoad PostLoadAction = iota
	// ArchiveAfterLoad 移动到归档目录
	ArchiveAfterLoad
	// KeepAfterLoad 保留原文件（记录已处理避免重复）
	KeepAfterLoad
)

// DirectoryLoader 目录任务加载器
type DirectoryLoader struct {
	scheduler *Scheduler
	options   LoaderOptions
	watcher   *fsnotify.Watcher
	mu        sync.RWMutex
	// 记录已处理的文件（避免重复加载，当使用KeepAfterLoad时）
	processedFiles map[string]time.Time
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// NewDirectoryLoader 创建加载器
func NewDirectoryLoader(scheduler *Scheduler, options LoaderOptions) (*DirectoryLoader, error) {
	if options.Pattern == "" {
		options.Pattern = "*.json"
	}
	if options.Dir == "" {
		options.Dir = "./jobs"
	}

	// 确保目录存在
	if err := os.MkdirAll(options.Dir, 0755); err != nil {
		return nil, fmt.Errorf("create jobs dir failed: %w", err)
	}

	loader := &DirectoryLoader{
		scheduler:      scheduler,
		options:        options,
		processedFiles: make(map[string]time.Time),
		stopCh:         make(chan struct{}),
	}

	// 如果启用归档，确保归档目录存在
	if options.PostLoadAction == ArchiveAfterLoad && options.ArchiveDir != "" {
		if err := os.MkdirAll(options.ArchiveDir, 0755); err != nil {
			return nil, fmt.Errorf("create archive dir failed: %w", err)
		}
	}

	return loader, nil
}

// Start 启动加载器（扫描现有文件+启动监控）
func (l *DirectoryLoader) Start() error {
	// 1. 先扫描并加载已有文件
	if err := l.ScanAndLoad(); err != nil {
		return fmt.Errorf("initial scan failed: %w", err)
	}

	// 2. 启动实时监控（如果启用）
	if l.options.EnableWatcher {
		if err := l.startWatcher(); err != nil {
			return fmt.Errorf("start watcher failed: %w", err)
		}
	}

	return nil
}

// Stop 停止加载器
func (l *DirectoryLoader) Stop() {
	close(l.stopCh)
	if l.watcher != nil {
		l.watcher.Close()
	}
	l.wg.Wait()
}

// ScanAndLoad 扫描目录并加载所有匹配的任务文件
func (l *DirectoryLoader) ScanAndLoad() error {
	var files []string

	// 根据是否递归选择遍历方式
	walkFunc := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if !l.options.Recursive && path != l.options.Dir {
				return filepath.SkipDir
			}
			return nil
		}

		// 检查文件匹配
		matched, err := filepath.Match(l.options.Pattern, info.Name())
		if err != nil {
			return err
		}
		if matched {
			files = append(files, path)
		}
		return nil
	}

	if err := filepath.Walk(l.options.Dir, walkFunc); err != nil {
		return err
	}

	// 加载每个文件
	for _, f := range files {
		if err := l.LoadFile(f); err != nil {
			log.Printf("Failed to load job file %s: %v", f, err)
			l.handleErrorFile(f, err)
		}
	}

	return nil
}

// LoadFile 加载单个任务文件
func (l *DirectoryLoader) LoadFile(filePath string) error {
	// 检查是否已处理（Keep模式）
	if l.options.PostLoadAction == KeepAfterLoad {
		l.mu.RLock()
		if _, processed := l.processedFiles[filePath]; processed {
			l.mu.RUnlock()
			return nil
		}
		l.mu.RUnlock()
	}

	// 读取文件
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file failed: %w", err)
	}

	// 解析JSON
	var format FileJobFormat
	if err := json.Unmarshal(data, &format); err != nil {
		return fmt.Errorf("parse json failed: %w", err)
	}

	// 转换为Job
	job, err := l.formatToJob(&format)
	if err != nil {
		return fmt.Errorf("convert to job failed: %w", err)
	}

	// 绑定Handler（如果提供了映射）
	if l.options.HandlerMap != nil {
		if h, ok := l.options.HandlerMap[format.Name]; ok {
			job.Handler = h
		}
	}

	// 添加到调度器
	if err := l.scheduler.Schedule(job); err != nil {
		return fmt.Errorf("schedule job failed: %w", err)
	}

	log.Printf("Loaded job from %s: ID=%s, Name=%s, TriggerAt=%v",
		filePath, job.ID, job.Name, job.TriggerAt)

	// 后处理
	return l.postProcess(filePath)
}

// formatToJob 将文件格式转换为Job对象
func (l *DirectoryLoader) formatToJob(f *FileJobFormat) (*Job, error) {
	job := &Job{
		ID:         f.ID,
		Name:       f.Name,
		Payload:    []byte(f.Payload),
		CronExpr:   f.CronExpr,
		IsRepeat:   f.IsRepeat,
		MaxRetries: f.MaxRetries,
		CreatedAt:  time.Now(),
		Status:     StatusPending,
	}

	// 生成ID（如果未指定）
	if job.ID == "" {
		job.ID = fmt.Sprintf("file_%d", time.Now().UnixNano())
	}

	// 处理触发时间（绝对时间优先）
	if f.TriggerAt != nil {
		job.TriggerAt = *f.TriggerAt
	} else if f.Delay != "" {
		// 解析延迟字符串
		delay, err := time.ParseDuration(f.Delay)
		if err != nil {
			return nil, fmt.Errorf("invalid delay format: %w", err)
		}
		job.TriggerAt = time.Now().Add(delay)
	} else {
		// 默认立即执行（1秒后）
		job.TriggerAt = time.Now().Add(1 * time.Second)
	}

	// 解析重试延迟
	if f.RetryDelay != "" {
		rd, err := time.ParseDuration(f.RetryDelay)
		if err != nil {
			return nil, fmt.Errorf("invalid retry_delay format: %w", err)
		}
		job.RetryDelay = rd
	} else {
		job.RetryDelay = 1 * time.Minute // 默认
	}

	// 默认重试次数
	if job.MaxRetries == 0 {
		job.MaxRetries = 3
	}

	return job, nil
}

// postProcess 加载后的文件处理
func (l *DirectoryLoader) postProcess(filePath string) error {
	switch l.options.PostLoadAction {
	case DeleteAfterLoad:
		if err := os.Remove(filePath); err != nil {
			return fmt.Errorf("delete file failed: %w", err)
		}
		log.Printf("Deleted processed file: %s", filePath)

	case ArchiveAfterLoad:
		if l.options.ArchiveDir == "" {
			return fmt.Errorf("archive dir not set")
		}
		filename := filepath.Base(filePath)
		timestamp := time.Now().Format("20060102_150405")
		newName := fmt.Sprintf("%s_%s", timestamp, filename)
		dest := filepath.Join(l.options.ArchiveDir, newName)

		if err := os.Rename(filePath, dest); err != nil {
			return fmt.Errorf("archive file failed: %w", err)
		}
		log.Printf("Archived file to: %s", dest)

	case KeepAfterLoad:
		l.mu.Lock()
		l.processedFiles[filePath] = time.Now()
		l.mu.Unlock()
	}

	return nil
}

// handleErrorFile 处理解析失败的文件
func (l *DirectoryLoader) handleErrorFile(filePath string, loadErr error) {
	if l.options.ErrorDir == "" {
		return
	}

	// 确保错误目录存在
	os.MkdirAll(l.options.ErrorDir, 0755)

	filename := filepath.Base(filePath)
	errorFile := filepath.Join(l.options.ErrorDir, filename+".error")

	// 复制原文件并附加错误信息
	src, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer src.Close()

	dst, err := os.Create(errorFile)
	if err != nil {
		return
	}
	defer dst.Close()

	io.Copy(dst, src)
	dst.WriteString(fmt.Sprintf("\n\n// ERROR: %s\n", loadErr.Error()))

	// 可选：删除原文件或保留，根据配置
	if l.options.PostLoadAction == DeleteAfterLoad {
		os.Remove(filePath)
	}
}

// startWatcher 启动文件系统监控
func (l *DirectoryLoader) startWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	l.watcher = watcher

	// 添加监控目录
	if err := watcher.Add(l.options.Dir); err != nil {
		return err
	}

	// 递归添加子目录（如果启用）
	if l.options.Recursive {
		filepath.Walk(l.options.Dir, func(path string, info os.FileInfo, err error) error {
			if info != nil && info.IsDir() {
				watcher.Add(path)
			}
			return nil
		})
	}

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for {
			select {
			case <-l.stopCh:
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// 只处理创建和写入事件
				if event.Op&fsnotify.Create == fsnotify.Create ||
					event.Op&fsnotify.Write == fsnotify.Write {
					// 检查文件匹配
					if matched, _ := filepath.Match(l.options.Pattern, filepath.Base(event.Name)); matched {
						// 延迟一点加载，避免文件写入不完整
						time.Sleep(100 * time.Millisecond)
						if err := l.LoadFile(event.Name); err != nil {
							log.Printf("Failed to load new file %s: %v", event.Name, err)
						}
					}
					// 如果是新目录且递归模式，添加监控
					if l.options.Recursive {
						if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
							watcher.Add(event.Name)
						}
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("Watcher error: %v", err)
			}
		}
	}()

	log.Printf("Started watching directory: %s", l.options.Dir)
	return nil
}

// BulkLoadFromReader 从io.Reader批量加载（支持从网络或标准输入读取）
func (l *DirectoryLoader) BulkLoadFromReader(r io.Reader) error {
	decoder := json.NewDecoder(r)

	// 支持两种格式：
	// 1. 单行JSON对象
	// 2. JSON数组 [obj1, obj2, ...]

	// 先尝试数组格式
	var formats []FileJobFormat
	if err := decoder.Decode(&formats); err == nil {
		for _, f := range formats {
			job, err := l.formatToJob(&f)
			if err != nil {
				log.Printf("Invalid job format: %v", err)
				continue
			}
			if l.options.HandlerMap != nil {
				if h, ok := l.options.HandlerMap[f.Name]; ok {
					job.Handler = h
				}
			}
			if err := l.scheduler.Schedule(job); err != nil {
				log.Printf("Failed to schedule job: %v", err)
			}
		}
		return nil
	}

	// 重置decoder尝试单行JSON流
	// 这里简化处理，实际可能需要更复杂的逻辑
	return fmt.Errorf("bulk load format not supported")
}
