package main

// 配置热重载的冒烟测试：一条生产重载链（cmd/server/reload.go 的 reloader）接在**真实下游**上跑。
//
// 与同包另外两组用例的分工：
//   - cmd/server/reload_test.go：落点全是替身，判的是链自己的算法（顺序、回滚、结论、读数）；
//   - main_integration_test.go 的 §5.2 那组：真链 + 替身下游，判的是装配（谁被递到谁）与关闭顺序；
//   - 本文件：真链 + 真 store / 真调度器 / 真 SQLite 两张表，判的是**行为真的变了**。
//
// 判据一律取行为（日志按新级别出、留痕按新上限淘汰、两张表按新条数剪），
// 分类结果只作为"该调哪个入口"的路由证据。R02 那版自带的 miniature 重载器（applyCandidate）
// 在 R06 接线后被删掉：从本文件起，被验的那条链就是进程里跑的那一条。
//
// 档位落点（executors.commands → Applier.ApplyConfig）不在本文件：真 Applier 要的是登记表、
// 产物存储与脚本文件那一整套，而它由 §5.2 的 TestRun_WiredReloadChainReachesEveryTarget
// 用真 Applier + 真处理函数表判过。这里的用例因此一律不碰 executors.*，配置里也把
// executors.enabled 关着（本 harness 没有 applier，那条键若被改动会报"未启用、本次不生效"，
// 会把"没测"混进"测过"的读数里）。

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// smokeStack 是一份真实装配出来的运行面：日志器 + 调度器 + JSON 存储 + 观测层两个写入器，
// 外加接在它们上面的那条生产重载链。
type smokeStack struct {
	t         *testing.T
	dir       string
	path      string
	logs      *logSink
	logger    *slog.Logger
	levelVar  *slog.LevelVar
	scheduler *core.Scheduler
	store     *core.JSONFileStore
	audit     *sqlite.AuditLog
	events    *sqlite.EventLog
	bus       *core.EventBus

	// chain 是被验的那个对象本身：生产构造函数造出来的真链，落点全是上面这些真下游。
	chain *reloader
}

// smokeBaseSpec 是冒烟起点那份配置。八个热更键各给一个可对照的起点值，两条时长都放 0
// （起点数据都是刚刚写的，不让时间维度顺手剪掉，条数判据才只反映条数入口的作用）。
func smokeBaseSpec() fileSpec {
	spec := baseSpec()
	spec.reloadEnabled = "true"
	spec.obsEnabled = "true"
	// 见文件头：本 harness 没有 applier，档位那条键不在冒烟的场景清单里。
	spec.execEnabled = "false"
	spec.level = "info"
	spec.workers = "2"
	spec.retryDelay = "60s"
	spec.historyLimit = "100"
	spec.historyTTL = "0s"
	spec.eventCount = "1000"
	spec.eventAge = "0s"
	spec.auditCount = "1000"
	spec.auditAge = "0s"
	return spec
}

// smokeChange 在**冒烟起点**之上改几项。单元用例那份 specChange 的起点是 baseSpec
// （观测层关着、执行器开着），从那里改出来的候选会顺带改掉本文件不关心的十几条键，
// applied_keys 的期望就再也对不上了，所以本文件另起一份起点。
func smokeChange(tune func(*fileSpec)) fileSpec {
	spec := smokeBaseSpec()
	tune(&spec)
	return spec
}

// newSmokeStack 把 spec 落成配置文件、读回来当 applied，再按 cmd/server 启动时的同一条形状
// 建运行面并把生产重载链接上去。配置文件与 core.Config 出自同一次 LoadConfig，
// 所以不会有"内存里那份与文件里那份分叉"的起点。
func newSmokeStack(t *testing.T, spec fileSpec) *smokeStack {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	stack := &smokeStack{t: t, dir: dir, path: path, logs: &logSink{}}
	stack.write(spec)

	cfg, err := core.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v\n%s", err, spec.render(dir, filepath.Join(dir, "ws"), "node"))
	}
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

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

	// 接链：与 run() 里那一次调用同一份 reloadDeps 形状，只是 applier 这一位空着（见文件头）。
	stack.chain = newReloader(reloadDeps{
		cfgPath:          path,
		applied:          cfg,
		store:            store,
		scheduler:        scheduler,
		events:           events,
		audit:            audit,
		applier:          nil,
		executorsEnabled: cfg.Executors.Enabled,
		levelVar:         levelVar,
		logger:           logger,
	})
	return stack
}

