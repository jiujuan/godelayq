package executor

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"godelayq/core"
)

// 这一组用例守的是设计文档的 I3：任何一次读取拿到的表必须自洽。
// 登记表从"构造后不变"改成"运行期整表替换"，读侧的旧承诺就换成了一条更强的约束——
// 每个方法只做一次 Load，读者永远看不到跨两张表的结果。

// newConfigRegistry 造一张只含 config 来源档位的登记表。
// 脚本文件并不存在，探测结论是"不可用"——这正好也证明降级与不可用是两件事。
func newConfigRegistry(t *testing.T, names ...string) *Registry {
	t.Helper()

	ec := core.DefaultConfig().Executors
	ec.Enabled = true
	ec.Workspace = t.TempDir()
	for _, name := range names {
		ec.Commands = append(ec.Commands, core.ExecutorCommand{
			Name: name, Kind: "script", Runtime: "node", Script: "scripts/" + name + ".mjs",
		})
	}

	registry, err := NewRegistry(core.Config{Executors: ec}, nil)
	if err != nil {
		t.Fatalf("NewRegistry 失败: %v", err)
	}
	return registry
}

func storeProfile(name string, marker time.Duration) *Profile {
	return &Profile{Name: name, Kind: KindScript, Timeout: marker}
}

func TestApplyStore_MergesWithConfigAndReplacesStoreSide(t *testing.T) {
	registry := newConfigRegistry(t, "cfg_one")

	if err := registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("store_a", time.Minute)},
		{Profile: storeProfile("store_b", 2*time.Minute), Probe: ProbeResult{Available: true, Path: "node"}},
	}); err != nil {
		t.Fatalf("ApplyStore 失败: %v", err)
	}

	keys := registry.Keys()
	want := []string{"exec.cfg_one", "exec.store_a", "exec.store_b"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("Keys 应是三档且按字典序，实际 %v", keys)
	}

	if source, ok := registry.SourceOf("exec.cfg_one"); !ok || source != SourceConfig {
		t.Errorf("配置侧的来源应是 config，实际 %q ok=%v", source, ok)
	}
	if source, ok := registry.SourceOf("exec.store_a"); !ok || source != SourceStore {
		t.Errorf("文件侧的来源应是 store，实际 %q ok=%v", source, ok)
	}
	if _, ok := registry.SourceOf("exec.nope"); ok {
		t.Error("不存在的键不该有来源")
	}

	// 整批替换：第二批里没有 store_a，它就消失；store_b 带着新的探测结论回来
	if err := registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("store_b", 3*time.Minute), Probe: ProbeResult{Reason: "runtime \"node\" not found in PATH"}},
		{Profile: storeProfile("store_c", 4*time.Minute)},
	}); err != nil {
		t.Fatalf("第二次 ApplyStore 失败: %v", err)
	}
	if _, ok := registry.Lookup("exec.store_a"); ok {
		t.Error("整批替换后，上一批里没被重新给的 store 档位必须消失")
	}
	if _, ok := registry.Lookup("exec.cfg_one"); !ok {
		t.Error("config 来源的条目不该因为一次 store 替换而消失")
	}
	reason, available := registry.Available("exec.store_b")
	if available {
		t.Error("store_b 的新探测结论应是不可用")
	}
	if !strings.Contains(reason, "node") {
		t.Errorf("不可用的原因应来自新那批，实际 %q", reason)
	}

	// 幂等：同一批再来一次，结果不变
	before := strings.Join(registry.Keys(), ",")
	if err := registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("store_b", 3*time.Minute), Probe: ProbeResult{Reason: "runtime \"node\" not found in PATH"}},
		{Profile: storeProfile("store_c", 4*time.Minute)},
	}); err != nil {
		t.Fatalf("重复 ApplyStore 失败: %v", err)
	}
	if after := strings.Join(registry.Keys(), ","); after != before {
		t.Errorf("同一批重复应用应幂等：%q → %q", before, after)
	}
}

