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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"godelayq/api"
	"godelayq/core"
	"godelayq/store/sqlite"
)

// logSink 是带锁的日志收集器。用例主协程读它的时候，store 的 flushLoop 与两个观测写入器的
// 后台协程都可能经同一条 logger 往里写（落盘失败、目录被占用都会走日志），
// 裸 bytes.Buffer 在这种交错下就是一条数据竞争报告。
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *logSink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// smokeStack 是一份真实装配出来的最小运行面：日志器 + 调度器 + JSON 存储 + 观测层两个写入器。
type smokeStack struct {
	logs      *logSink
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

	stack := &smokeStack{logs: &logSink{}, applied: cfg}

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
	t.Cleanup(func() { _ = store.Close() })

	scheduler := core.NewScheduler(store, retryPolicyFor(cfg), nil, core.WithLogger(logger))
	scheduler.SetConcurrency(cfg.Scheduler.Workers)
	scheduler.SetQueueCapacity(cfg.Scheduler.QueueCapacity)
	stack.scheduler = scheduler

	// 关闭顺序：t.Cleanup 后注册的先跑，所以按 store → db → events → audit 注册，
	// 实际关闭就是 audit → events → db → store，即两个写入器先撤订阅、再关库、最后关存储。
	// 每条都紧跟自己的构造函数注册：中途 Fatalf 时已经建起来的资源不会漏关。
	db, err := sqlite.Open(cfg.Observability, logger)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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
	t.Cleanup(func() { _ = events.Close() })

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
	t.Cleanup(func() { _ = audit.Close() })

	return stack
}

// retryPolicyFor 与 cmd/server 启动时同一条构造方式（main.go 用 scheduler.max_retry_delay 建它）。
func retryPolicyFor(cfg core.Config) core.RetryPolicy {
	return &core.ExponentialBackoffRetry{MaxDelay: cfg.Scheduler.MaxRetryDelay}
}

// reloadOutcome 是一次 miniature 重载的结果。前三个键名列表与 `result` 对齐 R06 要交出去的
// `core.ReloadState`（applied_keys / ignored_keys / rejected_keys / result）；
// `failedKeys` 与下面那个读失败占位标记是本 harness 自己的形状——`ReloadState` 没有
// failed_keys，失败原因走它的 `error` 字符串字段（设计文档 §5.4），R06 接线时按那边收口。
type reloadOutcome struct {
	appliedKeys  []string
	ignoredKeys  []string
	rejectedKeys []string
	failedKeys   []string
	result       core.ReloadResult
}

// reloadFromFile 是 applyCandidate 的外层：先读文件，读不进来就整次失败、一条都不应用。
// "新配置读不进来"那条路只有经过这里才有失败形状，否则用例只能复述起点状态。
func (s *smokeStack) reloadFromFile(path string) reloadOutcome {
	candidate, err := core.LoadConfig(path)
	if err != nil {
		// 读失败没有键路径可报，用固定标记占位，保证 failed_keys 不为空、日志能归因。
		return reloadOutcome{
			result:     core.ReloadFailed,
			failedKeys: []string{"<配置文件读失败>"},
		}
	}
	return s.applyCandidate(candidate.Normalized())
}

