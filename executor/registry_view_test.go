package executor

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 展示面的读法（TASK-W07）：List 一次读表给出"档位 + 来源 + 降级 + 可编辑"，
// PathDisplay 给出这个档位指向的本机文件写法，RuntimeAllow 把配置里的解释器名单透出去。
// 这三样都是只读的，本文件因此不碰调度器也不碰产物目录。

func TestPathDisplay_Kinds(t *testing.T) {
	ec := execConfigForMode(t)
	workspace := ec.Workspace
	outside := filepath.Join(t.TempDir(), "elsewhere", "report.mjs")

	// 同盘越界的相对写法：filepath.Rel 会给 ..\..\ 上跳形式，判越界不能看这个形状（D-0201）
	upJump := filepath.Join("..", "..", "outside-by-relative", "report.mjs")

	cases := []struct {
		label  string
		cmd    core.ExecutorCommand
		mode   PathMode
		expect func(t *testing.T, got string, p *Profile)
	}{
		{
			label: "script relative inside workspace",
			cmd:   core.ExecutorCommand{Name: "in_rel", Kind: "script", Runtime: "node", Script: "scripts/report.mjs"},
			mode:  PathWithinWorkspace,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Equal(t, "scripts/report.mjs", filepath.ToSlash(got))
			},
		},
		{
			label: "script absolute inside workspace",
			cmd: core.ExecutorCommand{Name: "in_abs", Kind: "script", Runtime: "node",
				Script: filepath.Join(workspace, "scripts", "report.mjs")},
			// 严格模式只接受相对写法（越界与写法两件事一次判掉），
			// 所以"绝对路径但其实就在 workspace 之内"这种只有页面那一侧能构造出来
			mode: PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Equal(t, "scripts/report.mjs", filepath.ToSlash(got),
					"绝对写法只要落在 workspace 内就归一成相对写法")
			},
		},
		{
			label: "script absolute outside workspace",
			cmd:   core.ExecutorCommand{Name: "out_abs", Kind: "script", Runtime: "node", Script: outside},
			mode:  PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Equal(t, filepath.ToSlash(p.ScriptPath), filepath.ToSlash(got),
					"越界时原样给绝对路径，也就是探测与执行真正用的那一条")
				assert.NotContains(t, filepath.ToSlash(got), "..")
			},
		},
		{
			label: "script relative jumping outside workspace",
			cmd:   core.ExecutorCommand{Name: "out_rel", Kind: "script", Runtime: "node", Script: upJump},
			mode:  PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				// 这一条正是 D-0201 的落点：ScriptRel 长得像相对写法，判据却只能是 withinDirectory
				assert.True(t, filepath.IsAbs(got), "同盘越界要给绝对路径，实际 %q", got)
				assert.NotContains(t, filepath.ToSlash(got), "..", "绝对路径里不该再带上跳段")
			},
		},
		{
			label: "binary from PATH",
			cmd:   core.ExecutorCommand{Name: "path_bin", Kind: "binary", Program: "node"},
			mode:  PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Empty(t, got, "PATH 里的程序名没有路径可言")
			},
		},
		{
			// 只有 PathAnywhere 模式能构造出"program 是 workspace 外文件路径"的档位：
			// 严格模式下 program 必须是 runtime_allow 里的程序名（否则启动即报越界）。
			// 这一条与上面五条的差别在于它补上了 ProgramPath/ProgramRel 的覆盖。
			label: "binary file outside workspace",
			cmd:   core.ExecutorCommand{Name: "out_bin", Kind: "binary", Program: outside},
			mode:  PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Equal(t, filepath.ToSlash(p.ProgramPath), filepath.ToSlash(got),
					"越界的产物给绝对路径")
			},
		},
		{
			label: "binary file inside workspace",
			cmd: core.ExecutorCommand{Name: "in_bin", Kind: "binary",
				Program: filepath.Join(workspace, "bin", "tool.exe")},
			mode: PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Equal(t, "bin/tool.exe", filepath.ToSlash(got),
					"workspace 内的产物给相对写法，与脚本档位同一条规则")
			},
		},
		{
			label: "http",
			cmd: core.ExecutorCommand{Name: "http", Kind: "http", Method: "GET",
				URLTemplate: "https://api.example.com/x", AllowedHosts: []string{"api.example.com"}},
			mode: PathAnywhere,
			expect: func(t *testing.T, got string, p *Profile) {
				assert.Empty(t, got, "http 档位没有文件可指")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			profile, err := BuildProfile(tc.cmd, ec, tc.mode)
			require.NoError(t, err)
			tc.expect(t, profile.PathDisplay(), profile)
		})
	}
}

// storeCmd 造一条只用于登记表的 http 档位（探测不访问网络，结论恒为可用）。
func storeCmd(name string) *Profile {
	return &Profile{
		Name: name, Kind: KindHTTP, Timeout: time.Minute, MaxParallel: 1,
		Method: "GET", URLTemplate: "https://api.example.com/" + name,
		AllowedHosts: []string{"api.example.com"},
	}
}

