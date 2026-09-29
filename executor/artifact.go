package executor

import (
	"context"
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
)

// ArtifactStore 是执行输出的文件存储：一次执行的完整 stdout / stderr 加一份 meta.json。
//
// 它存在的原因是任务快照装不下输出：core 的 JSON 存储每次合并落盘都要重写整个 jobs.json，
// 把几十 KB 的输出挂在快照上会让文件体积与写放大一起失控
// （依据见 docs/design/executor-design.md §6.4）。摘要留在快照里（core.ExecMeta），
// 正文留在这里，两边靠 job_id + attempt 对应。
//
// 与 core.Store 是松耦合的：这里不认识任务，任务侧也不同步删除产物。
// 取消与终态淘汰留下的目录由启动时的孤儿清理回收（取舍写在 PurgeOrphans）。
type ArtifactStore struct {
	dir      string
	maxBytes int
	ttl      time.Duration
	now      func() time.Time
	logger   *slog.Logger

	// purgeOnce 让"孤儿清理只在启动时跑一次"这个决定在代码里可见：
	// 反复跑会把 store.history_limit=-1（不留痕）的部署里刚跑完的任务产物当孤儿删掉。
	purgeOnce sync.Once
}

// ArtifactOptions 是构造产物存储需要的取值，对应 executors.output 一节。
type ArtifactOptions struct {
	// Dir 是产物根目录。
	Dir string
	// MaxBytes 是单条流（stdout 或 stderr）的落盘上限，达到即停止写入并标记截断。
	// 低于 minArtifactMaxBytes 的取值会被抬到该下限：配成 0 会让所有输出丢失，比配小更糟。
	MaxBytes int
	// TTL 是保留时长，0 表示不按时间清理（只做启动时的孤儿清理）。
	TTL time.Duration
	// Now 是时间源，nil 时用 time.Now。测试靠它注入时间，不需要 sleep 等待过期。
	Now func() time.Time
}

// minArtifactMaxBytes 是单条流的下限。
const minArtifactMaxBytes = 1024

// 产物目录与文件的权限：输出里可能出现参数带出来的业务数据，不放给同机其它用户读。
const (
	artifactDirPerm  os.FileMode = 0o750
	artifactFilePerm os.FileMode = 0o640
)

// artifactStreamSuffix 把对外的 stream 名限定成两个字面值。
// 读接口用它拼路径，取值一放开就等于给了"读任意文件名"。
var artifactStreamSuffix = map[string]string{
	"out": ".out",
	"err": ".err",
}

// ErrArtifactMissing 表示某次尝试的产物文件不存在（从未写入，或已被清理）。
// 接口层据此回 404，而不是把 os.ErrNotExist 的原始文本透出去。
var ErrArtifactMissing = errors.New("artifact not found")

// NewArtifactStore 建好根目录并返回存储；建不起来就返回错误——
// 输出没地方放的执行器不如不启动，否则每次执行的结果都会丢在半路上。
func NewArtifactStore(opts ArtifactOptions, logger *slog.Logger) (*ArtifactStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("artifact store: output dir must not be empty")
	}
	if opts.TTL < 0 {
		return nil, fmt.Errorf("artifact store: negative ttl %v", opts.TTL)
	}
	if opts.MaxBytes < minArtifactMaxBytes {
		// 抬到下限而不是报错：配置层的下限由 ExecutorsConfig.Validate 负责，
		// 这里挡的是绕过配置直接构造的调用方（测试与自行接入的程序）。
		logger.Warn("artifact max_bytes raised to minimum",
			"configured", opts.MaxBytes, "effective", minArtifactMaxBytes)
		opts.MaxBytes = minArtifactMaxBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	absolute, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("artifact store: resolve dir: %w", err)
	}
	if err := os.MkdirAll(absolute, artifactDirPerm); err != nil {
		return nil, fmt.Errorf("artifact store: create dir: %w", err)
	}

	return &ArtifactStore{
		dir:      absolute,
		maxBytes: opts.MaxBytes,
		ttl:      opts.TTL,
		now:      opts.Now,
		logger:   logger,
	}, nil
}

// Dir 返回产物根目录的绝对路径。
func (a *ArtifactStore) Dir() string { return a.dir }

// MaxBytes 返回实际生效的单流上限（已抬到下限）。
func (a *ArtifactStore) MaxBytes() int { return a.maxBytes }

// TTL 返回保留时长，0 表示不按时间清理。
func (a *ArtifactStore) TTL() time.Duration { return a.ttl }