// applyCandidate 走 R06 将要走的那几步：Diff 分类 → 有拒绝项就整次作废 →
// 先整批查表确认每条热更键都有入口 → 逐键调运行期入口 → 全部成功才把 applied 往前推。
//
// 半途失败（入口返回错误）时本 harness **不做回滚**：逐项逆序回滚（不变量 I2 的后半句）
// 是 R06 的交付物，这里只如实报出"哪一条失败了、已应用的是哪几条、applied 没动"，
// 免得冒烟具自己先立一套与 R06 不同的语义。
//
// 应用顺序取的是 `core.Diff` 给出的路径字典序，这只是本 harness 的取法：设计 §4 的 I2
// 要求 R06 按依赖顺序应用。下面有用例断到"失败键之后的键没被继续应用"，那条断言的语义是
// 循环在失败处停下，与顺序无关；但它选的"失败键之后那一条"（`store.history_limit`）是按
// 本 harness 的取法挑的，R06 换成依赖顺序时要跟着换键名。
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

	entries := s.hotEntries()

	// 先整批查表再应用：有一条热更键没有入口就是装配错误，一条都不该碰到现网。
	// （入口自己返回错误是另一种形状——已经应用的那几条留在那里不回滚，走第二个循环的分支。）
	for _, key := range change.Hot {
		if _, ok := entries[key.Path]; !ok {
			outcome.result = core.ReloadFailed
			outcome.failedKeys = append(outcome.failedKeys, key.Path)
			return outcome
		}
	}

	for _, key := range change.Hot {
		if err := s.applyHotKey(key.Path, candidate); err != nil {
			outcome.result = core.ReloadFailed
			outcome.failedKeys = append(outcome.failedKeys, key.Path)
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

// hotEntries 是 TASK-R02 §10.1 那张表的代码形态：热更键 → 入口。
// 表只写这一份：查入口与做应用都从这里取，避免"键名清单"在两处各自漂。
// 表里没写的键不会被应用（调用方按 ok 判定），以此避免"以后加了个热更键却没给入口，冒烟静默跳过"。
// 反过来（表上加了键却没有用例走它）由主用例把表上的键名集合与期望清单逐条比过一次来挡。
func (s *smokeStack) hotEntries() map[string]func(core.Config) error {
	return map[string]func(core.Config) error{
		"logging.level": func(cfg core.Config) error {
			return core.SetLogLevel(s.levelVar, cfg.Logging.Level)
		},
		"scheduler.max_retry_delay": func(cfg core.Config) error {
			s.scheduler.SetRetryPolicy(retryPolicyFor(cfg))
			return nil
		},
		"store.history_limit": func(cfg core.Config) error {
			s.store.SetHistoryRetention(cfg.Store.HistoryLimit, cfg.Store.HistoryTTL)
			return nil
		},
		"store.history_ttl": func(cfg core.Config) error {
			s.store.SetHistoryRetention(cfg.Store.HistoryLimit, cfg.Store.HistoryTTL)
			return nil
		},
		"observability.events.retention_count": func(cfg core.Config) error {
			s.events.SetRetention(cfg.Observability.Events.RetentionCount, cfg.Observability.Events.RetentionAge)
			return nil
		},
		"observability.events.retention_age": func(cfg core.Config) error {
			s.events.SetRetention(cfg.Observability.Events.RetentionCount, cfg.Observability.Events.RetentionAge)
			return nil
		},
		"observability.audit.retention_count": func(cfg core.Config) error {
			s.audit.SetRetention(cfg.Observability.Audit.RetentionCount, cfg.Observability.Audit.RetentionAge)
			return nil
		},
		"observability.audit.retention_age": func(cfg core.Config) error {
			s.audit.SetRetention(cfg.Observability.Audit.RetentionCount, cfg.Observability.Audit.RetentionAge)
			return nil
		},
	}
}

// applyHotKey 走 hotEntries 里的入口；表里没有这条键时返回带键名的错误，让日志能归因。
func (s *smokeStack) applyHotKey(path string, cfg core.Config) error {
	entry, ok := s.hotEntries()[path]
	if !ok {
		return fmt.Errorf("热更键 %q 在 TASK-R02 §10.1 的表里没有对应入口", path)
	}
	return entry(cfg)
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

// TestSmokeHotReloadAppliesEveryHotKey 是主冒烟：一次改动把 §10.1 表上的
// **八条热更键路径全部走一遍**（五个写入口 + 时长/年龄那几条成对的第二元素），
// 逐个验证行为真的变了——这是 R01 与 R02 合起来对外承诺的那件事。
// 每个键名字面量都要被走到一次，否则键名写错了没人发现（入口查不到只会走"缺入口"分支，
// 而那条分支在别的用例里才断）。
func TestSmokeHotReloadAppliesEveryHotKey(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Store.HistoryLimit = 100
		cfg.Store.HistoryTTL = 0 // 不按时间淘汰
		cfg.Scheduler.MaxRetryDelay = time.Minute
		cfg.Observability.Events.RetentionCount = 1000
		cfg.Observability.Events.RetentionAge = 0
		cfg.Observability.Audit.RetentionCount = 1000
		cfg.Observability.Audit.RetentionAge = 0
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
	// 两条时长都放到 24h：起点数据都是刚刚写的，不会被时间维度顺手剪掉，
	// 于是"条数维度剪到 2"这条判据仍然只反映条数入口的作用。
	candidate.Store.HistoryTTL = 24 * time.Hour
	candidate.Observability.Events.RetentionCount = 2
	candidate.Observability.Events.RetentionAge = 24 * time.Hour
	candidate.Observability.Audit.RetentionCount = 2
	candidate.Observability.Audit.RetentionAge = 24 * time.Hour

	outcome := stack.applyCandidate(candidate)

	if outcome.result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（failed=%v rejected=%v）",
			outcome.result, core.ReloadOK, outcome.failedKeys, outcome.rejectedKeys)
	}
	wantApplied := []string{
		"logging.level",
		"observability.audit.retention_age",
		"observability.audit.retention_count",
		"observability.events.retention_age",
		"observability.events.retention_count",
		"scheduler.max_retry_delay",
		"store.history_limit",
		"store.history_ttl",
	}
	assertSameList(t, "applied keys", outcome.appliedKeys, wantApplied)

	// 期望清单必须与表上的键名集合逐条相等，否则这份字面清单会与 hotEntries() 各自漂：
	// 表上多出一条（R03/R04 会往里加键）而这里没跟着改，就是"加了入口却没人走过它"。
	var entryKeys []string
	for path := range stack.hotEntries() {
		entryKeys = append(entryKeys, path)
	}
	sort.Strings(entryKeys)
	assertSameList(t, "keys in hotEntries()", entryKeys, wantApplied)

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
}

// TestSmokeRejectedKeyAbortsWholeReload 守住不变量 I1 与 I2：
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

// TestSmokeHotKeyWithoutEntryFailsLoud 是 §10.1 那张表上的保险丝，两层都要判：
// 入口查不到这条键时要返回带键名的错误；而一次改动里只要有一条热更键没有入口，
// 整次就一条都不应用（缺入口是装配错误，不该让别的键先改到现网上去）。
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

	candidate := applied
	candidate.Scheduler.Workers = 6   // 热更键，但本卡没给入口（R03 才交付 ResizeWorkers）
	candidate.Logging.Level = "debug" // 有入口：预检失败时它也一条都不该被应用

	outcome := stack.applyCandidate(candidate)

	if outcome.result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q", outcome.result, core.ReloadFailed)
	}
	assertSameList(t, "failed keys", outcome.failedKeys, []string{"scheduler.workers"})
	if len(outcome.appliedKeys) != 0 {
		t.Fatalf("缺入口的那一次不该应用任何键：%v", outcome.appliedKeys)
	}
	writeAtLevel(stack, slog.LevelDebug, "must stay hidden")
	if strings.Contains(stack.logs.String(), "must stay hidden") {
		t.Fatal("缺入口的那一次把日志级别也改了")
	}
}