// write 把一份 spec 覆盖到链要读的那个文件上（等价于"运维存了一次盘"）。
func (s *smokeStack) write(spec fileSpec) {
	s.t.Helper()

	executable, err := os.Executable()
	if err != nil {
		s.t.Fatalf("os.Executable: %v", err)
	}
	content := spec.render(s.dir, filepath.Join(s.dir, "ws"), executable)
	if err := os.WriteFile(s.path, []byte(content), 0o600); err != nil {
		s.t.Fatalf("write config: %v", err)
	}
}

// reload 走一次生产链。这里不调 watcher：本文件判的是链与真下游，
// "事件到链"那一段由 R05 的用例与 §5.2 的装配用例分别负责。
func (s *smokeStack) reload() core.ReloadState {
	s.t.Helper()

	state, err := s.chain.Reload()
	if err != nil && state.Result == core.ReloadOK {
		s.t.Fatalf("链交回错误却报成功：%v", err)
	}
	return state
}

// wantApplied 断 applied_keys 恰好是 want 那份（顺序就是 core.Diff 的路径字典序）。
func wantApplied(t *testing.T, state core.ReloadState, want []string) {
	t.Helper()
	assertSameList(t, "applied_keys", state.AppliedKeys, want)
}

// retryPolicyFor 与 cmd/server 启动时同一条构造方式（main.go 用 scheduler.max_retry_delay 建它）。
func retryPolicyFor(cfg core.Config) core.RetryPolicy {
	return &core.ExponentialBackoffRetry{MaxDelay: cfg.Scheduler.MaxRetryDelay}
}

// eightHotKeys 是主冒烟那一次改动应当一次走到的八条热更键路径
// （R02 §10.1 那张表的全部内容：五个入口 + 成对给值的时长/年龄那几条第二位）。
var eightHotKeys = []string{
	"logging.level",
	"observability.audit.retention_age",
	"observability.audit.retention_count",
	"observability.events.retention_age",
	"observability.events.retention_count",
	"scheduler.max_retry_delay",
	"store.history_limit",
	"store.history_ttl",
}

// TestSmokeHotReloadAppliesEveryHotKey 是主冒烟：一次改动把八条热更键全部走一遍，
// 逐个验证**真实下游**的行为真的变了——这是 R01+R02 合起来对外承诺的那件事，
// 从本卡起由生产链兑现（R02 那版是冒烟具自己写的 miniature 链）。
//
// 断 applied_keys 恰好等于这八条，同时就是"链上没有第二条路走到这些下游"的证据：
// 链的分派表与 core 的分档表谁先漂一步，这里就会红（cmd/server 侧的守卫另有
// TestReloadEveryHotKeyHasDispatchEntry，那条判的是静态覆盖，这条判的是真的跑过）。
func TestSmokeHotReloadAppliesEveryHotKey(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())

	// 先把起点跑成可观察的形状：debug 写不出来、留痕四条全在、两张表各四条。
	writeAtLevel(stack, slog.LevelDebug, "hidden at info")
	if strings.Contains(stack.logs.String(), "hidden at info") {
		t.Fatal("冒烟起点不对：info 级别的日志器写出了 debug 记录")
	}
	seedTerminalSnapshots(t, stack, 4)
	seedEvents(t, stack, 4)
	seedAudit(t, stack, 4)

	stack.write(smokeChange(func(s *fileSpec) {
		s.level = "debug"
		s.retryDelay = "5s"
		s.historyLimit = "2"
		s.historyTTL = "24h"
		s.eventCount = "2"
		s.eventAge = "24h"
		s.auditCount = "2"
		s.auditAge = "24h"
	}))

	state := stack.reload()
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（error=%q）", state.Result, core.ReloadOK, state.Error)
	}
	wantApplied(t, state, eightHotKeys)
	if state.LastAppliedAt.IsZero() {
		t.Fatal("真的应用了却不给 LastAppliedAt")
	}

	// 1. 日志级别：下一条记录就按 debug 出，且 info 仍出。
	writeAtLevel(stack, slog.LevelDebug, "debug visible")
	if !strings.Contains(stack.logs.String(), "debug visible") {
		t.Fatalf("改到 debug 之后记录仍被挡在门外：\n%s", stack.logs.String())
	}
	writeAtLevel(stack, slog.LevelInfo, "info visible")
	if !strings.Contains(stack.logs.String(), "info visible") {
		t.Fatal("info 记录在 debug 级别下反而写不出来")
	}

	// 2. 重试上限：调度器上读回来的就是新值（排期行为由 core 包的用例证明，这里只证接得上）。
	if got := stack.scheduler.RetryPolicyMaxDelay(); got != 5*time.Second {
		t.Fatalf("RetryPolicyMaxDelay = %v, want 5s", got)
	}

	// 3. 留痕：起点那四条不该被这次改动顺手剪掉（setter 不动已有数据），下一次写入才用新上限。
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
// 一次改动里混进凭据键时整次作废，本来会生效的键一条都不应用，而被拒的键名要能被读数拿到
// （不能让"被拒"这件事静默）。行为侧的判据才是这条用例的价值：级别与上限都照旧。
func TestSmokeRejectedKeyAbortsWholeReload(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())

	stack.write(smokeChange(func(s *fileSpec) {
		s.token = "rotated-token"
		s.level = "debug"    // 本来会热更
		s.historyLimit = "3" // 本来会热更
	}))

	state := stack.reload()
	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q", state.Result, core.ReloadRejected)
	}
	assertSameList(t, "rejected_keys", state.RejectedKeys, []string{"server.auth.token"})
	if len(state.AppliedKeys) != 0 {
		t.Fatalf("被拒的那次还有 applied_keys = %v", state.AppliedKeys)
	}
	if !strings.Contains(state.Error, "一项都没有应用") {
		t.Fatalf("error 文案没交代什么都没动：%q", state.Error)
	}
	if strings.Contains(state.Error, "rotated-token") {
		t.Fatalf("error 里带出了凭据取值：%q", state.Error)
	}

	// 级别仍是 info：debug 写不出来。
	writeAtLevel(stack, slog.LevelDebug, "should not appear")
	if strings.Contains(stack.logs.String(), "should not appear") {
		t.Fatal("被拒的重载把日志级别也改了")
	}
	// 留痕上限仍是 100：五条都在。
	seedTerminalSnapshots(t, stack, 5)
	requireSnapshotCount(t, stack, 5, "旧的 100 条上限要原样留着")
	// applied 没被推进：链里那份权威仍然对应起点。
	if stack.chain.applied.Server.Auth.Token != "" {
		t.Fatal("失败的重载把 applied 往前推了，违反不变量 I1")
	}
}

