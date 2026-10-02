package executor

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组用例守的是 Registry.ApplyConfig（TASK-R04，设计文档 §7.5）：
// 它是 ApplyStore 的镜像——只动 config 那一批、store 那一批原样保留，
// 差别在多一条运行期才有的规则：新 config 条目与现存 store 条目撞名时 config 赢。

// newConfigSideRegistry 造一张 config 侧有 names 那几条档位的登记表，
// 顺带把 workspace 交出去（同一批用例要拿它造新一批档位）。
// webEnabled 决定展示面上的 editable：撞名降级那条必须是 false，
// 所以需要一个"开了 web"的场景，否则 false 可以由开关本身解释掉。
func newConfigSideRegistry(t *testing.T, webEnabled bool, names ...string) (*Registry, string) {
	t.Helper()

	workspace := t.TempDir()
	cfg := configAllowing(workspace, []string{selfExecutable(t)})
	cfg.Executors.WebEnabled = webEnabled
	for _, name := range names {
		cfg.Executors.Commands = append(cfg.Executors.Commands, namedScript(t, workspace, name))
	}

	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)
	require.True(t, registry.Enabled())
	return registry, workspace
}

// applyConfigProfiles 按启动同一条链路（LoadProfiles）把命令列表换成待登记的档位：
// ApplyConfig 的真实入参就是它产出的那种 *Profile，用例因此造不出实现拿不到的档位。
func applyConfigProfiles(t *testing.T, workspace string, commands ...core.ExecutorCommand) []*Profile {
	t.Helper()

	return mustLoad(t, configAllowing(workspace, []string{selfExecutable(t)}, commands...))
}

// activeRow 与 degradedRow 从展示面里按键取行：撞名时同一个键有两行，只能靠 Degraded 区分。
func activeRow(t *testing.T, registry *Registry, key string) ListedProfile {
	t.Helper()

	for _, item := range registry.List() {
		if !item.Degraded && item.Profile.HandlerKey() == key {
			return item
		}
	}
	t.Fatalf("展示面里没有生效的 %q 行：%v", key, registry.List())
	return ListedProfile{}
}

func degradedRow(t *testing.T, registry *Registry, key string) ListedProfile {
	t.Helper()

	for _, item := range registry.List() {
		if item.Degraded && item.Profile.HandlerKey() == key {
			return item
		}
	}
	t.Fatalf("展示面里没有降级的 %q 行：%v", key, registry.List())
	return ListedProfile{}
}

func TestRegistry_ApplyConfig_ReplacesConfigSideAndKeepsStoreSide(t *testing.T) {
	registry, workspace := newConfigSideRegistry(t, false, "a", "b")
	require.Equal(t, []string{"exec.a", "exec.b"}, registry.Keys())

	// store 那条走既有入口登记进来：本卡不该有任何路径动到它
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("s", time.Minute)}}))

	// 新一批：b 带着一个可辨认的 timeout 回来，c 是新增，a 不在这一批里
	replaced := namedScript(t, workspace, "b")
	replaced.Timeout = 3 * time.Minute
	batch := applyConfigProfiles(t, workspace, replaced, namedScript(t, workspace, "c"))
	require.NoError(t, registry.ApplyConfig(batch))

	assert.Equal(t, []string{"exec.b", "exec.c", "exec.s"}, registry.Keys(),
		"整表替换：不在这一批里的旧 config 条目消失，store 那批原样保留")

	profile, ok := registry.Lookup("exec.b")
	require.True(t, ok)
	assert.Equal(t, 3*time.Minute, profile.Timeout,
		"生效表里的 b 必须是这一批给的那条，而不是启动时留在表里的那条")

	source, ok := registry.SourceOf("exec.s")
	require.True(t, ok)
	assert.Equal(t, SourceStore, source, "store 条目的来源不该因为一次 config 替换而改变")

	// 探测在 ApplyConfig 里补做，口径与 NewRegistry 相同：不可用也入表，可用就是可用
	reason, available := registry.Available("exec.c")
	assert.True(t, available, "解释器与脚本都在本机上的 config 档位应判为可用，实际 %q", reason)

	assert.Empty(t, registry.Degraded(), "没有撞名就不该有降级条目")
}

