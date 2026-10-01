package executor

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 启动合并的两份来源规则不同、校验同源（设计文档 §4 的 I1 与 §5.2）：
// config 侧一条写错即启动失败，store 侧一条写错只跳过那一条。
// 下面既断言 store 侧的"不连坐"，也断言合并没有顺手把 config 侧的规则放宽。

// storedRecords 把档位定义换成档位文件里的记录，名字以外的字段照原样过一遍。
func storedRecords(commands ...core.ExecutorCommand) []core.ExecutorProfileRecord {
	records := make([]core.ExecutorProfileRecord, 0, len(commands))
	for _, cmd := range commands {
		records = append(records, core.NewExecutorProfileRecord(cmd))
	}
	return records
}

func storeKeys(entries []StoreEntry) []string {
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Profile.HandlerKey())
	}
	return keys
}

func TestMergeStoreProfiles_StoreOnlyRecords(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	cfg := configAllowing(workspace, []string{executable})

	entries, warnings := MergeStoreProfiles(cfg, storedRecords(
		namedScript(t, workspace, "alpha"),
		namedScript(t, workspace, "beta"),
	))

	assert.Empty(t, warnings)
	// 顺序沿用存储 List 的字典序口径，不做二次加工
	assert.Equal(t, []string{"exec.alpha", "exec.beta"}, storeKeys(entries))
	for _, entry := range entries {
		assert.Equal(t, SourceStore, entry.Source)
		assert.False(t, entry.Degraded, "config 侧一条都没有，不该有降级")
		assert.True(t, entry.Probe.Available, entry.Probe.Reason)
	}
}

func TestMergeStoreProfiles_NoRecordsYieldsNoEntries(t *testing.T) {
	cfg := configAllowing(t.TempDir(), nil)

	entries, warnings := MergeStoreProfiles(cfg, nil)

	assert.Empty(t, entries)
	assert.Empty(t, warnings)
}

// 页面建的档位允许指向 workspace 之外的本机路径（设计文档 D5 的主动偏离）。
func TestMergeStoreProfiles_AcceptsPathsOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	executable := selfExecutable(t)
	scriptAbs := filepath.Join(outside, filepath.FromSlash(declareFile(t, outside, "scripts/report.mjs")))

	cfg := configAllowing(workspace, []string{executable})
	record := core.NewExecutorProfileRecord(core.ExecutorCommand{
		Name:    "elsewhere",
		Kind:    "script",
		Runtime: executable,
		Script:  scriptAbs,
	})

	entries, warnings := MergeStoreProfiles(cfg, []core.ExecutorProfileRecord{record})

	assert.Empty(t, warnings, "宽松模式下越界写法不该连坐")
	require.Len(t, entries, 1)
	assert.Equal(t, filepath.Clean(scriptAbs), entries[0].Profile.ScriptPath)
	assert.True(t, entries[0].Probe.Available, entries[0].Probe.Reason)
}

func TestMergeStoreProfiles_NameCollisionMarksStoreSideDegraded(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	// config 侧声明 alpha，store 侧同名一条加另一条
	cfg := configAllowing(workspace, []string{executable}, namedScript(t, workspace, "alpha"))

	entries, warnings := MergeStoreProfiles(cfg, storedRecords(
		namedScript(t, workspace, "alpha"),
		namedScript(t, workspace, "beta"),
	))

	assert.Empty(t, warnings)
	require.Len(t, entries, 2)

	assert.Equal(t, "exec.alpha", entries[0].Profile.HandlerKey())
	assert.True(t, entries[0].Degraded, "与 executors.commands 撞名的 store 条目要降级")
	assert.Empty(t, entries[0].Reason, "降级文案由登记表统一给，这里留空免得两处各写一句")
	assert.Equal(t, SourceStore, entries[0].Source)

	assert.Equal(t, "exec.beta", entries[1].Profile.HandlerKey())
	assert.False(t, entries[1].Degraded)
}

// 降级条目交给登记表之后才是最终形态：config 那条留在表里、store 那条只进展示面。
func TestMergeStoreProfiles_ResultFeedsApplyStore(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	cfg := configAllowing(workspace, []string{executable}, namedScript(t, workspace, "alpha"))

	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)

	entries, _ := MergeStoreProfiles(cfg, storedRecords(
		namedScript(t, workspace, "alpha"),
		namedScript(t, workspace, "beta"),
	))
	require.NoError(t, registry.ApplyStore(entries))

	assert.Equal(t, []string{"exec.alpha", "exec.beta"}, registry.Keys())
	source, ok := registry.SourceOf("exec.alpha")
	require.True(t, ok)
	assert.Equal(t, SourceConfig, source, "同名时 config 赢")
	source, ok = registry.SourceOf("exec.beta")
	require.True(t, ok)
	assert.Equal(t, SourceStore, source)

	degraded := registry.Degraded()
	require.Len(t, degraded, 1)
	assert.Equal(t, "exec.alpha", degraded[0].Profile.HandlerKey())
	assert.Contains(t, degraded[0].Reason, "executors.commands")
}