// TestSmokeBrokenConfigFailsWithoutTouchingTheStack 证明"新配置读不进来"是一次可报告的失败，
// 而不是"什么都没发生"：一份解码不了的配置（这里往 logging 一节里塞了一个不认识的键，
// `LoadConfig` 走的 `UnmarshalExact` 会把它拒掉）经 reloadFromFile 得到 result=failed
// 与一条可归因的失败项，运行面继续按上一次成功的配置工作。
// 起点先把级别热更到 debug，再喂这份坏文件——这样"仍然按改过的配置工作"才是判据，
// 而不是无动作可推翻的起点复述。
func TestSmokeBrokenConfigFailsWithoutTouchingTheStack(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Store.HistoryLimit = 100
		return cfg
	})
	stack := newSmokeStack(t, applied)

	good := applied
	good.Logging.Level = "debug"
	if outcome := stack.applyCandidate(good); outcome.result != core.ReloadOK {
		t.Fatalf("起点热更失败：%+v", outcome)
	}
	writeAtLevel(stack, slog.LevelDebug, "applied before the broken file")
	if !strings.Contains(stack.logs.String(), "applied before the broken file") {
		t.Fatal("起点没把级别改到 debug，后面的判据就没有对照")
	}

	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("logging:\n  level_not_a_key: debug\n"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}

	outcome := stack.reloadFromFile(broken)

	if outcome.result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q", outcome.result, core.ReloadFailed)
	}
	if len(outcome.failedKeys) == 0 {
		t.Fatal("读失败没有可归因的失败项，日志会看不出是哪一次坏了")
	}
	if len(outcome.appliedKeys) != 0 {
		t.Fatalf("读失败的那一次不该应用任何键：%v", outcome.appliedKeys)
	}

	// 现网继续按上一次成功的配置工作：debug 仍写得出来、留痕上限仍是 100。
	stack.logs.Reset()
	writeAtLevel(stack, slog.LevelDebug, "still debug")
	if !strings.Contains(stack.logs.String(), "still debug") {
		t.Fatal("坏配置把已经生效的级别带跑了")
	}
	seedTerminalSnapshots(t, stack, 5)
	requireSnapshotCount(t, stack, 5, "坏配置之后旧的留痕上限要原样留着")

	if stack.applied.Logging.Level != "debug" {
		t.Fatalf("applied 被坏配置改写了：level = %q", stack.applied.Logging.Level)
	}
}

