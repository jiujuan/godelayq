package main

// 配置热重载的冒烟测试：把 R01 的判据与 R02 的六个运行期入口串起来跑一遍。
//
// R06 才把监听器与 /admin/runtime 接上来，所以这里自带一个 miniature 重载器 applyCandidate：
// 它按 TASK-R02 §10.1 那张"热更键 → 入口 → 生效时机"的表，把一份从磁盘读进来的新配置过一遍
// core.Diff，再逐键调用对应的入口。这张表在这里第一次被当成接口使用——签名对不上就编译不过。
//
// 判据一律取行为（日志真的按新级别出、留痕真的按新上限淘汰、两张观测表真的按新条数剪），
// 分类结果只作为"该调哪个入口"的路由证据。R06 接线之后本文件的 applyCandidate 应被生产重载链替换，
// 场景清单留下。

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"godelayq/api"
	"godelayq/core"
	"godelayq/store/sqlite"
)

// smokeStack 是一份真实装配出来的最小运行面：日志器 + 调度器 + JSON 存储 + 观测层两个写入器。
type smokeStack struct {
	logs      *bytes.Buffer
	logger    *slog.Logger
	levelVar  *slog.LevelVar
	scheduler *core.Scheduler
	store     *core.JSONFileStore
	audit     *sqlite.AuditLog
	events    *sqlite.EventLog
	bus       *core.EventBus

	// applied 是"当前生效的那一份"，只有重载成功才往前推（系列不变量 I1）。
	applied core.Config
}

func newSmokeStack(t *testing.T, cfg core.Config) *smokeStack {
	t.Helper()

	stack := &smokeStack{logs: &bytes.Buffer{}, applied: cfg}

	logger, levelVar, err := core.NewLoggerWithLevelVar(cfg.Logging.Level, cfg.Logging.Format, stack.logs)
	if err != nil {
		t.Fatalf("NewLoggerWithLevelVar: %v", err)
	}
	stack.logger = logger
	stack.levelVar = levelVar

	store, err := core.NewJSONFileStoreWithOptions(cfg.Store.Path, core.StoreOptions{
		HistoryLimit: cfg.Store.HistoryLimit,
		HistoryTTL:   cfg.Store.HistoryTTL,
		Interval:     cfg.Store.FlushInterval,
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("NewJSONFileStoreWithOptions: %v", err)
	}
	stack.store = store

	scheduler := core.NewScheduler(store, retryPolicyFor(cfg), nil, core.WithLogger(logger))
	scheduler.SetConcurrency(cfg.Scheduler.Workers)
	scheduler.SetQueueCapacity(cfg.Scheduler.QueueCapacity)
	stack.scheduler = scheduler

	db, err := sqlite.Open(cfg.Observability, logger)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}

	bus := core.NewEventBus(200)
	stack.bus = bus

	events, err := sqlite.NewEventLog(bus, db, sqlite.EventLogOptions{
		FlushInterval:  cfg.Observability.FlushInterval,
		QueueCapacity:  cfg.Observability.QueueCapacity,
		RetentionCount: cfg.Observability.Events.RetentionCount,
		RetentionAge:   cfg.Observability.Events.RetentionAge,
	}, logger)
	if err != nil {
		t.Fatalf("NewEventLog: %v", err)
	}
	stack.events = events

	audit, err := sqlite.NewAuditLog(db, sqlite.AuditLogOptions{
		FlushInterval:  cfg.Observability.FlushInterval,
		QueueCapacity:  cfg.Observability.QueueCapacity,
		RetentionCount: cfg.Observability.Audit.RetentionCount,
		RetentionAge:   cfg.Observability.Audit.RetentionAge,
	}, logger)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	stack.audit = audit

	// 关闭顺序照装配口径：先撤两个写入器的订阅，再关库，最后关存储。
	t.Cleanup(func() {
		_ = events.Close()
		_ = audit.Close()
		_ = db.Close()
		_ = store.Close()
	})

	return stack
}

// retryPolicyFor 与 cmd/server 启动时同一条构造方式（main.go 用 scheduler.max_retry_delay 建它）。
func retryPolicyFor(cfg core.Config) core.RetryPolicy {
	return &core.ExponentialBackoffRetry{MaxDelay: cfg.Scheduler.MaxRetryDelay}
}

