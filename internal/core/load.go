package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// execJobRejectReason 是加载器不接受执行器任务文件时写给日志与错误副本的原因（TASK-E17）。
const execJobRejectReason = "executor jobs are not accepted from the loader"

// FileJobFormat 任务文件JSON格式
type FileJobFormat struct {
	// 基础字段
	ID      string          `json:"id"`
	Name    string          `json:"name"`    // 必填，同时作为 Handler 查找键
	Payload json.RawMessage `json:"payload"` // 使用RawMessage保持原始JSON

	// 时间设置（二选一）
	TriggerAt *time.Time `json:"trigger_at,omitempty"` // 绝对时间
	Delay     string     `json:"delay,omitempty"`      // 相对延迟，如 "10m", "1h30m"

	// Cron重复任务
	CronExpr string `json:"cron_expr,omitempty"`
	IsRepeat bool   `json:"is_repeat"`

	// 单次执行超时，如 "30s"；为空表示不限制
	Timeout string `json:"timeout,omitempty"`

	// 重试配置。MaxRetries 用指针区分"未写"与"写 0"：
	// 未写取 DefaultLoaderMaxRetries，写 0 表示不重试。
	// 注意与 POST /jobs 不同：请求体里的 max_retries 是普通整数，省略即为 0。
	MaxRetries *int   `json:"max_retries,omitempty"`
	RetryDelay string `json:"retry_delay"` // 如 "30s", "5m"
}

// DefaultLoaderMaxRetries 是任务文件未声明 max_retries 时的重试次数。
const DefaultLoaderMaxRetries = 3

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

	// AllowExecJobs 允许加载 exec. 前缀的任务文件，默认 false。
	//
	// 默认关的理由（TASK-E17）：任务文件走这条路径时没有任何凭据，"能往这个目录写文件"
	// 就等于"能在本机执行档位声明的命令"。要把执行器任务交给目录投递，得由程序显式打开。
	// 服务端二进制当前不启用目录加载器（docs/design/executor-design.md §6.9），所以这个字段
	// 约束的是自行把 Scheduler 与 DirectoryLoader 接起来的程序；配置项 executors.loader_allow
	// 的取值由 executor.Registry.LoaderAllowed() 读出，调用方可以把它原样接到这里。
	AllowExecJobs bool

	// Logger 加载过程的日志器，nil 表示 slog.Default()
	Logger *slog.Logger
}

// PostLoadAction 加载后动作
type PostLoadAction int

// loaderDebounceInterval 是监控模式下同一文件的静默窗口：
// 写入往往被拆成 Create + 多次 Write，逐次加载会读到半截 JSON，
// 也会让事件循环串行等待。窗口内的事件合并成一次加载。
const loaderDebounceInterval = 100 * time.Millisecond

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
	// logger 构造后不再变更
	logger  *slog.Logger
	watcher *fsnotify.Watcher
	mu      sync.RWMutex
	// 记录已处理的文件（避免重复加载，当使用KeepAfterLoad时）
	processedFiles map[string]time.Time
	// pendingLoads 按路径合并短时间内的重复写入事件，受 mu 保护
	pendingLoads map[string]*time.Timer
	stopCh       chan struct{}
	wg           sync.WaitGroup
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
		logger:         resolveLogger(options.Logger),
		processedFiles: make(map[string]time.Time),
		pendingLoads:   make(map[string]*time.Timer),
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

// Stop 停止加载器：取消未触发的 debounce 加载，关闭监控与读写协程
func (l *DirectoryLoader) Stop() {
	close(l.stopCh)
	if l.watcher != nil {
		if err := l.watcher.Close(); err != nil {
			l.logger.Error("failed to close fs watcher", "error", err)
		}
	}

	l.mu.Lock()
	for path, timer := range l.pendingLoads {
		timer.Stop()
		delete(l.pendingLoads, path)
	}
	l.mu.Unlock()

	l.wg.Wait()
}

// scheduleLoad 在静默窗口后加载该文件；窗口内的重复事件只保留一次加载。
func (l *DirectoryLoader) scheduleLoad(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if timer, ok := l.pendingLoads[path]; ok {
		timer.Reset(loaderDebounceInterval)
		return
	}

	l.pendingLoads[path] = time.AfterFunc(loaderDebounceInterval, func() {
		l.forgetPending(path)

		select {
		case <-l.stopCh:
			return // 已关停，不再起新加载
		default:
		}

		if err := l.LoadFile(path); err != nil {
			l.logger.Error("failed to load new file", "path", path, "error", err)
		}
	})
}