func TestApplyStore_RejectsBadBatchesAndKeepsOldTable(t *testing.T) {
	t.Run("同批内重复键", func(t *testing.T) {
		registry := newConfigRegistry(t, "cfg_one")
		if err := registry.ApplyStore([]StoreEntry{
			{Profile: storeProfile("dup", time.Minute)},
			{Profile: storeProfile("dup", 2*time.Minute)},
		}); err == nil {
			t.Fatal("同一批里重复的注册键必须被拒")
		}
		if len(registry.Keys()) != 1 {
			t.Errorf("失败的那批不该改动表，实际 %v", registry.Keys())
		}
	})

	t.Run("空档位", func(t *testing.T) {
		registry := newConfigRegistry(t, "cfg_one")
		if err := registry.ApplyStore([]StoreEntry{{Profile: nil}}); err == nil {
			t.Fatal("没有 Profile 的条目必须被拒")
		}
		if len(registry.Keys()) != 1 {
			t.Errorf("失败的那批不该改动表，实际 %v", registry.Keys())
		}
	})

	t.Run("执行器关闭", func(t *testing.T) {
		registry, err := NewRegistry(core.Config{Executors: core.ExecutorsConfig{Enabled: false}}, nil)
		if err != nil {
			t.Fatalf("NewRegistry 失败: %v", err)
		}
		if len(registry.Keys()) != 0 {
			t.Fatalf("关闭时表必须是空的，实际 %v", registry.Keys())
		}
		if err := registry.ApplyStore([]StoreEntry{{Profile: storeProfile("late", time.Minute)}}); err == nil {
			t.Fatal("关闭的登记表不该接受 store 条目：那会造出接口看得见、调度器跑不了的半状态")
		}
		if len(registry.Keys()) != 0 {
			t.Errorf("被拒之后表仍是空的，实际 %v", registry.Keys())
		}
	})
}

// 撞名是唯一允许"两条档位共用一个注册键"的情形，而它的处理方向是固定的：
// config 那条继续生效，store 那条降级到只看得见。反过来（页面顶掉配置）违反设计文档 D4。
func TestApplyStore_CollisionWithConfigDegradesStoreSide(t *testing.T) {
	registry := newConfigRegistry(t, "clash")

	t.Run("没标降级就拒", func(t *testing.T) {
		err := registry.ApplyStore([]StoreEntry{{Profile: storeProfile("clash", time.Minute)}})
		if err == nil {
			t.Fatal("与 config 撞名又没标 Degraded 的条目必须被拒")
		}
		if !strings.Contains(err.Error(), "exec.clash") {
			t.Errorf("错误信息应指名撞的键，实际 %v", err)
		}
		if len(registry.Degraded()) != 0 {
			t.Errorf("被拒的那批不该留下降级条目，实际 %d 条", len(registry.Degraded()))
		}
	})

	t.Run("标了降级就只进展示面", func(t *testing.T) {
		if err := registry.ApplyStore([]StoreEntry{{
			Profile: storeProfile("clash", time.Minute), Degraded: true,
		}}); err != nil {
			t.Fatalf("降级条目应被接受: %v", err)
		}
		if len(registry.Keys()) != 1 {
			t.Errorf("降级条目不该进注册表，实际键 %v", registry.Keys())
		}
		if len(registry.Profiles()) != 1 {
			t.Errorf("降级条目不该出现在 Profiles() 里（提交它等于提交一个没处理函数的类型），实际 %d 条", len(registry.Profiles()))
		}
		profile, ok := registry.Lookup("exec.clash")
		if !ok {
			t.Fatal("撞名之后 config 那条必须仍然查得到")
		}
		// 生效的是配置那条：它的超时是归一化后的全局默认，而不是 store 侧的 1 分钟标记
		if profile.Timeout != core.DefaultExecDefaultTimeout {
			t.Errorf("查到的应是 config 那条（timeout 走全局默认），实际 %v", profile.Timeout)
		}

		degraded := registry.Degraded()
		if len(degraded) != 1 {
			t.Fatalf("应有 1 条降级档位，实际 %d", len(degraded))
		}
		if degraded[0].Profile.Name != "clash" || degraded[0].Profile.Timeout != time.Minute {
			t.Errorf("降级条目应是 store 那一条，实际 %+v", degraded[0].Profile)
		}
		if !strings.Contains(degraded[0].Reason, "executors.commands") {
			t.Errorf("降级原因要直接说得清冲突来源，实际 %q", degraded[0].Reason)
		}

		// 自定义原因优先
		if err := registry.ApplyStore([]StoreEntry{{
			Profile: storeProfile("clash", 2*time.Minute), Degraded: true, Reason: "自定义的降级说明",
		}}); err != nil {
			t.Fatalf("再次降级失败: %v", err)
		}
		if got := registry.Degraded()[0].Reason; got != "自定义的降级说明" {
			t.Errorf("调用方给的原因应原样透出，实际 %q", got)
		}
	})
}