// checkArtifactJobID 拒绝任何可能被当成路径写法的任务 ID。
//
// 现实里的 ID 是 UUIDv7 文本形式，只含小写十六进制与连字符；但目录名来自调用方
// （自行接入的程序、测试、以及未来任何写入路径），所以这里不依赖"上游一定安全"：
// 分隔符、点号（含 `..`）、绝对路径前缀与任何非 ASCII 都挡掉。
func checkArtifactJobID(jobID string) error {
	if jobID == "" {
		return errors.New("artifact: job id is empty")
	}
	if len(jobID) > 128 {
		return fmt.Errorf("artifact: job id too long (%d characters)", len(jobID))
	}
	for _, r := range jobID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			// 允许字母、数字与连字符，其余（`.` `/` `\` `:` 等）一律拒绝
		default:
			return fmt.Errorf("artifact: job id %q contains an unusable character %q", jobID, r)
		}
	}
	return nil
}

// jobDir 校验 ID 并给出该任务的产物目录。
func (a *ArtifactStore) jobDir(jobID string) (string, error) {
	if err := checkArtifactJobID(jobID); err != nil {
		return "", err
	}
	return filepath.Join(a.dir, jobID), nil
}

// streamPath 把"目录 + attempt + 流名"拼成文件名。
func streamPath(dir string, attempt int, stream string) (string, error) {
	suffix, ok := artifactStreamSuffix[stream]
	if !ok {
		return "", fmt.Errorf("artifact: unknown stream %q (use out or err)", stream)
	}
	if attempt < 0 {
		return "", fmt.Errorf("artifact: attempt must not be negative, got %d", attempt)
	}
	return filepath.Join(dir, fmt.Sprintf("a%d%s", attempt, suffix)), nil
}

// Open 为一次执行建写入句柄：每个 attempt 一套文件，任务目录不存在时创建。
//
// 按 attempt 分文件是必须的：重试副本沿用同一个任务 ID（core 的 CloneForRetry），
// 不分文件的话第二次尝试会覆盖第一次的输出，而"第一次为什么失败"往往正是排障要看的。
func (a *ArtifactStore) Open(jobID string, attempt int) (*ArtifactWriter, error) {
	dir, err := a.jobDir(jobID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, artifactDirPerm); err != nil {
		return nil, fmt.Errorf("artifact: create job dir: %w", err)
	}

	writer := &ArtifactWriter{jobID: jobID, attempt: attempt, dir: dir}
	if writer.stdout, err = a.createStream(dir, jobID, attempt, "out"); err != nil {
		return nil, err
	}
	if writer.stderr, err = a.createStream(dir, jobID, attempt, "err"); err != nil {
		// 第一个文件已经建出来了，关掉它再返回，不留下一个开着句柄的半套产物
		_ = writer.stdout.close()
		return nil, err
	}
	return writer, nil
}

func (a *ArtifactStore) createStream(dir, jobID string, attempt int, stream string) (*limitedWriter, error) {
	path, err := streamPath(dir, attempt, stream)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, artifactFilePerm)
	if err != nil {
		return nil, fmt.Errorf("artifact: create %s file for job %s: %w", stream, jobID, err)
	}
	return &limitedWriter{file: file, path: path, maxBytes: a.maxBytes}, nil
}

// ArtifactWriter 是一次执行的写入侧。两个流各自计数、各自判定截断。
//
// 使用顺序固定：先把 Stdout()/Stderr() 挂给执行器，执行结束后 Close() 拿到落盘结论，
// 需要 meta.json 时再 WriteMeta()。
type ArtifactWriter struct {
	jobID   string
	attempt int
	dir     string

	stdout *limitedWriter
	stderr *limitedWriter

	mu     sync.Mutex
	closed bool
}

// Stdout / Stderr 返回带上限的输出流。上限的含义是"停止写入"，
// 不是"截断已写内容"，也不是"终止执行"。
func (w *ArtifactWriter) Stdout() io.Writer { return w.stdout }
func (w *ArtifactWriter) Stderr() io.Writer { return w.stderr }

// OutPath / ErrPath 返回两条流的落盘位置，读侧不需要自己拼路径。
func (w *ArtifactWriter) OutPath() string { return w.stdout.path }
func (w *ArtifactWriter) ErrPath() string { return w.stderr.path }

// Close 关闭两个文件并返回落盘结论。重复调用安全，返回同样的结论。
func (w *ArtifactWriter) Close() (ArtifactInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	info := w.info()
	if w.closed {
		return info, nil
	}
	w.closed = true

	err := w.stdout.close()
	if closeErr := w.stderr.close(); err == nil {
		err = closeErr
	}
	return info, err
}