// reloadOutcome 是一次 miniature 重载的结果，字段形状对齐 R06 要交出去的 ReloadState。
type reloadOutcome struct {
	appliedKeys  []string
	ignoredKeys  []string
	rejectedKeys []string
	result       core.ReloadResult
}

// applyCandidate 走 R06 将要走的那几步：Diff 分类 → 有拒绝项就整次作废 →
// 逐键调运行期入口 → 成功后才把 applied 往前推。
func (s *smokeStack) applyCandidate(candidate core.Config) reloadOutcome {
	change := core.Diff(s.applied, candidate)
	outcome := reloadOutcome{result: core.ReloadOK}

	switch {
	case !change.HasChanges():
		outcome.result = core.ReloadUnchanged
		return outcome
	case change.HasRejections():
		// 不变量 I2：有拒绝项就一条都不应用。
		for _, key := range change.Reject {
			outcome.rejectedKeys = append(outcome.rejectedKeys, key.Path)
		}
		outcome.result = core.ReloadRejected
		return outcome
	}

	for _, key := range change.Hot {
		if err := s.applyHotKey(key.Path, candidate); err != nil {
			outcome.result = core.ReloadFailed
			outcome.ignoredKeys = append(outcome.ignoredKeys, key.Path)
			return outcome
		}
		outcome.appliedKeys = append(outcome.appliedKeys, key.Path)
	}

	// 重启档不应用，只如实报告"这些键要重启才生效"（不变量 I3：静默就是缺陷）。
	for _, key := range change.Restart {
		outcome.ignoredKeys = append(outcome.ignoredKeys, key.Path)
	}

	s.applied = candidate
	return outcome
}

// applyHotKey 是 TASK-R02 §10.1 那张表的代码形态：热更键 → 入口。
// 表里没写的键返回错误，避免"以后加了个热更键却没给入口，冒烟静默跳过"。
func (s *smokeStack) applyHotKey(path string, cfg core.Config) error {
	switch path {
	case "logging.level":
		return core.SetLogLevel(s.levelVar, cfg.Logging.Level)
	case "scheduler.max_retry_delay":
		s.scheduler.SetRetryPolicy(retryPolicyFor(cfg))
		return nil
	case "store.history_limit", "store.history_ttl":
		s.store.SetHistoryRetention(cfg.Store.HistoryLimit, cfg.Store.HistoryTTL)
		return nil
	case "observability.events.retention_count", "observability.events.retention_age":
		s.events.SetRetention(cfg.Observability.Events.RetentionCount, cfg.Observability.Events.RetentionAge)
		return nil
	case "observability.audit.retention_count", "observability.audit.retention_age":
		s.audit.SetRetention(cfg.Observability.Audit.RetentionCount, cfg.Observability.Audit.RetentionAge)
		return nil
	default:
		return fmt.Errorf("热更键 %q 在 TASK-R02 §10.1 的表里没有对应入口", path)
	}
}