// forgetPending 在定时器触发后摘掉该路径的记录，允许下一轮写入重新排期
func (l *DirectoryLoader) forgetPending(path string) {
	l.mu.Lock()
	delete(l.pendingLoads, path)
	l.mu.Unlock()
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
			l.logger.Error("failed to load job file", "path", f, "error", err)
			if herr := l.handleErrorFile(f, err); herr != nil {
				l.logger.Error("failed to stash invalid job file", "path", f, "error", herr)
			}
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

	// 执行器任务默认不从目录接受（TASK-E17 §3.2）：判在绑定 Handler 与入队之前，
	// 那时还没有任何东西被执行。
	if !l.options.AllowExecJobs && IsExecHandlerKey(format.Name) {
		return l.rejectExecJob(filePath, format.Name)
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

	l.logger.Debug("loaded job from file",
		"path", filePath, "job_id", job.ID, "job_name", job.Name, "trigger_at", job.TriggerAt)

	// 后处理
	return l.postProcess(filePath)
}

// rejectExecJob 是加载器对 exec. 前缀任务文件的拒绝路径（TASK-E17 §3.2）。
//
// 返回 nil 而不是 error：调用方（ScanAndLoad 与监控回调）会把 error 记成 error 级并再抄一份
// 错误副本，而"这类文件我们不从目录接受"是有意的拒绝，不是故障。日志只留一行 warn。
//
// 两种情况都不把文件留在原地反复扫描：
//   - 配了 ErrorDir：文件复制进错误目录（带一句原因）。删除策略下 handleErrorFile 会顺手
//     删掉源文件，这时就不再走 postProcess，否则会对同一个不存在的文件删第二次。
//   - 没配 ErrorDir：其余失败要报出来，但源文件仍按 PostLoadAction 处理
//     （Keep 模式下记成"已处理"，下一轮不会重复拒绝同一个文件）。
func (l *DirectoryLoader) rejectExecJob(filePath, name string) error {
	l.logger.Warn("executor job file rejected by the loader",
		"path", filePath, "job_name", name, "reason", execJobRejectReason,
		"hint", "set LoaderOptions.AllowExecJobs only when writing into this directory is meant to grant execution")

	if l.options.ErrorDir == "" {
		return l.postProcess(filePath)
	}

	if err := l.handleErrorFile(filePath, errors.New(execJobRejectReason)); err != nil {
		l.logger.Error("failed to stash the rejected job file", "path", filePath, "error", err)
		return l.postProcess(filePath)
	}
	if l.options.PostLoadAction == DeleteAfterLoad {
		return nil
	}
	return l.postProcess(filePath)
}

// formatToJob 将文件格式转换为Job对象
func (l *DirectoryLoader) formatToJob(f *FileJobFormat) (*Job, error) {
	// name 既是任务标识也是 Handler 查找键：留空的任务注定无 handler，直接在入口拒掉
	if strings.TrimSpace(f.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}

	job := &Job{
		ID:        f.ID,
		Name:      f.Name,
		Payload:   []byte(f.Payload),
		CronExpr:  f.CronExpr,
		IsRepeat:  f.IsRepeat,
		CreatedAt: time.Now(),
		Status:    StatusPending,
	}

	// 重试次数：未写用默认，显式 0 表示不重试
	maxRetries := DefaultLoaderMaxRetries
	if f.MaxRetries != nil {
		if *f.MaxRetries < 0 {
			return nil, fmt.Errorf("max_retries must not be negative, got %d", *f.MaxRetries)
		}
		maxRetries = *f.MaxRetries
	}
	job.MaxRetries = maxRetries

	// 生成ID（如果未指定）。不能用 UnixNano 直接拼：时钟粒度会让同批文件得到相同 ID，
	// 后一个任务会覆盖前一个。
	if job.ID == "" {
		job.ID = generateID()
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

	// 解析执行超时
	if f.Timeout != "" {
		td, err := time.ParseDuration(f.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout format: %w", err)
		}
		job.Timeout = td
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
		l.logger.Debug("deleted processed file", "path", filePath)

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
		l.logger.Debug("archived processed file", "path", dest)

	case KeepAfterLoad:
		l.mu.Lock()
		l.processedFiles[filePath] = time.Now()
		l.mu.Unlock()
	}

	return nil
}

// handleErrorFile 把解析失败的文件复制到 ErrorDir 并附错误说明。
// 未配置 ErrorDir 时什么都不做（原始错误已由调用方记录），其余失败一律返回错误。
func (l *DirectoryLoader) handleErrorFile(filePath string, loadErr error) error {
	if l.options.ErrorDir == "" {
		return nil
	}

	if err := os.MkdirAll(l.options.ErrorDir, 0755); err != nil {
		return fmt.Errorf("create error dir %s failed: %w", l.options.ErrorDir, err)
	}

	filename := filepath.Base(filePath)
	errorFile := filepath.Join(l.options.ErrorDir, filename+".error")

	src, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open failed file %s: %w", filePath, err)
	}
	defer src.Close()

	dst, err := os.Create(errorFile)
	if err != nil {
		return fmt.Errorf("create error file %s failed: %w", errorFile, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy failed file to %s: %w", errorFile, err)
	}
	if _, err := fmt.Fprintf(dst, "\n\n// ERROR: %s\n", loadErr.Error()); err != nil {
		return fmt.Errorf("append error note to %s: %w", errorFile, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close error file %s: %w", errorFile, err)
	}

	// 失败文件已归档，按后处理策略决定原文件去留
	if l.options.PostLoadAction == DeleteAfterLoad {
		if err := os.Remove(filePath); err != nil {
			return fmt.Errorf("delete failed file %s: %w", filePath, err)
		}
	}

	return nil
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

	// 递归添加子目录（如果启用）：单个目录加不上就记日志跳过，
	// 不能因为一次遍历失败而丢掉整个监控
	if l.options.Recursive {
		if err := filepath.Walk(l.options.Dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				l.logger.Warn("failed to scan directory for watcher", "path", path, "error", err)
				return nil
			}
			if info != nil && info.IsDir() && path != l.options.Dir {
				if err := watcher.Add(path); err != nil {
					l.logger.Error("failed to watch subdirectory", "path", path, "error", err)
				}
			}
			return nil
		}); err != nil {
			l.logger.Error("failed to walk jobs dir", "dir", l.options.Dir, "error", err)
		}
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
						// 等静默窗口过去再读，既不阻塞事件循环也避免读到半截文件
						l.scheduleLoad(event.Name)
					}
					// 如果是新目录且递归模式，添加监控。stat 失败多为目标已被删除，跳过即可
					if l.options.Recursive {
						if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
							if err := watcher.Add(event.Name); err != nil {
								l.logger.Error("failed to watch new directory", "path", event.Name, "error", err)
							}
						}
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				l.logger.Error("watcher error", "error", err)
			}
		}
	}()

	l.logger.Info("started watching directory", "dir", l.options.Dir)
	return nil
}