func newViewRegistry(t *testing.T, webEnabled bool) *Registry {
	t.Helper()

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.WebEnabled = webEnabled
	cfg.Executors.Workspace = t.TempDir()
	cfg.Executors.Commands = []core.ExecutorCommand{
		{Name: "cfg_a", Kind: "http", Method: "GET",
			URLTemplate: "https://api.example.com/a", AllowedHosts: []string{"api.example.com"}},
		{Name: "cfg_b", Kind: "http", Method: "GET",
			URLTemplate: "https://api.example.com/b", AllowedHosts: []string{"api.example.com"}},
	}
	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)
	return registry
}

func TestRegistryList_MergesSourcesAndOrders(t *testing.T) {
	registry := newViewRegistry(t, true)
	require.NoError(t, registry.ApplyStore([]StoreEntry{
		{Profile: storeCmd("store_z")},
		{Profile: storeCmd("cfg_a"), Degraded: true},
	}))

	listed := registry.List()
	require.Len(t, listed, 4, "三条生效 + 一条降级")

	type row struct {
		key      string
		source   Source
		degraded bool
		editable bool
	}
	got := make([]row, 0, len(listed))
	for _, item := range listed {
		got = append(got, row{item.Profile.HandlerKey(), item.Source, item.Degraded, item.Editable})
	}

	// 按注册键字典序；同一个键的两行里生效那条在前（cfg_a 出现两次正是撞名的本义）
	assert.Equal(t, []row{
		{"exec.cfg_a", SourceConfig, false, false},
		{"exec.cfg_a", SourceStore, true, false},
		{"exec.cfg_b", SourceConfig, false, false},
		{"exec.store_z", SourceStore, false, true},
	}, got)

	for _, item := range listed {
		if item.Degraded {
			assert.Contains(t, item.Reason, "executors.commands",
				"降级条目要自带一句能展示的原因，接口那边不再补文案")
		}
	}
}

func TestRegistryList_EditableFollowsWebEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		registry := newViewRegistry(t, enabled)
		require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeCmd("store_z")}}))

		var storeRows int
		for _, item := range registry.List() {
			if item.Source != SourceStore {
				assert.False(t, item.Editable, "配置侧档位永远不可编辑")
				continue
			}
			storeRows++
			assert.Equal(t, enabled, item.Editable,
				"editable 就是 web_enabled 那一条开关的取值，两处不许各判一次")
		}
		require.Equal(t, 1, storeRows)
	}
}

func TestRegistryList_IsACopy(t *testing.T) {
	registry := newViewRegistry(t, true)
	require.NoError(t, registry.ApplyStore([]StoreEntry{{Profile: storeCmd("store_z")}}))

	listed := registry.List()
	require.Len(t, listed, 3)
	listed[0].Editable = !listed[0].Editable
	listed[0].Reason = "caller wrote here"

	assert.NotEqual(t, "caller wrote here", registry.List()[0].Reason,
		"接口层拿到的是副本，改它不该影响登记表")
}

func TestRegistryRuntimeAllow(t *testing.T) {
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = false
	cfg.Executors.RuntimeAllow = []string{"python", "node"}
	registry, err := NewRegistry(cfg, quietLogger())
	require.NoError(t, err)

	// 执行器关着也给：这是一份配置事实，W08 的下拉在未启用的部署里也要能渲染出候选项
	assert.Equal(t, []string{"python", "node"}, registry.RuntimeAllow())

	got := registry.RuntimeAllow()
	got[0] = "tampered"
	assert.Equal(t, []string{"python", "node"}, registry.RuntimeAllow(), "返回的是副本")

	// 配置里留空时给的是归一化补齐的那份默认名单，不是空表
	defaults, err := NewRegistry(core.DefaultConfig(), quietLogger())
	require.NoError(t, err)
	assert.NotEmpty(t, defaults.RuntimeAllow())
}

// TestRegistryList_RaceWithApplyStore 守 I3 的另一半：读侧一次 Load 就必须看到一张自洽的表。
// List 需要 entries 与 degraded 两份数据，分两次 Load 时这里会读到"降级条目来自上一张表"。
// 与 W03 那条变异同一手法：把 List 改成两次 Load，-race 下应当报出竞争。
func TestRegistryList_RaceWithApplyStore(t *testing.T) {
	registry := newViewRegistry(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		if i%5 == 0 {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				entries := []StoreEntry{{Profile: storeCmd("store_z")}}
				if n%2 == 0 {
					entries = append(entries, StoreEntry{Profile: storeCmd("cfg_a"), Degraded: true})
				}
				require.NoError(t, registry.ApplyStore(entries))
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			listed := registry.List()
			for _, item := range listed {
				if item.Profile == nil {
					t.Error("List 读到了没有档位的行")
					return
				}
			}
		}()
	}
	wg.Wait()
}