// TestSmokeRestartKeysAreReportedNotApplied 守住重启档的可见性（不变量 I3）：
// 一条都不应用，但键名要出现在 ignored_keys 里，而且结论仍是 ok。
func TestSmokeRestartKeysAreReportedNotApplied(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())
	// 起点：调度器按启动期那一份装配（队列通道是 Start 那一刻建的，这里也没 Start）。
	before := stack.scheduler.RuntimeStats()

	// 只改文件，不改装配：两条重启档存一次盘不该换掉任何东西。
	stack.write(smokeChange(func(s *fileSpec) {
		s.port = "9090"
		s.queueCapacity = "88"
	}))

	state := stack.reload()
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want %q（重启档不该把整次判失败；error=%q）",
			state.Result, core.ReloadOK, state.Error)
	}
	if len(state.AppliedKeys) != 0 {
		t.Fatalf("重启档被当成热更应用了：%v", state.AppliedKeys)
	}
	assertSameList(t, "ignored_keys", state.IgnoredKeys,
		[]string{"scheduler.queue_capacity", "server.port"})
	if !state.LastAppliedAt.IsZero() {
		t.Fatalf("一个落点都没动却给了 LastAppliedAt = %v", state.LastAppliedAt)
	}
	// 真下游对照：并发与队列容量都不因为存了一次盘而变（监听中的服务不是链能换的东西）。
	after := stack.scheduler.RuntimeStats()
	if after.QueueCapacity != before.QueueCapacity || after.Workers != before.Workers {
		t.Fatalf("重启档改到了运行面上：%+v → %+v", before, after)
	}
}

// TestSmokeUnchangedConfigIsNotAReload 守住"文件被 touch 但内容没变"不该算一次重载：
// 判据是 core.Diff，而不是文件事件或内容哈希。连着走两次同一份文件，第二次必须 unchanged。
func TestSmokeUnchangedConfigIsNotAReload(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())
	stack.write(smokeChange(func(s *fileSpec) { s.level = "debug" }))

	if first := stack.reload(); first.Result != core.ReloadOK {
		t.Fatalf("起点那次应当真的应用一条：%+v", first)
	}
	// 文件一个字都没再改，只是又存了一次盘（等价于 touch）：不算一次重载。
	second := stack.reload()
	if second.Result != core.ReloadUnchanged {
		t.Fatalf("result = %q, want %q", second.Result, core.ReloadUnchanged)
	}
	if len(second.AppliedKeys) != 0 || len(second.IgnoredKeys) != 0 {
		t.Fatalf("unchanged 那次不该有任何键：%+v", second)
	}
	if !second.LastAppliedAt.IsZero() {
		t.Fatalf("unchanged 不该推进 LastAppliedAt：%v", second.LastAppliedAt)
	}
}