// 同批重复键的兜底必须由 ApplyConfig 自己给：LoadProfiles 那一步会拒重名，
// 但它是"配置文件里的那份列表"的判据，登记表这一侧的写入口不能假设调用方一定先过它。
func TestRegistry_ApplyConfig_RejectsBadBatchesAndKeepsOldTable(t *testing.T) {
	t.Run("同批内重复键", func(t *testing.T) {
		registry, workspace := newConfigSideRegistry(t, false, "cfg_one")
		require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("s", time.Minute)}}))
		before := registry.Keys()
		beforeDegraded := registry.Degraded()

		// 直接构造两条同名档位（storeProfile 只是个 Profile 构造函数，来源由被调的方法决定）
		err := registry.ApplyConfig([]*Profile{
			storeProfile("dup", time.Minute),
			storeProfile("dup", 2*time.Minute),
			namedProfile(t, workspace, "legitimate"),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate handler key")
		assert.Contains(t, err.Error(), "exec.dup")

		assert.Equal(t, before, registry.Keys(), "被拒的那批不该留下中间态：连合法的那条也不能进表")
		assert.Equal(t, beforeDegraded, registry.Degraded())
		_, ok := registry.Lookup("exec.legitimate")
		assert.False(t, ok)
	})

	t.Run("空档位", func(t *testing.T) {
		registry, _ := newConfigSideRegistry(t, false, "cfg_one")
		before := registry.Keys()

		require.Error(t, registry.ApplyConfig([]*Profile{nil}))
		assert.Equal(t, before, registry.Keys(), "没有档位的条目必须被拒，表不动")
	})
}

// namedProfile 造一条本机可执行的 config 档位（LoadProfiles 的产物形状，只用于同批构造）。
func namedProfile(t *testing.T, workspace, name string) *Profile {
	t.Helper()

	return applyConfigProfiles(t, workspace, namedScript(t, workspace, name))[0]
}

func TestRegistry_ApplyConfig_CollisionWithStoreSideGoesToDegraded(t *testing.T) {
	registry, workspace := newConfigSideRegistry(t, true, "cfg_one")
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("page_one", time.Minute)}}))

	// 换批之前：page_one 在生效表里，来源 store，页面上可编辑
	require.Equal(t, SourceStore, mustSourceOf(t, registry, "exec.page_one"))
	require.True(t, activeRow(t, registry, "exec.page_one").Editable,
		"web_enabled 打开时 store 条目本来可编辑——下面那个 false 才只能由降级解释")

	batch := applyConfigProfiles(t, workspace,
		namedScript(t, workspace, "cfg_one"),
		namedScript(t, workspace, "page_one"))
	require.NoError(t, registry.ApplyConfig(batch))

	assert.Equal(t, []string{"exec.cfg_one", "exec.page_one"}, registry.Keys())
	assert.Equal(t, SourceConfig, mustSourceOf(t, registry, "exec.page_one"),
		"撞名时 config 赢：这个键的来源从此是 config")

	won, ok := registry.Lookup("exec.page_one")
	require.True(t, ok)
	assert.Same(t, batch[1], won, "生效表里必须是新交进来的那条 config 档位")

	degraded := registry.Degraded()
	require.Len(t, degraded, 1, "被顶掉的 store 条目要留在展示面里")
	assert.Equal(t, "page_one", degraded[0].Profile.Name)
	assert.Equal(t, time.Minute, degraded[0].Profile.Timeout,
		"降级那条是被顶掉的 store 条目本身，不是压它的那条 config 档位")
	assert.Contains(t, degraded[0].Reason, "executors.commands",
		"降级原因要一眼说得清是谁把它顶掉的，实际 %q", degraded[0].Reason)

	// §5.1：降级条目在展示面上不可编辑，且原因原样透出
	row := degradedRow(t, registry, "exec.page_one")
	assert.False(t, row.Editable, "降级条目不该给编辑入口：改了它也不会生效")
	assert.Equal(t, SourceStore, row.Source)
	assert.NotEmpty(t, row.Reason)
}

func mustSourceOf(t *testing.T, registry *Registry, key string) Source {
	t.Helper()

	source, ok := registry.SourceOf(key)
	require.True(t, ok, "键 %q 不在生效表里", key)
	return source
}

func TestRegistry_ApplyConfig_RejectsWhenExecutorsDisabled(t *testing.T) {
	registry, workspace := newConfigSideRegistry(t, false, "cfg_one")
	closed, err := NewRegistry(core.Config{Executors: core.ExecutorsConfig{Enabled: false}}, quietLogger())
	require.NoError(t, err)
	require.False(t, closed.Enabled())
	require.Empty(t, closed.Keys())

	err = closed.ApplyConfig(applyConfigProfiles(t, workspace, namedScript(t, workspace, "late")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executors are disabled")
	assert.Empty(t, closed.Keys(), "被拒之后表仍是空的，不能出现接口看得见、调度器跑不了的半状态")

	// 同一个 workspace 上正常那张表不受影响：这条只是确认判据在登记表自己身上
	assert.Equal(t, []string{"exec.cfg_one"}, registry.Keys())
}

// 空批次是"删掉配置里最后一条档位"的路径，不是不可能：它必须真的清空 config 那一批。
func TestRegistry_ApplyConfig_EmptyBatchClearsConfigSide(t *testing.T) {
	registry, _ := newConfigSideRegistry(t, false, "a", "b")
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("s", time.Minute)}}))

	require.NoError(t, registry.ApplyConfig(nil))

	assert.Equal(t, []string{"exec.s"}, registry.Keys(),
		"ApplyConfig(nil) 清空 config 那一批，store 那一批不受影响（不是 no-op）")
	assert.Empty(t, registry.Degraded())
	assert.Equal(t, SourceStore, mustSourceOf(t, registry, "exec.s"))

	// 再给回一批：整表替换在空表上照常成立
	require.NoError(t, registry.ApplyConfig([]*Profile{storeProfile("fresh", time.Minute)}))
	assert.Equal(t, []string{"exec.fresh", "exec.s"}, registry.Keys())
}