// Profiles()/Degraded() 返回的是副本：调用方排序或截断不该动到表。
func TestRegistryReadsReturnCopies(t *testing.T) {
	registry := newConfigRegistry(t, "b_one", "a_two")
	// 降级只在撞名时发生，所以这里用配置里已有的名字
	if err := registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("b_one", time.Minute), Degraded: true},
	}); err != nil {
		t.Fatalf("ApplyStore 失败: %v", err)
	}

	keys := registry.Keys()
	if strings.Join(keys, ",") != "exec.a_two,exec.b_one" {
		t.Fatalf("Keys 应是排好序的两条，实际 %v", keys)
	}
	keys[0] = "tampered"
	if registry.Keys()[0] != "exec.a_two" {
		t.Error("Keys 应返回副本，改动它不该影响登记表")
	}

	profiles := registry.Profiles()
	if len(profiles) != 2 {
		t.Fatalf("应有 2 条生效档位，实际 %d", len(profiles))
	}
	profiles[0] = nil
	if registry.Profiles()[0] == nil {
		t.Error("Profiles 应返回副本切片")
	}

	degraded := registry.Degraded()
	if len(degraded) != 1 {
		t.Fatalf("应有 1 条降级档位，实际 %d", len(degraded))
	}
	degraded[0].Reason = "tampered"
	if registry.Degraded()[0].Reason == "tampered" {
		t.Error("Degraded 应返回副本切片")
	}
}

// 设计文档 I3 的现场证明：写侧每轮换一批，读侧一次调用里看到的所有 store 档位
// 必须来自**同一轮**（用 Timeout 当轮次标记）。
//
// 这条用例在"先删后加"的实现下会失败——那正是它存在的理由（DoD 要求做反向验证）。
func TestRegistryReaderAlwaysSeesOneSelfConsistentTable(t *testing.T) {
	registry := newConfigRegistry(t, "cfg_one")
	const rounds = 120

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧：8 个 goroutine 反复读整表
	var readFailures int64
	var failureMu sync.Mutex
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				profiles := registry.Profiles()
				round := time.Duration(-1)
				for _, profile := range profiles {
					if !strings.HasPrefix(profile.Name, "store_") {
						continue
					}
					if round == time.Duration(-1) {
						round = profile.Timeout
						continue
					}
					if profile.Timeout != round {
						failureMu.Lock()
						readFailures++
						failureMu.Unlock()
						return
					}
				}
				// 键与表也必须来自同一份：store 档位数要么是 2（每轮都给全），要么是 0（没给）
				storeKeys := 0
				for _, key := range registry.Keys() {
					if strings.HasPrefix(key, "exec.store_") {
						storeKeys++
					}
				}
				if storeKeys != 0 && storeKeys != 2 {
					failureMu.Lock()
					readFailures++
					failureMu.Unlock()
					return
				}
			}
		}()
	}

	// 写侧：整批替换 120 轮
	for round := 1; round <= rounds; round++ {
		marker := time.Duration(round) * time.Second
		items := []StoreEntry{
			{Profile: storeProfile("store_left", marker)},
			{Profile: storeProfile("store_right", marker)},
		}
		if err := registry.ApplyStore(items); err != nil {
			t.Fatalf("第 %d 轮 ApplyStore 失败: %v", round, err)
		}
		if err := registry.ApplyStore(nil); err != nil {
			t.Fatalf("第 %d 轮清空失败: %v", round, err)
		}
		items[0].Profile = storeProfile("store_left", marker)
		items[1].Profile = storeProfile("store_right", marker)
		if err := registry.ApplyStore(items); err != nil {
			t.Fatalf("第 %d 轮重填失败: %v", round, err)
		}
	}

	close(stop)
	wg.Wait()

	if failures := readFailures; failures > 0 {
		t.Errorf("读到过跨轮的半成品表 %d 次，整表替换的自洽性没保住", failures)
	}
}