// TestSmokeOverBoundWorkersRollBackEveryRealTarget 是 R02 那版冒烟写不出来的一条：
// 链在半路失败，而失败点在八个落点中间。生产链这时必须逆序把**已经落到真下游上的**
// 每一项写回旧值——留痕上限、两张表的条数、日志级别都各归其位，applied 不前推。
//
// 选的失败点是 scheduler.workers 的荒谬取值（链上的上界判定拦住它，它自己在 Validate 之后）：
// 排在前面的 #1/#4/#5 都已真的改到现网，排在后面的 #7 压根不该被碰。
func TestSmokeOverBoundWorkersRollBackEveryRealTarget(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())

	// 起点先跑成已知形状：debug 写不出来，两张表各四条（新上限 2 会把它们剪成两条，
	// 所以"回滚成功"在这三处都读得出来）。
	seedTerminalSnapshots(t, stack, 4)
	seedEvents(t, stack, 4)
	seedAudit(t, stack, 4)

	stack.write(smokeChange(func(s *fileSpec) {
		s.level = "debug"
		s.retryDelay = "5s"
		s.historyLimit = "2"
		s.historyTTL = "24h"
		s.eventCount = "2"
		s.eventAge = "24h"
		s.auditCount = "2"
		s.auditAge = "24h"
		s.workers = "100000"
	}))

	state := stack.reload()
	if state.Result != core.ReloadFailed {
		t.Fatalf("result = %q, want %q（applied=%v error=%q）",
			state.Result, core.ReloadFailed, state.AppliedKeys, state.Error)
	}
	if !strings.Contains(state.Error, "上界") {
		t.Fatalf("error 没说是上界拦住的：%q", state.Error)
	}
	if len(state.AppliedKeys) != 0 {
		t.Fatalf("失败那次的 applied_keys = %v，那些已被回滚", state.AppliedKeys)
	}

	// 回滚之后的真实现场，一项一项读：
	// 1. 级别回到 info（debug 写不出来，info 仍写得出）。
	stack.logs.Reset()
	writeAtLevel(stack, slog.LevelDebug, "must stay hidden")
	if strings.Contains(stack.logs.String(), "must stay hidden") {
		t.Fatal("回滚没把日志级别写回去")
	}
	writeAtLevel(stack, slog.LevelInfo, "info still works")
	if !strings.Contains(stack.logs.String(), "info still works") {
		t.Fatalf("回滚把级别载体写坏了：\n%s", stack.logs.String())
	}
	// 2. 重试上限回到 60s。
	if got := stack.scheduler.RetryPolicyMaxDelay(); got != 60*time.Second {
		t.Fatalf("RetryPolicyMaxDelay = %v, want 回到 60s", got)
	}
	// 3. 留痕上限回到 100：起点那四条一条都没被剪（上限换成 2 之后没有新的写入来触发淘汰），
	//    再写五条也全留着——回滚没发生的话这里只剩 2 条。
	for i := 0; i < 5; i++ {
		writeTerminalSnapshot(t, stack, fmt.Sprintf("after-rollback-%d", i))
	}
	requireSnapshotCount(t, stack, 9, "留痕上限要跟着回滚回到 100")
	// 4. 事件表回到 1000 条：再落一次盘仍剩全部（没回去的话 prune 会剪到 2）。
	publishEvent(t, stack, "event-after-rollback")
	requireTableCount(t, stack.events, 5, "事件表回到 1000/0")
	// 5. 台账同上。
	appendAuditRow(t, stack)
	requireTableCount(t, stack.audit, 5, "台账回到 1000/0")

	// applied 没有被推进：下一次重载仍然把这八条当成待应用的改动。
	if stack.chain.applied.Logging.Level != "info" {
		t.Fatalf("失败那次推进了 applied：level = %q", stack.chain.applied.Logging.Level)
	}
}