// smokeConfig 写一份落在 temp 目录里的配置并读回来：路径全部指向用例自己的目录，
// 没写的键由 core 的默认值补。level 传空串表示不写 logging 一节。
func smokeConfig(t *testing.T, dir, level string, tune func(core.Config) core.Config) core.Config {
	t.Helper()

	logging := ""
	if level != "" {
		logging = fmt.Sprintf("logging:\n  level: %s\n", level)
	}

	content := logging + `
store:
  type: json
  path: ` + filepath.Join(dir, "jobs.json") + `
  groups_path: ` + filepath.Join(dir, "groups.json") + `
observability:
  enabled: true
  path: ` + filepath.Join(dir, "observe.sqlite") + `
  events:
    enabled: true
  audit:
    enabled: true
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := core.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if tune != nil {
		cfg = tune(cfg)
	}
	return cfg
}

// TestSmokeHotReloadAppliesAllSixEntries 是主冒烟：一次改全部六个热更键，
// 逐个验证行为真的变了——这是 R01 与 R02 合起来对外承诺的那件事。
func TestSmokeHotReloadAppliesAllSixEntries(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Store.HistoryLimit = 100
		cfg.Scheduler.MaxRetryDelay = time.Minute
		cfg.Observability.Events.RetentionCount = 1000
		cfg.Observability.Audit.RetentionCount = 1000
		return cfg
	})
	stack := newSmokeStack(t, applied)

	// 先把旧值跑出可观察的形状：debug 写不出来、留痕四条全在、两张表各四条。
	writeAtLevel(stack, slog.LevelDebug, "hidden at info")
	if strings.Contains(stack.logs.String(), "hidden at info") {
		t.Fatal("冒烟起点不对：info 级别的日志器写出了 debug 记录")
	}
	seedTerminalSnapshots(t, stack, 4)
	seedEvents(t, stack, 4)
	seedAudit(t, stack, 4)

	candidate := applied
	candidate.Logging.Level = "debug"
	candidate.Scheduler.MaxRetryDelay = 5 * time.Second
	candidate.Store.HistoryLimit = 2
	candidate.Observability.Events.RetentionCount = 2
	candidate.Observability.Audit.RetentionCount = 2

	outcome := stack.applyCandidate(candidate)

	if outcome.result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（rejected=%v）", outcome.result, core.ReloadOK, outcome.rejectedKeys)
	}
	wantApplied := []string{
		"logging.level",
		"observability.audit.retention_count",
		"observability.events.retention_count",
		"scheduler.max_retry_delay",
		"store.history_limit",
	}
	assertSameList(t, "applied keys", outcome.appliedKeys, wantApplied)

	// 1. 日志级别：下一条日志就按 debug 出，且 info 仍出。
	writeAtLevel(stack, slog.LevelDebug, "debug visible")
	if !strings.Contains(stack.logs.String(), "debug visible") {
		t.Fatalf("改到 debug 之后记录仍被挡在门外：%q", stack.logs.String())
	}
	writeAtLevel(stack, slog.LevelInfo, "info visible")
	if !strings.Contains(stack.logs.String(), "info visible") {
		t.Fatal("info 记录在 debug 级别下反而写不出来")
	}

	// 2. 重试上限：读数换成新值（排期行为由 core 包里的用例证明，这里只证入口接得上）。
	if got := stack.scheduler.RetryPolicyMaxDelay(); got != 5*time.Second {
		t.Fatalf("RetryPolicyMaxDelay = %v, want 5s", got)
	}

	// 3. 留痕上限：下一次写入触发的 trim 用 2。
	requireSnapshotCount(t, stack, 4, "setter 之后不该自己重排已有留痕")
	writeTerminalSnapshot(t, stack, "after-apply")
	requireSnapshotCount(t, stack, 2, "新上限要在下一次写入生效")

	// 4. 事件表：下一个批量周期的 prune 用 2（淘汰在批量写的事务里跑，所以要再落一次盘）。
	publishEvent(t, stack, "job-after")
	requireTableCount(t, stack.events, 2, "job_events")

	// 5. 台账：同上。
	appendAuditRow(t, stack)
	requireTableCount(t, stack.audit, 2, "write_audit")
} // TestSmokeRejectedKeyAbortsWholeReload 守住不变量 I1 与 I2：
// 一次改动里混进凭据键时整次作废，本来会生效的键也一条都不应用，
// 而被拒的键名要能被读数拿到（不能让"被拒"这件事静默）。
func TestSmokeRejectedKeyAbortsWholeReload(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Store.HistoryLimit = 100
		return cfg
	})
	stack := newSmokeStack(t, applied)

	candidate := applied
	candidate.Logging.Level = "debug"       // 本来会热更
	candidate.Store.HistoryLimit = 3        // 本来会热更
	candidate.Server.Auth.Token = "rotated" // 拒绝档：凭据

	outcome := stack.applyCandidate(candidate)

	if outcome.result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", outcome.result, core.ReloadRejected)
	}
	assertSameList(t, "rejected keys", outcome.rejectedKeys, []string{"server.auth.token"})
	if len(outcome.appliedKeys) != 0 {
		t.Fatalf("被拒的那次仍然应用了 %v，整次作废没守住", outcome.appliedKeys)
	}

	// 行为侧两条判据：级别仍是 info、留痕上限仍是 100。
	writeAtLevel(stack, slog.LevelDebug, "should not appear")
	if strings.Contains(stack.logs.String(), "should not appear") {
		t.Fatal("被拒的重载把日志级别也改了")
	}
	seedTerminalSnapshots(t, stack, 5)
	requireSnapshotCount(t, stack, 5, "旧的 100 条上限要原样留着")

	if stack.applied.Server.Auth.Token != applied.Server.Auth.Token {
		t.Fatal("失败的重载把 applied 往前推了，违反不变量 I1")
	}
}

// TestSmokeRestartKeysAreReportedNotApplied 守住重启档的可见性：一条都不应用，
// 但键名要出现在 ignored_keys 里，不能静默。
func TestSmokeRestartKeysAreReportedNotApplied(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", nil)
	stack := newSmokeStack(t, applied)

	candidate := applied
	candidate.Server.Port = "9090"
	candidate.Observability.Enabled = false

	outcome := stack.applyCandidate(candidate)

	if outcome.result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（重启档不该把整次判失败）", outcome.result, core.ReloadOK)
	}
	if len(outcome.appliedKeys) != 0 {
		t.Fatalf("重启档被当成热更应用了：%v", outcome.appliedKeys)
	}
	assertSameList(t, "ignored keys", outcome.ignoredKeys, []string{"observability.enabled", "server.port"})
}

// TestSmokeUnchangedConfigIsNotAReload 守住"文件被 touch 但内容没变"不该算一次重载：
// 判据是 core.Diff，而不是文件事件或内容哈希。
func TestSmokeUnchangedConfigIsNotAReload(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", nil)
	stack := newSmokeStack(t, applied)

	outcome := stack.applyCandidate(applied.Normalized())
	if outcome.result != core.ReloadUnchanged {
		t.Fatalf("result = %q, want %q", outcome.result, core.ReloadUnchanged)
	}
	if len(outcome.appliedKeys) != 0 || len(outcome.ignoredKeys) != 0 {
		t.Fatalf("unchanged 那次不该有任何键：%+v", outcome)
	}
}

// TestSmokeHotKeyWithoutEntryFailsLoud 是 §10.1 那张表上的保险丝：
// 以后有人给 core 加了热更键却没在重载链里给它入口时，这里要判失败，
// 而不是"改了文件、什么都没发生"。
func TestSmokeHotKeyWithoutEntryFailsLoud(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", nil)
	stack := newSmokeStack(t, applied)

	// scheduler.workers 属 R03，此刻还不在表上。
	err := stack.applyHotKey("scheduler.workers", applied)
	if err == nil {
		t.Fatal("表里没有入口的键被静默接受了")
	}
	if !strings.Contains(err.Error(), "scheduler.workers") {
		t.Fatalf("错误里没有键名，日志无法归因：%v", err)
	}
}

// TestSmokeBrokenConfigKeepsRunning 证明"新配置读不进来"只是一次重载失败，
// 运行面继续按旧配置工作（不变量 I2 的另一半：失败不碰现网）。
func TestSmokeBrokenConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Store.HistoryLimit = 100
		return cfg
	})
	stack := newSmokeStack(t, applied)

	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("logging:\n  level_not_a_key: debug\n"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}
	if _, err := core.LoadConfig(broken); err == nil {
		t.Fatal("未知键没有被 UnmarshalExact 拒掉，这条冒烟的前提不成立")
	}

	// 运行面照常：info 出、debug 挡、存储照常留痕。
	writeAtLevel(stack, slog.LevelInfo, "still working")
	if !strings.Contains(stack.logs.String(), "still working") {
		t.Fatal("info 记录写不出来")
	}
	writeAtLevel(stack, slog.LevelDebug, "still filtered")
	if strings.Contains(stack.logs.String(), "still filtered") {
		t.Fatal("级别被坏配置带跑了")
	}
	writeTerminalSnapshot(t, stack, "ok")
	requireSnapshotCount(t, stack, 1, "坏配置之后存储仍应正常留痕")
}

// TestSmokeEnvOverrideReachesTheSameEntries 守住设计 §5.3 的口径：
// 环境变量覆盖与文件改动走的是同一条应用路径。
func TestSmokeEnvOverrideReachesTheSameEntries(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Scheduler.MaxRetryDelay = time.Minute
		return cfg
	})
	stack := newSmokeStack(t, applied)

	t.Setenv("GODELAYQ_LOGGING_LEVEL", "debug")
	t.Setenv("GODELAYQ_SCHEDULER_MAX_RETRY_DELAY", "7s")

	candidate, err := core.LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig with env: %v", err)
	}
	candidate = candidate.Normalized()

	outcome := stack.applyCandidate(candidate)
	if outcome.result != core.ReloadOK {
		t.Fatalf("result = %q, want ok（rejected=%v）", outcome.result, outcome.rejectedKeys)
	}
	for _, key := range []string{"logging.level", "scheduler.max_retry_delay"} {
		if !containsString(outcome.appliedKeys, key) {
			t.Fatalf("applied keys %v 里没有 %q", outcome.appliedKeys, key)
		}
	}
	if got := stack.scheduler.RetryPolicyMaxDelay(); got != 7*time.Second {
		t.Fatalf("RetryPolicyMaxDelay = %v, want 7s", got)
	}
	writeAtLevel(stack, slog.LevelDebug, "from env")
	if !strings.Contains(stack.logs.String(), "from env") {
		t.Fatal("环境变量覆盖的级别没有落到运行面")
	}
}

// ---- 观察用的辅助：全部走公开写路径，不读私有字段 ----

// writeAtLevel 用装配时那条 logger 按指定级别写一条记录；级别由载体决定。
func writeAtLevel(stack *smokeStack, level slog.Level, message string) {
	switch level {
	case slog.LevelDebug:
		stack.logger.Debug(message)
	case slog.LevelInfo:
		stack.logger.Info(message)
	case slog.LevelWarn:
		stack.logger.Warn(message)
	case slog.LevelError:
		stack.logger.Error(message)
	default:
		panic("writeAtLevel: 未预期的级别")
	}
}

func writeTerminalSnapshot(t *testing.T, stack *smokeStack, id string) {
	t.Helper()

	if err := stack.store.Update(core.JobSnapshot{
		ID: id, Name: "task", Status: int(core.StatusSuccess), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Update(%s): %v", id, err)
	}
}

func seedTerminalSnapshots(t *testing.T, stack *smokeStack, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		writeTerminalSnapshot(t, stack, fmt.Sprintf("seed-%d", i))
	}
}

func requireSnapshotCount(t *testing.T, stack *smokeStack, want int, comment string) {
	t.Helper()

	snapshots, err := stack.store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(snapshots) != want {
		t.Fatalf("留痕条数 = %d, want %d（%s）", len(snapshots), want, comment)
	}
}

func publishEvent(t *testing.T, stack *smokeStack, jobID string) {
	t.Helper()

	stack.bus.Publish(core.Event{Type: core.EventJobCompleted, JobID: jobID, JobName: "task"})
}

func seedEvents(t *testing.T, stack *smokeStack, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		publishEvent(t, stack, fmt.Sprintf("event-%d", i))
	}
	requireTableCount(t, stack.events, n, "job_events（起点）")
}

func appendAuditRow(t *testing.T, stack *smokeStack) {
	t.Helper()

	if err := stack.audit.Append(api.AuditEntry{
		Time: time.Now(), Actor: "smoke", ActorKind: "user", Role: "admin",
		Action: "job.create", Method: "POST", Route: "/api/v1/jobs",
		Status: 200, Latency: time.Millisecond, Verdict: "ok",
	}); err != nil {
		t.Fatalf("audit Append: %v", err)
	}
}

func seedAudit(t *testing.T, stack *smokeStack, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		appendAuditRow(t, stack)
	}
	requireTableCount(t, stack.audit, n, "write_audit（起点）")
}

// logWriter 是两个观测写入器共有的公开动作：落一次盘，再读行数。
type logWriter interface {
	Flush() error
	Count() (int64, error)
}

// requireTableCount 先落盘（淘汰是在批量写的事务里跑的，不落盘就读不到结果），
// 再等行数到判据；等的是条件而不是固定时长。
func requireTableCount(t *testing.T, log logWriter, want int, label string) {
	t.Helper()

	if err := log.Flush(); err != nil {
		t.Fatalf("Flush(%s): %v", label, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var count int64
	for {
		got, err := log.Count()
		if err != nil {
			t.Fatalf("Count(%s): %v", label, err)
		}
		count = got
		if int(count) == want || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if int(count) != want {
		t.Fatalf("%s 行数 = %d, want %d", label, count, want)
	}
}

func assertSameList(t *testing.T, label string, got, want []string) {
	t.Helper()

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
