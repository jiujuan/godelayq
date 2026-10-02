package executor

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组用例守的是 Applier.ApplyConfig（TASK-R04 §5.2–§5.5）：
// 它把 executors.commands 的新列表换进登记表与调度器，失败点全部在动手之前，
// 摘除只看 exec. 前缀、两批全部重登记，而且新增/改名一条档位只能在冻结许可之内。

// configFromExec 用给定的 executors 取值（非 Commands 部分）+ 新命令列表拼一份 core.Config：
// 这是"绕过 configWithCommands、要手工塞越界/改时命令"的用例的公共构造点。
func configFromExec(exec core.ExecutorsConfig, commands ...core.ExecutorCommand) core.Config {
	exec.Commands = append([]core.ExecutorCommand(nil), commands...)
	return core.Config{Executors: exec}
}

// --- §5.2 基本行为 ---

// 新增一条 config 档位 → 替身里出现 exec.<新名>、类别是 JobClassExec、Added 含它、Config 计数 +1。
func TestApplier_ApplyConfig_AddsNewProfile(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_a") // 启动 config 侧只有 cfg_a，且已预登记处理函数

	res, err := f.applier.ApplyConfig(f.configWithCommands(t, "cfg_a", "cfg_b"))
	require.NoError(t, err)

	assert.Contains(t, res.Added, "exec.cfg_b")
	assert.Empty(t, res.Removed, "既有键没被删就不该有摘除")
	assert.Equal(t, 2, res.Config, "cfg_a + cfg_b 都是 config 来源")
	assert.Zero(t, res.Stored, "这一批里没有 store 条目")

	_, ok := f.syncer.LookupHandler("exec.cfg_b")
	require.True(t, ok, "新 config 档位要立刻在调度器里可用")
	class, ok := f.syncer.classOf("exec.cfg_b")
	require.True(t, ok)
	assert.Equal(t, core.JobClassExec, class, "运行期新加的 config 键在类别上回 JobClassExec（§3.4 第 2 条）")
}

// 删掉一条 config 档位 → 那个键被摘掉；普通任务键（不带 exec. 前缀的 payment_check）不被触碰。
func TestApplier_ApplyConfig_RemovesDroppedProfileAndLeavesOrdinaryKeys(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_a", "cfg_b")

	res, err := f.applier.ApplyConfig(f.configWithCommands(t, "cfg_a"))
	require.NoError(t, err)

	assert.Contains(t, res.Removed, "exec.cfg_b")
	assert.Empty(t, res.Added)
	_, ok := f.syncer.LookupHandler("exec.cfg_b")
	assert.False(t, ok, "被删掉的 config 档位要连处理函数一起摘除")
	_, ok = f.syncer.classOf("exec.cfg_b")
	assert.False(t, ok, "摘除必须成对删类别")

	// 普通任务键的既有承诺：摘除判据含 core.ExecPrefix，代码注册的键永不在这条路径上被动到。
	_, ok = f.syncer.LookupHandler("payment_check")
	assert.True(t, ok, "普通任务键不得因为一次 config 摘除而被删除")
}

// 改一条 config 档位的 timeout → 键位不变、Added/Removed 都空，但闭包被重新登记过（一次）。
func TestApplier_ApplyConfig_ReregistersChangedProfile(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_a")

	changed := namedScript(t, f.workspace, "cfg_a")
	changed.Timeout = 7 * time.Minute

	f.syncer.writes = nil // 只观测这一次 ApplyConfig 的写入
	res, err := f.applier.ApplyConfig(configFromExec(f.executor, changed))
	require.NoError(t, err)

	assert.Empty(t, res.Added)
	assert.Empty(t, res.Removed, "键位没变，既不算新增也不算摘除")
	assert.Equal(t, []string{"exec.cfg_a"}, f.syncer.writeLog(),
		"改的是同一条档位的闭包内容，整批重登记会写它一次（不是键位可推出来的增减）")

	profile, ok := f.registry.Lookup("exec.cfg_a")
	require.True(t, ok)
	assert.Equal(t, 7*time.Minute, profile.Timeout, "生效表与调度器拿到的必须是新的一条")
}