// store 侧不连坐：一条非法只跳过它，其余照常进表。
func TestMergeStoreProfiles_InvalidRecordIsSkippedNotFatal(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	cfg := configAllowing(workspace, []string{executable})

	badCombo := core.NewExecutorProfileRecord(core.ExecutorCommand{
		Name: "broken_combo",
		Kind: "script",
		// 缺 runtime：字段组合非法，归 BuildProfile 判
		Script: "scripts/x.mjs",
	})
	badTimeout := core.NewExecutorProfileRecord(core.ExecutorCommand{
		Name:        "broken_timeout",
		Kind:        "http",
		Method:      "GET",
		URLTemplate: "http://example.invalid/",
	})
	badTimeout.Timeout = "-5s"

	entries, warnings := MergeStoreProfiles(cfg, []core.ExecutorProfileRecord{
		badCombo,
		badTimeout,
		core.NewExecutorProfileRecord(namedScript(t, workspace, "survivor")),
	})

	require.Len(t, entries, 1, "坏掉的两条不该带走剩下的那条")
	assert.Equal(t, "exec.survivor", entries[0].Profile.HandlerKey())

	require.Len(t, warnings, 2)
	assert.Equal(t, "broken_combo", warnings[0].Name)
	assert.Contains(t, warnings[0].Reason, "broken_combo", "错误文本要点名是哪条档位")
	assert.Equal(t, "broken_timeout", warnings[1].Name)
	assert.Contains(t, warnings[1].Reason, "timeout")
}

// 同一份非法配置，config 侧照旧连坐：合并函数没有顺手放宽它。
func TestMergeStoreProfiles_DoesNotRevalidateConfigSide(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	climbing := filepath.Join(outside, declareFile(t, outside, "evil.sh"))

	// 严格模式下越界的 config 档位：LoadProfiles 拒，因此 NewRegistry 拒
	cfg := configAllowing(workspace, []string{selfExecutable(t)}, core.ExecutorCommand{
		Name:    "climbing",
		Kind:    "script",
		Runtime: "node",
		Script:  filepath.ToSlash(climbing),
	})
	_, err := NewRegistry(cfg, quietLogger())
	require.Error(t, err, "config 侧一条非法即启动失败，这条口径不属于本卡")

	// 而合并 store 侧时不重跑 config 校验：它只按名字判撞名
	entries, warnings := MergeStoreProfiles(cfg, storedRecords(namedScript(t, workspace, "stored_ok")))
	assert.Empty(t, warnings)
	require.Len(t, entries, 1)
	assert.Equal(t, "exec.stored_ok", entries[0].Profile.HandlerKey())
	assert.False(t, entries[0].Degraded)
}

// 撞名判定只看名字，不看 config 那条合不合法：越界的 alpha 依然让 store 的 alpha 降级。
// 这条与上一条合起来才说明"降级判据是键位占用，不是键位可用性"。
func TestMergeStoreProfiles_CollisionChecksNameOnly(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	climbing := filepath.Join(outside, declareFile(t, outside, "evil.sh"))

	cfg := configAllowing(workspace, []string{selfExecutable(t)}, core.ExecutorCommand{
		Name:    "alpha",
		Kind:    "script",
		Runtime: "node",
		Script:  filepath.ToSlash(climbing),
	})

	entries, warnings := MergeStoreProfiles(cfg, storedRecords(namedScript(t, workspace, "alpha")))

	assert.Empty(t, warnings)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].Degraded, "配置里那条即使探测/校验不过也占着键位")
}

// 探测失败的 store 条目照常进表：与 config 侧"看得见但跑不了"同一口径。
func TestMergeStoreProfiles_KeepsUnavailableProbe(t *testing.T) {
	workspace := t.TempDir()
	cfg := configAllowing(workspace, []string{missingProgram})
	record := core.NewExecutorProfileRecord(scriptUsingRuntime(missingProgram,
		declareFile(t, workspace, "scripts/plain.mjs")))

	entries, warnings := MergeStoreProfiles(cfg, []core.ExecutorProfileRecord{record})

	assert.Empty(t, warnings)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].Probe.Available)
	assert.NotEmpty(t, entries[0].Probe.Reason)
}

// 档位文件里的记录进登记表之后，注册链路与配置档位走的是同一条路：
// 这条是"页面建的档位活过重启"在包内的最小证明。
func TestMergeStoreProfiles_RegisteredProfileReachesTheScheduler(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	cfg := configAllowing(workspace, []string{executable})
	record := core.NewExecutorProfileRecord(namedScript(t, workspace, "from_store"))

	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)
	entries, warnings := MergeStoreProfiles(cfg, []core.ExecutorProfileRecord{record})
	require.Empty(t, warnings)
	require.NoError(t, registry.ApplyStore(entries))

	_, ok := registry.Lookup("exec.from_store")
	require.True(t, ok)

	registrar := newFakeRegistrar()
	result, err := Register(registrar, registry, cfg, nil, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Registered)
	assert.Zero(t, result.Degraded)
	assert.Equal(t, []string{"exec.from_store"}, registrar.registeredKeys())
	// 合并出来的档位照样落执行器池：来源不影响执行类别（TASK-E13）
	assert.Equal(t, core.JobClassExec, registrar.classes["exec.from_store"])
}