// TestSmokeEntryReturningErrorKeepsAppliedBack 覆盖"入口存在但返回错误"那一支：
// 这里给一个 parseLogLevel 认不出的级别名（真链路上 Validate 会先拦住，本用例刻意不调它），
// 只为把这条分支的返回值形状固定下来。断三件事：这条键被报成失败、循环在它这里就停了
// （排在它后面的键一条都没应用）、applied 没有前推。
//
// 本用例造不出"半应用"的状态：表上只有 logging.level 那一个入口会返回错误，
// 而它在 Diff 的有序路径里排最前，所以走到失败时必然还没应用任何键。
// 设计 §4 的不变量 I2 要的是"按依赖顺序应用 + 中途失败逐项回滚"，
// 半应用与回滚只能由 R06 用排在后面、又会失败的入口来断。
func TestSmokeEntryReturningErrorKeepsAppliedBack(t *testing.T) {
	dir := t.TempDir()
	applied := smokeConfig(t, dir, "info", func(cfg core.Config) core.Config {
		cfg.Store.HistoryLimit = 100
		return cfg
	})
	stack := newSmokeStack(t, applied)

	candidate := applied
	candidate.Store.HistoryLimit = 2    // 它排在失败键之后，所以本次不会被应用
	candidate.Logging.Level = "verbose" // 入口会返回错误，而且它排在最前

	outcome := stack.applyCandidate(candidate)

	if outcome.result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q", outcome.result, core.ReloadFailed)
	}
	assertSameList(t, "failed keys", outcome.failedKeys, []string{"logging.level"})
	if len(outcome.appliedKeys) != 0 {
		t.Fatalf("applied keys = %v, want 空（失败处应当停住，后面的键不该被接着应用）",
			outcome.appliedKeys)
	}
	if stack.applied.Store.HistoryLimit == 2 {
		t.Fatal("失败的重载把 applied 往前推了，违反不变量 I1")
	}

	// 行为侧同一条判据：store.history_limit 的入口没被跑到，运行面的上限仍是 100，
	// 四条留痕一条都不会被剪掉（若上限已被换成 2，写完四条之后只剩 2 条）。
	// 这里证明的是"这一步没执行"；"执行了就要用候选配置里的值"那一半由主用例证明。
	seedTerminalSnapshots(t, stack, 4)
	requireSnapshotCount(t, stack, 4, "失败键之后的热更项不该被继续应用")

	// 现网：级别既没被写坏也没被清空，仍是 info（debug 出不来）。
	writeAtLevel(stack, slog.LevelDebug, "must stay hidden")
	if strings.Contains(stack.logs.String(), "must stay hidden") {
		t.Fatal("失败的 SetLogLevel 把级别载体改坏了")
	}
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