// 新列表里有一条非法（脚本路径越出冻结 workspace）→ 返回错误，登记表/替身/记账一字未动；
// 并且后续 Apply() 的撞名判定仍按旧列表——那是记账没被推进的唯一证据。
func TestApplier_ApplyConfig_IllegalEntryLeavesEverythingAlone(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_a")

	// 一条越界的命令：脚本落在另一个临时目录里（frozen workspace 之外），
	// 但它与同批里那条合法的 gone 一起，被 LoadProfiles 的连坐规则整批拒绝。
	outside := t.TempDir()
	rel := declareFile(t, outside, "scripts/outside.mjs")
	abs := filepath.Join(outside, filepath.FromSlash(rel))
	illegal := core.ExecutorCommand{Name: "outside", Kind: "script", Runtime: selfExecutable(t), Script: abs}

	beforeKeys := f.registry.Keys()
	beforeHandlers := f.syncer.registeredKeys()
	beforeCommands := len(f.applier.configCommands)

	candidate := configFromExec(f.executor,
		namedScript(t, f.workspace, "cfg_a"),
		namedScript(t, f.workspace, "gone"), // 本身合法，但同批有非法项 → 整批拒
		illegal)

	_, err := f.applier.ApplyConfig(candidate)
	require.Error(t, err, "越出冻结 workspace 的一条必须让整次失败")

	assert.Equal(t, beforeKeys, f.registry.Keys(), "登记表一字未动")
	assert.Equal(t, beforeHandlers, f.syncer.registeredKeys(), "调度器一字未动")
	assert.Equal(t, beforeCommands, len(f.applier.configCommands), "记账停在旧列表")

	_, ok := f.syncer.LookupHandler("exec.gone")
	assert.False(t, ok, "合法那条也因连坐没进来")

	// 后续 Apply 的撞名判定按旧列表：gone 不在旧 config 里，所以文件里的 gone 是活的 store 条目。
	// 若记账被错误推进到含 gone 的新列表，store 那条就会被判撞名而降级——这里断言它没降级。
	f.save(t, "gone")
	res, err := f.applier.Apply()
	require.NoError(t, err)
	assert.Contains(t, res.Added, "exec.gone", "旧列表里没有 gone，所以文件里那条正常登记为 store")
	assert.Equal(t, SourceStore, mustSourceOf(t, f.registry, "exec.gone"))
}