// 降级面跟着新一批重算，两条方向都要钉住：
//   - 同一批重复应用（可重入）→ 降级条目原样留着，计数不来回跳。
//   - 顶掉它的那条 config 档位被删掉 → 它整条消失，既不回到生效表也不留在降级面；
//     复活它需要一次 store 路径的整批重登记（设计文档 §12 那条风险的既定行为，不是缺陷）。
func TestRegistry_ApplyConfig_DegradedViewFollowsTheNewBatch(t *testing.T) {
	registry, workspace := newConfigSideRegistry(t, true, "cfg_one")
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("page_one", time.Minute)}}))

	clashing := applyConfigProfiles(t, workspace,
		namedScript(t, workspace, "cfg_one"),
		namedScript(t, workspace, "page_one"))
	require.NoError(t, registry.ApplyConfig(clashing))
	require.Len(t, registry.Degraded(), 1)

	// 同一批再来一次：降级条目仍在（还是那条 config 档位压着它），不会晃成 0
	require.NoError(t, registry.ApplyConfig(clashing))
	require.Len(t, registry.Degraded(), 1, "重复应用同一批不该让降级计数来回跳")
	assert.Equal(t, []string{"exec.cfg_one", "exec.page_one"}, registry.Keys())

	// 这一批里没有 page_one 了：那个键位整条消失，不"复活"成生效条目
	require.NoError(t, registry.ApplyConfig(
		applyConfigProfiles(t, workspace, namedScript(t, workspace, "cfg_one"))))
	assert.Equal(t, []string{"exec.cfg_one"}, registry.Keys(),
		"被顶掉的 store 条目不能因为压它的那条没了就自己回到生效表")
	assert.Empty(t, registry.Degraded())

	// store 路径重读文件才让它回来——这条证明两条链对同一张表的操作是收敛的
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("page_one", time.Minute)}}))
	assert.Equal(t, []string{"exec.cfg_one", "exec.page_one"}, registry.Keys())
	assert.Equal(t, SourceStore, mustSourceOf(t, registry, "exec.page_one"))
}

// 并发：换 config 批次与读展示面同时进行（-race 跑这条）。
// 读侧一次 Load 必须拿到一张自洽的表——同一批里 page_one 要么生效要么降级，不能两行都不在，
// 也不能既显示降级又给出编辑入口。
func TestRegistry_ApplyConfig_ConcurrentReadersSeeOneConsistentTable(t *testing.T) {
	registry, workspace := newConfigSideRegistry(t, true, "cfg_one")
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeProfile("page_one", time.Minute)}}))

	withClash := applyConfigProfiles(t, workspace,
		namedScript(t, workspace, "cfg_one"),
		namedScript(t, workspace, "page_one"))
	withoutClash := applyConfigProfiles(t, workspace, namedScript(t, workspace, "cfg_one"))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, item := range registry.List() {
					if item.Profile == nil {
						t.Error("List 读到了没有档位的行")
						return
					}
					if item.Degraded && item.Editable {
						t.Errorf("降级条目不该同时可编辑：%+v", item)
						return
					}
				}
				registry.Profiles()
				registry.Degraded()
				registry.RuntimeAllow()
			}
		}()
	}

	for round := 0; round < 20; round++ {
		batch := withClash
		if round%2 == 1 {
			batch = withoutClash
		}
		require.NoError(t, registry.ApplyConfig(batch))
	}

	close(stop)
	wg.Wait()

	// 终态自洽：最后一批是 withClash，所以两个键都在生效表里，page_one 归 config。
	// 降级面此时是空的——那条 store 条目在第一轮撞名时就被移出去了，
	// 而后续批次里没有一次 store 路径的重读去把它取回来（既定行为，见上面的注释）。
	require.NoError(t, registry.ApplyConfig(withClash))
	assert.Equal(t, []string{"exec.cfg_one", "exec.page_one"}, registry.Keys())
	assert.Equal(t, SourceConfig, mustSourceOf(t, registry, "exec.page_one"))
	assert.Empty(t, registry.Degraded())

	// store 路径重读一次之后，同样的 withClash 批次才又给出降级行：两条链对同一张表收敛。
	// 这里给 Degraded:true 正是 MergeStoreProfiles 在那张表上会算出的标记（config 赢，§5.2）。
	require.NoError(t, registry.ApplyStore([]StoreEntry{
		{Profile: storeProfile("page_one", time.Minute), Degraded: true},
	}))
	assert.Len(t, registry.Degraded(), 1)
	require.NoError(t, registry.ApplyConfig(withClash))
	assert.Len(t, registry.Degraded(), 1)
}