func (w *ArtifactWriter) info() ArtifactInfo {
	return ArtifactInfo{
		JobID:     w.jobID,
		Attempt:   w.attempt,
		OutPath:   w.OutPath(),
		ErrPath:   w.ErrPath(),
		OutBytes:  w.stdout.written(),
		ErrBytes:  w.stderr.written(),
		Truncated: w.stdout.truncated() || w.stderr.truncated(),
	}
}

// ArtifactInfo 是一次执行产物的落盘结论，由 Close 返回给执行侧填进 core.ExecMeta。
type ArtifactInfo struct {
	JobID     string `json:"job_id"`
	Attempt   int    `json:"attempt"`
	OutPath   string `json:"out_path"`
	ErrPath   string `json:"err_path"`
	OutBytes  int64  `json:"out_bytes"`
	ErrBytes  int64  `json:"err_bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// metaFileName 是某次尝试的元数据文件名。
func metaFileName(attempt int) string { return fmt.Sprintf("a%d.meta.json", attempt) }

// WriteMeta 把执行结论写成 a<attempt>.meta.json。
//
// 先写临时文件再改名：读侧（TASK-E07）因此可以认为"存在的 meta.json 内容是完整的"，
// 不必处理读到半成品。文件名带 attempt，不同尝试互不覆盖。
// 重复调用是覆盖上一份，同样走临时文件。
func (w *ArtifactWriter) WriteMeta(meta any) error {
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("artifact: encode meta: %w", err)
	}
	payload = append(payload, '\n')

	name := metaFileName(w.attempt)
	tmp, err := os.CreateTemp(w.dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("artifact: create meta temp: %w", err)
	}
	tmpName := tmp.Name()
	// 失败路径必须留下干净的目录：改名成功后这个删除是空操作
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("artifact: write meta: %w", err)
	}
	if err := tmp.Chmod(artifactFilePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("artifact: set meta file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("artifact: close meta: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(w.dir, name)); err != nil {
		return fmt.Errorf("artifact: publish meta: %w", err)
	}
	return nil
}

// limitedWriter 是带上限的文件写入器。
type limitedWriter struct {
	file     *os.File
	path     string
	maxBytes int

	// accepted 是已经写入文件的字节数（不含被丢弃的部分）。
	accepted int64
	// dropped 是被上限挡掉的字节数，用来解释"文件比实际输出短"。
	dropped   int64
	wasClosed bool
	closeErr  error
}

// Write 实现 io.Writer。
//
// 关键契约：超过上限后照旧返回 (len(b), nil)。
// 上限的含义是"停止写入"而不是"停止接收"——io.Copy（exec.Cmd 的 Stdout 管道复制走的就是它）
// 把"返回的 n 小于 len(b) 且没有错误"当成 io.ErrShortWrite 直接放弃复制，os/exec 还会因此
// 中断子进程。我们要的恰恰是只截断输出、不打断执行，所以满额返回并把差额记进 dropped。
func (w *limitedWriter) Write(b []byte) (int, error) {
	remaining := int64(w.maxBytes) - w.accepted
	if remaining <= 0 {
		w.dropped += int64(len(b))
		return len(b), nil
	}
	if int64(len(b)) > remaining {
		n, err := w.file.Write(b[:remaining])
		w.accepted += int64(n)
		w.dropped += int64(len(b)) - int64(n)
		return len(b), err
	}
	n, err := w.file.Write(b)
	w.accepted += int64(n)
	return n, err
}

func (w *limitedWriter) written() int64  { return w.accepted }
func (w *limitedWriter) truncated() bool { return w.dropped > 0 }

func (w *limitedWriter) close() error {
	if w.wasClosed {
		return nil
	}
	w.wasClosed = true
	if err := w.file.Close(); err != nil {
		w.closeErr = err
	}
	return w.closeErr
}

// Exists 判断某次尝试是否留下过 stdout 产物。
//
// 只认 .out：只写了 meta 的目录不该被当成"输出存在"，
// 否则进程在写入前崩溃时接口会报告一个空文件。
func (a *ArtifactStore) Exists(jobID string, attempt int) bool {
	path, err := a.pathOf(jobID, attempt, "out")
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Stat 返回某个流已落盘的字节数；文件不存在时给出 ErrArtifactMissing。
//
// 接口需要它才能把"文件里一共多少"与"这次返回了多少"分开说清（TASK-E07 的 size_bytes）：
// Read/Tail 只报告内容与是否截断，长度不足以还原原始大小。
func (a *ArtifactStore) Stat(jobID string, attempt int, stream string) (int64, error) {
	path, err := a.pathOf(jobID, attempt, stream)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, a.wrapOpenError(err, jobID, attempt, stream)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("artifact: job %s attempt %d stream %s is not a regular file", jobID, attempt, stream)
	}
	return info.Size(), nil
}

// Read 读取某个流的内容，最多 maxBytes 字节。
//
// 超限从头部保留、尾部去掉并置 truncated：看"这次执行开头在说什么"比看结尾更有用，
// 而默认展示尾部是 Tail 的职责。maxBytes <= 0 表示不限制——单个文件不超过 max_bytes，
// 调用方拿到全量不会造成内存失控。
func (a *ArtifactStore) Read(jobID string, attempt int, stream string, maxBytes int64) ([]byte, bool, error) {
	path, err := a.pathOf(jobID, attempt, stream)
	if err != nil {
		return nil, false, err
	}

	if maxBytes <= 0 {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false, a.wrapOpenError(err, jobID, attempt, stream)
		}
		return data, false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, false, a.wrapOpenError(err, jobID, attempt, stream)
	}
	defer file.Close()

	// 多读 1 字节：只有确实还有更多内容时才标记截断，正好等于上限时不算截断。
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("artifact: read %s: %w", stream, err)
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

// Tail 从文件末尾反向读取最多 n 字节，供结果面板默认只展示尾部。
//
// 文件不超过 n 时返回全文且 truncated=false。n<=0 表示"只要结论不要内容"：
// 返回空切片，文件非空时 truncated=true。
func (a *ArtifactStore) Tail(jobID string, attempt int, stream string, n int64) ([]byte, bool, error) {
	path, err := a.pathOf(jobID, attempt, stream)
	if err != nil {
		return nil, false, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, false, a.wrapOpenError(err, jobID, attempt, stream)
	}
	defer file.Close()

	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, false, fmt.Errorf("artifact: size %s: %w", stream, err)
	}
	if n <= 0 {
		return []byte{}, size > 0, nil
	}

	start := int64(0)
	if size > n {
		start = size - n
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("artifact: seek %s: %w", stream, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, false, fmt.Errorf("artifact: read %s: %w", stream, err)
	}
	return data, size > n, nil
}

// pathOf 校验入参并拼出某个流的路径，所有读入口都走它。
func (a *ArtifactStore) pathOf(jobID string, attempt int, stream string) (string, error) {
	dir, err := a.jobDir(jobID)
	if err != nil {
		return "", err
	}
	return streamPath(dir, attempt, stream)
}

// wrapOpenError 把"文件不存在"换成可判定的哨兵错误，其余原样包装。
func (a *ArtifactStore) wrapOpenError(err error, jobID string, attempt int, stream string) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: job %s attempt %d stream %s", ErrArtifactMissing, jobID, attempt, stream)
	}
	return fmt.Errorf("artifact: open %s: %w", stream, err)
}

// Remove 删除某个任务的全部产物。
//
// 本卡不把它挂到 Cancel 上：core.Store 不该知道文件系统产物（设计文档 §6.4）。
// 取消与终态淘汰留下的目录由启动时的孤儿清理回收，最多多活到下一次重启。
func (a *ArtifactStore) Remove(jobID string) error {
	dir, err := a.jobDir(jobID)
	if err != nil {
		return err
	}

	// ID 已经校验过，这里再确认解析后的目录仍在根目录内：
	// 手工放进来的符号链接不该让删除跑到别处。
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("artifact: resolve %s: %w", jobID, err)
	}
	if !isUnder(a.dir, resolved) {
		return fmt.Errorf("artifact: refusing to remove %s, it is not under %s", resolved, a.dir)
	}
	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("artifact: remove %s: %w", jobID, err)
	}
	return nil
}

// isUnder 判断路径是否落在根目录之内。比较时带上结尾分隔符，
// 避免 /data/exec 与 /data/executions 互相误判。
func isUnder(root, path string) bool {
	if root == path {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// PurgeExpired 删除修改时间超过 TTL 的任务目录，返回删除数量。
// TTL 为 0 表示不按时间清理，直接返回 0。
func (a *ArtifactStore) PurgeExpired() (int, error) {
	if a.ttl <= 0 {
		return 0, nil
	}
	dirs, err := a.jobDirs()
	if err != nil {
		return 0, err
	}

	cutoff := a.now().Add(-a.ttl)
	deleted := 0
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return deleted, fmt.Errorf("artifact: stat %s: %w", dir, err)
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return deleted, fmt.Errorf("artifact: remove expired %s: %w", dir, err)
		}
		deleted++
	}
	if deleted > 0 {
		a.logger.Info("artifact expired directories purged", "count", deleted, "ttl", a.ttl)
	}
	return deleted, nil
}

// PurgeOrphans 删除"任务已不存在"的产物目录，返回删除数量。
//
// live 返回错误时本轮整体跳过、什么都不删：存储读失败是小事，
// 在信息不全的情况下做删除才是不可接受的。
//
// 与 history_limit 的既有关系（配置两份时要连着看）：`store.history_limit: -1` 的部署不留终态快照，
// 于是刚跑完的任务在下次启动时就会被判成孤儿。正因为这个风险，本方法只在启动时跑一次
// （见 Start），周期扫描只按 TTL 删除。
func (a *ArtifactStore) PurgeOrphans(live func() (map[string]bool, error)) (int, error) {
	if live == nil {
		return 0, errors.New("artifact: PurgeOrphans requires a live job set provider")
	}

	liveJobs, err := live()
	if err != nil {
		a.logger.Warn("artifact orphan scan skipped because the live job set is unavailable", "error", err)
		return 0, nil
	}

	dirs, err := a.jobDirs()
	if err != nil {
		return 0, err
	}

	deleted := 0
	for _, dir := range dirs {
		if liveJobs[filepath.Base(dir)] {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return deleted, fmt.Errorf("artifact: remove orphan %s: %w", dir, err)
		}
		deleted++
	}
	if deleted > 0 {
		a.logger.Info("artifact orphan directories purged", "count", deleted)
	}
	return deleted, nil
}

// jobDirs 列出根目录下的一级任务目录，非目录条目忽略。
func (a *ArtifactStore) jobDirs() ([]string, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, fmt.Errorf("artifact: list %s: %w", a.dir, err)
	}

	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// 目录名是写入时校验过的，但手工放进来的名字不受约束；
		// 校验不过的名字不参与清理，避免误删别人放在这里的目录。
		if err := checkArtifactJobID(entry.Name()); err != nil {
			a.logger.Warn("artifact entry is not a job id, leaving it alone", "name", entry.Name())
			continue
		}
		dirs = append(dirs, filepath.Join(a.dir, entry.Name()))
	}
	return dirs, nil
}

// purgeInterval 是周期清理的间隔：只按 TTL 过期，不重跑孤儿扫描。
const purgeInterval = 24 * time.Hour

// Start 清一次、再挂一个后台清理：启动这一轮同步跑（孤儿 + 过期），之后每 24 小时只按 TTL 清理。
// 返回的通道在协程退出时关闭，调用方据此等待收尾（关闭路径用它 join）。
//
// 同步跑第一轮有两个理由：调用方在开始服务之前就把产物清干净，
// 以及测试不需要和协程调度抢顺序就能断言删除结果。
//
// ctx 传 nil 会被归一化为 context.Background()：否则 <-ctx.Done() 永久阻塞，
// 表现为"取消清理协程也退不出去"。
// 两轮清理里的 panic 都会被恢复并记日志：清理任务不该把进程带下去。
func (a *ArtifactStore) Start(ctx context.Context, live func() (map[string]bool, error)) <-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	a.runStartup(live)

	done := make(chan struct{})
	go func() {
		defer close(done)

		ticker := time.NewTicker(purgeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.runSafely("artifact expiry purge", func() { a.purgeExpiredChecked() })
			}
		}
	}()

	return done
}

// runStartup 是启动那一轮完整扫描。孤儿清理只做这一次（理由见 PurgeOrphans）。
func (a *ArtifactStore) runStartup(live func() (map[string]bool, error)) {
	a.runSafely("artifact startup scan", func() {
		a.purgeOnce.Do(func() {
			if _, err := a.PurgeOrphans(live); err != nil {
				a.logger.Warn("artifact orphan purge failed", "error", err)
			}
		})
		a.purgeExpiredChecked()
	})
}

// purgeExpiredChecked 是带日志的过期清理，供两处调用方共用。
func (a *ArtifactStore) purgeExpiredChecked() {
	if _, err := a.PurgeExpired(); err != nil {
		a.logger.Warn("artifact expiry purge failed", "error", err)
	}
}

// runSafely 兜住清理里的 panic：产物清理是后台事务，不该把进程带下去。
// 恢复之后本次调用直接结束，周期协程仍会继续下一轮。
func (a *ArtifactStore) runSafely(label string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			a.logger.Error("artifact cleaner recovered from a panic", "stage", label, "panic", recovered)
		}
	}()
	fn()
}