// enabled=false → 返回错误且不碰任何东西（登记表空、调度器不动）。
func TestApplier_ApplyConfig_RejectsWhenExecutorsDisabled(t *testing.T) {
	closed, err := NewRegistry(core.DefaultConfig(), quietLogger())
	require.NoError(t, err)
	require.False(t, closed.Enabled())

	syncer := newFakeRegistrar()
	applier, err := NewConfigApplier(syncer, closed, nil, quietLogger())
	require.NoError(t, err)

	// candidate 内容无所谓：步骤 1 在合成配置与 LoadProfiles 之前就按 enabled 退出了。
	_, err = applier.ApplyConfig(core.Config{Executors: core.ExecutorsConfig{
		Commands: []core.ExecutorCommand{{Name: "late", Kind: "script", Runtime: "node", Script: "scripts/late.mjs"}},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executors are disabled")

	assert.Empty(t, closed.Keys(), "被拒之后仍是空表，不出现接口看得见、调度器跑不了的半状态")
	assert.Empty(t, syncer.registeredKeys(), "被拒之后不碰调度器")
}

// 免重启新增档位被限定在既有许可之内（设计文档 §9 兜底在 R04）：
// candidate 同时放宽顶层 workspace 与 runtime_allow，并新增一条只有按放宽 runtime_allow 才合法的档位；
// ApplyConfig 用冻结值校验，整次失败、一字不动。这条守的是"热更不能扩边界"。
//
// 判据取 runtime 这一维而不是 script 路径越界：LoadProfiles 走 PathWithinWorkspace，
// 严格模式无条件拒绝绝对路径与 ".."（resolveInside 的前两层），所以"相对 workspace 越界"
// 换 workspace 也换不出来——能随顶层取值放宽而改变合法性的只有 runtime_allow。
// 顶层 workspace 一并放宽只是为了证明它压根没被用来做校验。
func TestApplier_ApplyConfig_ValidatesAgainstFrozenPermissions(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_a")

	// far_node 用一个不在冻结白名单里的解释器、脚本是 f.workspace 内的普通相对路径：
	// 只有当 runtime_allow 放宽到含它才合法（脚本本身两种 workspace 下都在根内）。
	farScript := declareFile(t, f.workspace, "scripts/far.mjs")
	farNode := core.ExecutorCommand{Name: "far_node", Kind: "script", Runtime: missingProgram, Script: farScript}

	elsewhere := t.TempDir()
	relaxedExec := f.executor // 从冻结值出发，再放宽顶层许可
	relaxedExec.Workspace = elsewhere
	relaxedExec.RuntimeAllow = append(append([]string{}, f.executor.RuntimeAllow...), missingProgram)
	candidate := configFromExec(relaxedExec, namedScript(t, f.workspace, "cfg_a"), farNode)

	// 先自证放宽值确实能过 LoadProfiles，否则"失败只可能来自用冻结值"就不成立。
	_, relaxedErr := LoadProfiles(candidate)
	require.NoError(t, relaxedErr, "整份 candidate（放宽值）本身应当能加载，否则测不到冻结值这条兜底")

	beforeKeys := f.registry.Keys()
	beforeHandlers := f.syncer.registeredKeys()
	beforeCommands := len(f.applier.configCommands)

	_, err := f.applier.ApplyConfig(candidate)
	require.Error(t, err, "far_node 的 runtime 不在冻结 runtime_allow 里，必须整次失败")
	assert.Contains(t, err.Error(), "far_node")

	assert.Equal(t, beforeKeys, f.registry.Keys())
	assert.Equal(t, beforeHandlers, f.syncer.registeredKeys())
	assert.Equal(t, beforeCommands, len(f.applier.configCommands))
	_, ok := f.syncer.LookupHandler("exec.far_node")
	assert.False(t, ok, "即使 YAML 顶层许可已放宽，新档位也不能按新值建出来")
}

// 连续两次 ApplyConfig 同一份列表 → 第二次增减全空、每个键只被重登记一次（可重入）。
func TestApplier_ApplyConfig_IsReentrant(t *testing.T) {
	f := newConfigApplierFixture(t)
	candidate := f.configWithCommands(t, "x", "y")

	f.syncer.writes = nil
	first, err := f.applier.ApplyConfig(candidate)
	require.NoError(t, err)
	assert.Equal(t, []string{"exec.x", "exec.y"}, first.Added)

	f.syncer.writes = nil
	second, err := f.applier.ApplyConfig(candidate)
	require.NoError(t, err)
	assert.Empty(t, second.Added)
	assert.Empty(t, second.Removed)
	assert.ElementsMatch(t, []string{"exec.x", "exec.y"}, f.syncer.writeLog(),
		"每个键只被重登记一次，不多不少")
}

// --- §5.3 两批共存时的判定 ---

func TestApplier_ApplyConfig_CoexistsWithStoreSide(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_one")
	f.save(t, "page_one") // store 那条走默认 timeout
	_, err := f.applier.Apply()
	require.NoError(t, err)
	require.Equal(t, SourceStore, mustSourceOf(t, f.registry, "exec.page_one"))

	// 1. config 加一条与 page_one 同名（timeout 取得可区分）的档位 → config 赢、store 那条降级。
	clashing := namedScript(t, f.workspace, "page_one")
	clashing.Timeout = 7 * time.Minute
	res1, err := f.applier.ApplyConfig(configFromExec(f.executor,
		namedScript(t, f.workspace, "cfg_one"), clashing))
	require.NoError(t, err)

	assert.Equal(t, []string{"exec.cfg_one", "exec.page_one"}, f.registry.Keys())
	assert.Equal(t, SourceConfig, mustSourceOf(t, f.registry, "exec.page_one"), "撞名时 config 赢：键的来源从此是 config")
	won, _ := f.registry.Lookup("exec.page_one")
	require.NotNil(t, won)
	assert.Equal(t, 7*time.Minute, won.Timeout,
		"生效表里的是 config 那条（用 timeout 与 store 那条区分），改 store 那条的 timeout 不影响这里")

	degraded := f.registry.Degraded()
	require.Len(t, degraded, 1)
	assert.Equal(t, "page_one", degraded[0].Profile.Name)
	assert.Equal(t, 5*time.Minute, degraded[0].Profile.Timeout, "降级那条是被顶掉的 store 条目本身")
	assert.Contains(t, degraded[0].Reason, "executors.commands")
	assert.Equal(t, 2, res1.Config)
	assert.Zero(t, res1.Stored, "被顶掉的 store 条目不在生效表里")
	assert.Equal(t, 1, res1.Degraded)

	// 替身里这个键的处理函数是新 config 那条建的：整批重登记后仍只有一份、且是 config 的闭包。
	_, ok := f.syncer.LookupHandler("exec.page_one")
	require.True(t, ok, "撞名后键位仍由新 config 档位占着，处理函数在")

	// 记账生效的一半证据：此刻 configCommands 已含 page_one，紧接着的 store 路径 Apply
	// 必须按"当前列表"判撞名——文件里那条 page_one 被判降级而不是让 ApplyStore 因硬撞名报错。
	// 若 Apply 还读启动期冻结的 registry.executors（不含 page_one），这一步会直接 error。
	resApply, err := f.applier.Apply()
	require.NoError(t, err, "Apply 的撞名判定用的是当前 configCommands（含 page_one），不是启动期那份")
	assert.Zero(t, len(resApply.Added), "page_one 已由 config 占着，store 那条只降级不新登记")
	assert.Equal(t, SourceConfig, mustSourceOf(t, f.registry, "exec.page_one"))
	assert.Len(t, f.registry.Degraded(), 1, "文件里那条 page_one 又被降级")

	// 2. 再 ApplyConfig 把撞名那条 config 档位删掉 → 键位从生效表消失，降级空，处理函数被摘除。
	//    这是设计文档 §12 那条风险的落点：降级条目在 config 批次换掉之后不该"复活"成生效条目，
	//    复活需要一次 store 路径的 Apply 重读文件——所以下一步专门验它。这不是缺陷，是既定行为。
	res2, err := f.applier.ApplyConfig(f.configWithCommands(t, "cfg_one"))
	require.NoError(t, err)

	assert.Equal(t, []string{"exec.cfg_one"}, f.registry.Keys(),
		"压它的 config 档位没了，被顶掉的 store 条目不自己回到生效表")
	assert.Empty(t, f.registry.Degraded(), "降级面跟着一批重算，压它的那条不在这一批里就整条消失")
	assert.Contains(t, res2.Removed, "exec.page_one")
	_, ok = f.syncer.LookupHandler("exec.page_one")
	assert.False(t, ok, "键位被摘除，而不是留着一条没人认领的降级")

	// 3. 调一次 a.Apply()（store 路径重读文件）→ page_one 回到生效表（store 来源）。
	//    这一步证明两条链对同一张表的操作是收敛的：文件里那条一直都在，只是需要 store 路径才会重新生效。
	res3, err := f.applier.Apply()
	require.NoError(t, err)
	assert.Contains(t, res3.Added, "exec.page_one")
	assert.Equal(t, SourceStore, mustSourceOf(t, f.registry, "exec.page_one"), "store 路径重读才让它复活成生效条目")
}

// --- §5.4 与重启等价 ---

// 运行期换表与"改完 yaml 重启"必须给出同一张表。
func TestApplier_ApplyConfigMatchesStartup(t *testing.T) {
	f := newApplierFixture(t, "cfg_a") // 启动时 config 侧只有 cfg_a
	candidate := f.configWithCommands(t, "cfg_a", "cfg_b")
	_, err := f.applier.ApplyConfig(candidate)
	require.NoError(t, err)

	// 用同一份 candidate 走启动路径建一张全新的表。
	fresh, err := NewRegistry(candidate, quietLogger())
	require.NoError(t, err)

	assert.Equal(t, fresh.Keys(), f.registry.Keys())
	for _, key := range fresh.Keys() {
		wantSource, _ := fresh.SourceOf(key)
		gotSource, _ := f.registry.SourceOf(key)
		assert.Equal(t, wantSource, gotSource, key)

		wantProfile, _ := fresh.Lookup(key)
		gotProfile, _ := f.registry.Lookup(key)
		require.NotNil(t, wantProfile, key)
		require.NotNil(t, gotProfile, key)
		assert.Equal(t, wantProfile.Timeout, gotProfile.Timeout, key)
	}
}

// --- §5.5 并发 ---

// ApplyConfig 与 Apply 共用一把写锁串行，读侧只看 snapshot。
// 终态断言只问一件事：每个生效键都有处理函数、每个 exec. 处理函数都对应一个生效键——
// 一次完整的写临界区（registry 换表 + syncHandlers）会把调度器对齐到生效表，
// 两个写协程都结束后没有再写，所以终态必然自洽；不断言中间态。
func TestApplier_ApplyConfigConcurrentWithApplyStaysConsistent(t *testing.T) {
	f := newConfigApplierFixture(t, "x") // 启动 config 侧有 x
	f.save(t, "s")                       // store 侧有 s
	list1 := f.configWithCommands(t, "x", "y")
	list2 := f.configWithCommands(t, "x", "z")

	var readers, writers sync.WaitGroup
	stop := make(chan struct{})

	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, item := range f.registry.List() {
				if item.Profile == nil {
					t.Error("List 读到了没有档位的行")
					return
				}
			}
			f.syncer.HandlerNames()
		}
	}()

	writers.Add(2)
	go func() {
		defer writers.Done()
		for round := 0; round < 20; round++ {
			candidate := list1
			if round%2 == 1 {
				candidate = list2
			}
			if _, err := f.applier.ApplyConfig(candidate); err != nil {
				t.Errorf("ApplyConfig 失败: %v", err)
			}
		}
	}()
	go func() {
		defer writers.Done()
		for round := 0; round < 20; round++ {
			if _, err := f.applier.Apply(); err != nil {
				t.Errorf("Apply 失败: %v", err)
			}
		}
	}()

	writers.Wait()
	close(stop)
	readers.Wait()

	live := f.registry.Keys()
	liveSet := make(map[string]bool, len(live))
	for _, key := range live {
		liveSet[key] = true
	}
	for _, key := range live {
		_, ok := f.syncer.LookupHandler(key)
		assert.True(t, ok, "终态：生效表的键 %s 应当在调度器里有处理函数", key)
	}
	for _, name := range f.syncer.HandlerNames() {
		if strings.HasPrefix(name, core.ExecPrefix) {
			assert.True(t, liveSet[name], "终态：调度器里不该有多余的 exec. 键 %s", name)
		}
	}

	_, ok := f.syncer.LookupHandler("payment_check")
	assert.True(t, ok, "并发全程普通任务键都不被触碰")
}

// NewConfigApplier 不接 store：Apply（store 路径）返回明确错误而不是 nil panic，
// ApplyConfig 照常可用（设计文档 §13 P2 的那一支）。
func TestApplier_NewConfigApplierStorePathErrorsNotPanics(t *testing.T) {
	f := newConfigApplierFixture(t, "cfg_a")

	configOnly, err := NewConfigApplier(f.syncer, f.registry, nil, quietLogger())
	require.NoError(t, err)

	_, err = configOnly.Apply()
	require.Error(t, err, "没有档位文件的同步器上，store 路径要返回明确错误")
	assert.Contains(t, err.Error(), "no profile store")

	// ApplyConfig 在这种部署里照常可用。
	res, err := configOnly.ApplyConfig(f.configWithCommands(t, "cfg_a", "cfg_b"))
	require.NoError(t, err)
	assert.Contains(t, res.Added, "exec.cfg_b")
}