// 并发下所有读方法都不该触发 data race（-race 跑这条）。
func TestRegistryConcurrentReadsWithWrites(t *testing.T) {
	registry := newConfigRegistry(t, "cfg_one", "cfg_two")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				registry.Profiles()
				registry.Keys()
				registry.Lookup("exec.cfg_one")
				registry.ProbeOf("exec.cfg_two")
				registry.Available("exec.cfg_one")
				registry.SourceOf("exec.cfg_two")
				registry.Degraded()
				registry.InlinePreview()
				registry.MaxTimeout()
			}
		}()
	}

	for round := 0; round < 60; round++ {
		items := []StoreEntry{
			{Profile: storeProfile("cfg_one", time.Duration(round)*time.Second), Degraded: true},
			{Profile: storeProfile("rolling", time.Duration(round)*time.Second)},
		}
		if err := registry.ApplyStore(items); err != nil {
			t.Fatalf("第 %d 轮失败: %v", round, err)
		}
	}

	close(stop)
	wg.Wait()
}

// Register 的启动日志要说出"有几条白写在文件里"（TASK-W03 §4.5）。
func TestRegisterCountsDegradedProfiles(t *testing.T) {
	registry := newConfigRegistry(t, "clash")
	if err := registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("clash", time.Minute), Degraded: true},
		{Profile: storeProfile("extra", time.Minute)},
	}); err != nil {
		t.Fatalf("ApplyStore 失败: %v", err)
	}

	artifacts, err := NewArtifactStore(ArtifactOptions{
		Dir: filepath.Join(t.TempDir(), "exec"), MaxBytes: 4096,
	}, quietLogger())
	if err != nil {
		t.Fatalf("NewArtifactStore 失败: %v", err)
	}

	registrar := newFakeRegistrar()
	result, err := Register(registrar, registry, core.Config{Executors: registry.executors}, artifacts, quietLogger())
	if err != nil {
		t.Fatalf("Register 失败: %v", err)
	}

	if result.Total != 2 || result.Registered != 2 {
		t.Errorf("生效档位应是 2 条（clash + extra），实际 %+v", result)
	}
	if result.Degraded != 1 {
		t.Errorf("降级计数应是 1，实际 %d", result.Degraded)
	}
	if keys := strings.Join(registrar.registeredKeys(), ","); keys != "exec.clash,exec.extra" {
		t.Errorf("注册的键应是生效那两条，实际 %q", keys)
	}
}

func TestRegistryDegradedSortedByKey(t *testing.T) {
	registry := newConfigRegistry(t, "zzz", "aaa")
	if err := registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("zzz", time.Minute), Degraded: true},
		{Profile: storeProfile("aaa", time.Minute), Degraded: true},
	}); err != nil {
		t.Fatalf("ApplyStore 失败: %v", err)
	}

	items := registry.Degraded()
	if len(items) != 2 {
		t.Fatalf("应有 2 条降级档位，实际 %d", len(items))
	}
	if items[0].Profile.Name != "aaa" || items[1].Profile.Name != "zzz" {
		t.Errorf("Degraded 应按注册键字典序，实际 %s, %s", items[0].Profile.Name, items[1].Profile.Name)
	}
}