// TestSmokeBrokenConfigKeepsTheRunningValues 证明"新配置读不进来"是一次可报告、
// 可自愈的失败，而不是"什么都没发生"：一份解码不了的配置（往 logging 一节里塞一个不认识的键，
// LoadConfig 的 UnmarshalExact 会拒）给出 result=rejected 与一条含路径的 error，
// 运行面继续按上一次成功的配置工作；把文件修好，下一个窗口内不需要任何人干预就自动恢复。
func TestSmokeBrokenConfigKeepsTheRunningValues(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())

	// 起点先把级别热更到 debug，再喂坏文件——这样"仍按改过的配置工作"才是判据，
	// 而不是无动作可推翻的起点复述。
	stack.write(smokeChange(func(s *fileSpec) { s.level = "debug" }))
	if first := stack.reload(); first.Result != core.ReloadOK {
		t.Fatalf("起点热更失败：%+v", first)
	}
	stack.logs.Reset()
	writeAtLevel(stack, slog.LevelDebug, "applied before the broken file")
	if !strings.Contains(stack.logs.String(), "applied before the broken file") {
		t.Fatal("起点没把级别改到 debug，后面的判据就没有对照")
	}

	if err := os.WriteFile(stack.path, []byte("logging:\n  level_not_a_key: debug\n"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}
	stack.logs.Reset()
	state := stack.reload()

	if state.Result != core.ReloadRejected {
		t.Fatalf("result = %q, want %q（坏文件是整次作废，不是半套）", state.Result, core.ReloadRejected)
	}
	if !strings.Contains(state.Error, "level_not_a_key") {
		t.Fatalf("error 没带上配置层的原文，运维看不出是哪一次读坏：%q", state.Error)
	}
	if !strings.Contains(state.Error, "现网继续按当前生效的取值运行") {
		t.Fatalf("error 没说清现网怎么样：%q", state.Error)
	}
	if !strings.Contains(stack.logs.String(), "level=ERROR") {
		t.Fatalf("坏文件没记 error 级日志：\n%s", stack.logs.String())
	}

	// 现网继续按上一次成功的配置工作：debug 仍写得出来。
	stack.logs.Reset()
	writeAtLevel(stack, slog.LevelDebug, "still debug")
	if !strings.Contains(stack.logs.String(), "still debug") {
		t.Fatal("坏配置把已经生效的级别带跑了")
	}
	// 留痕上限仍是起点的 100：五条都留着。
	seedTerminalSnapshots(t, stack, 5)
	requireSnapshotCount(t, stack, 5, "坏配置之后旧的留痕上限要原样留着")
	if stack.chain.applied.Logging.Level != "debug" {
		t.Fatalf("applied 被坏配置改写了：level = %q", stack.chain.applied.Logging.Level)
	}

	// 修好文件即自动恢复（设计文档 §8 那一条）：不需要任何人干预，下一次就成功。
	stack.write(smokeChange(func(s *fileSpec) {
		s.level = "debug"
		s.retryDelay = "9s"
	}))
	recovered := stack.reload()
	if recovered.Result != core.ReloadOK {
		t.Fatalf("修好文件之后没有自动恢复：%+v", recovered)
	}
	assertSameList(t, "恢复那次的 applied_keys", recovered.AppliedKeys, []string{"scheduler.max_retry_delay"})
	if got := stack.scheduler.RetryPolicyMaxDelay(); got != 9*time.Second {
		t.Fatalf("RetryPolicyMaxDelay = %v, want 9s", got)
	}
}

// TestSmokeEnvOverrideReachesTheSameEntries 守住设计 §5.3 的口径：
// 环境变量覆盖与文件改动走的是同一条应用路径（链每读一次都会重新绑 GODELAYQ_*）。
func TestSmokeEnvOverrideReachesTheSameEntries(t *testing.T) {
	stack := newSmokeStack(t, smokeBaseSpec())

	t.Setenv("GODELAYQ_LOGGING_LEVEL", "debug")
	t.Setenv("GODELAYQ_SCHEDULER_MAX_RETRY_DELAY", "7s")

	// 文件一个字都没改：这一次变化全部来自环境变量。
	state := stack.reload()
	if state.Result != core.ReloadOK {
		t.Fatalf("result = %q, want ok（error=%q）", state.Result, state.Error)
	}
	for _, key := range []string{"logging.level", "scheduler.max_retry_delay"} {
		if !containsKey(state.AppliedKeys, key) {
			t.Fatalf("applied_keys = %v，少了 %q", state.AppliedKeys, key)
		}
	}
	if got := stack.scheduler.RetryPolicyMaxDelay(); got != 7*time.Second {
		t.Fatalf("RetryPolicyMaxDelay = %v, want 7s", got)
	}
	writeAtLevel(stack, slog.LevelDebug, "from env")
	if !strings.Contains(stack.logs.String(), "from env") {
		t.Fatal("环境变量覆盖的级别没有落到运行面")
	}

	// 环境变量这一层压住之后，文件里的同一处改动就读成无变化——设计 §5.3 与待拍板 P4 说的那件事。
	stack.write(smokeChange(func(s *fileSpec) {
		s.level = "debug"
		s.retryDelay = "7s"
	}))
	if again := stack.reload(); again.Result != core.ReloadUnchanged {
		t.Fatalf("环境变量压住的键被算成了一次改动：%+v", again)
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
