package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"godelayq/core"
)

// 这一组用例守的是本系列的第一条不变量（I1 一份规则）：
// 页面上保存档位用的校验，与启动时读 config.yaml 用的是同一条代码路径。
// 两边一旦分叉，表现是"页面上存得进去、重启后启动失败"，而那中间还夹着一次重启。

func execConfigForMode(t *testing.T) core.ExecutorsConfig {
	t.Helper()

	ec := core.DefaultConfig().Executors
	ec.Workspace = t.TempDir()
	return ec
}

// describeProfile 把一条档位摊成可比较的文本。
//
// 不用 reflect.DeepEqual：ArgSpec.Pattern 是 *regexp.Regexp，两份分别编译出来的正则在
// DeepEqual 下并不相等（内部带锁与状态字段），比较结果只反映编译位置而不是规则。
func describeProfile(p *Profile) string {
	if p == nil {
		return "<nil>"
	}
	var lines []string
	lines = append(lines,
		fmt.Sprintf("name=%s", p.Name),
		fmt.Sprintf("kind=%s", p.Kind),
		fmt.Sprintf("workspace=%s", p.Workspace),
		fmt.Sprintf("runtime=%s", p.Runtime),
		fmt.Sprintf("scriptPath=%s", p.ScriptPath),
		fmt.Sprintf("scriptRel=%s", p.ScriptRel),
		fmt.Sprintf("programName=%s", p.ProgramName),
		fmt.Sprintf("programPath=%s", p.ProgramPath),
		fmt.Sprintf("programRel=%s", p.ProgramRel),
		fmt.Sprintf("fixedArgs=%v", p.FixedArgs),
		fmt.Sprintf("argsRender=%v", p.ArgsRender),
		fmt.Sprintf("cwdPath=%s", p.CwdPath),
		fmt.Sprintf("cwdRel=%s", p.CwdRel),
		fmt.Sprintf("env=%v", p.Env),
		fmt.Sprintf("envAllow=%v", p.EnvAllow),
		fmt.Sprintf("timeout=%v", p.Timeout),
		fmt.Sprintf("maxParallel=%d", p.MaxParallel),
		fmt.Sprintf("retryOnExit=%v", p.RetryOnExit),
		fmt.Sprintf("method=%s", p.Method),
		fmt.Sprintf("url=%s", p.URLTemplate),
		fmt.Sprintf("hosts=%v", p.AllowedHosts),
		fmt.Sprintf("headers=%v", p.Headers),
		fmt.Sprintf("headerAllow=%v", p.HeaderAllow),
		fmt.Sprintf("body=%s", p.Body),
		fmt.Sprintf("expectStatus=%v", p.ExpectStatus),
		fmt.Sprintf("capture=%v", p.CaptureResponse),
		fmt.Sprintf("maxBody=%d", p.MaxBodyBytes),
		fmt.Sprintf("denyPrivate=%v", p.DenyPrivate),
	)
	for _, arg := range p.Args {
		lines = append(lines, fmt.Sprintf("arg[%s] required=%v default=%s pattern=%s secret=%v allowDash=%v",
			arg.Name, arg.Required, arg.Default, arg.PatternText, arg.Secret, arg.AllowDash))
	}
	if p.Positional != nil {
		lines = append(lines, fmt.Sprintf("positional max=%d pattern=%s", p.Positional.Max, p.Positional.PatternText))
	}
	return strings.Join(lines, "\n")
}

// reasonOf 剥掉位置前缀，只留判定原因。
// 配置侧带 `executors.commands[i] "name":`，单条侧带 `profile "name":` ——
// 前缀本来就该不同（单条校验不属于那份列表），要一致的是前缀之后的原因文本。
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, prefix := range []string{"executors.commands", "profile "} {
		if strings.HasPrefix(message, prefix) {
			if _, rest, ok := strings.Cut(message, ": "); ok {
				return rest
			}
		}
	}
	return message
}

// 同一条档位定义，走列表入口与走单条入口必须给出完全相同的结论。
func TestBuildProfileSameRulesAsLoadProfiles(t *testing.T) {
	cases := []struct {
		title string
		cmd   core.ExecutorCommand
	}{
		{"script 档位", core.ExecutorCommand{
			Name: "nightly_report", Kind: "script", Runtime: "node", Script: "scripts/report.mjs",
			Args:        []core.ExecutorArg{{Name: "day", Required: true, Pattern: `^(yesterday|today)$`}},
			ArgsRender:  []string{"--day={day}"},
			Timeout:     core.DefaultExecDefaultTimeout,
			MaxParallel: 1,
			RetryOnExit: []int{75},
		}},
		{"binary 档位走 workspace 路径", core.ExecutorCommand{
			Name: "etl", Kind: "binary", Program: "bin/etl", FixedArgs: []string{"--stdin"},
		}},
		{"binary 档位走 PATH 程序名", core.ExecutorCommand{Name: "journal", Kind: "binary", Program: "bash"}},
		{"http 档位", core.ExecutorCommand{
			Name: "ping", Kind: "http", Method: "GET", URLTemplate: "https://example.invalid/{id}",
			AllowedHosts: []string{"example.invalid"},
			Args:         []core.ExecutorArg{{Name: "id", Required: true}},
		}},
		{"script 缺 runtime", core.ExecutorCommand{Name: "half", Kind: "script", Script: "scripts/x.py"}},
		{"script 填了 http 字段", core.ExecutorCommand{Name: "mixed", Kind: "script", Runtime: "node", Script: "a.mjs", Method: "GET"}},
		{"解释器不在白名单", core.ExecutorCommand{Name: "ruby", Kind: "script", Runtime: "ruby", Script: "a.rb"}},
		{"渲染模板引用未声明的参数", core.ExecutorCommand{
			Name: "bogus", Kind: "script", Runtime: "node", Script: "a.mjs", ArgsRender: []string{"--x={x}"},
		}},
		{"绝对路径（配置侧必拒）", core.ExecutorCommand{Name: "abs", Kind: "script", Runtime: "node", Script: "/etc/passwd"}},
		{"越界相对路径（配置侧必拒）", core.ExecutorCommand{Name: "up", Kind: "script", Runtime: "node", Script: "../outside/a.sh"}},
		{"档位名带点", core.ExecutorCommand{Name: "a.b", Kind: "script", Runtime: "node", Script: "a.mjs"}},
		{"超时超过上限", core.ExecutorCommand{
			Name: "slow", Kind: "script", Runtime: "node", Script: "a.mjs", Timeout: core.DefaultExecMaxTimeout + 1,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			ec := execConfigForMode(t)

			cfg := core.Config{Executors: ec}
			cfg.Executors.Commands = []core.ExecutorCommand{tc.cmd}
			listed, listErr := LoadProfiles(cfg)

			single, singleErr := BuildProfile(tc.cmd, ec, PathWithinWorkspace)

			if (listErr == nil) != (singleErr == nil) {
				t.Fatalf("两个入口的接受/拒绝必须一致：列表侧 err=%v，单条侧 err=%v", listErr, singleErr)
			}
			if reasonOf(listErr) != reasonOf(singleErr) {
				t.Errorf("两个入口的判定原因必须一致：\n列表侧 %v\n单条侧 %v", listErr, singleErr)
			}
			if listErr != nil {
				return
			}

			fromList := describeProfile(listed[0])
			fromSingle := describeProfile(single)
			if fromList != fromSingle {
				t.Errorf("同一条档位定义在两个入口下产出的 Profile 必须逐字段一致：\n%s\n----\n%s", fromList, fromSingle)
			}
		})
	}
}

// LoadProfiles 必须一直走严格模式：这是"config.yaml 侧口径不变"的落点。
func TestLoadProfilesAlwaysStrictEvenWithLooseDefaults(t *testing.T) {
	ec := execConfigForMode(t)
	absolute := filepath.Join(t.TempDir(), "outside.py")

	cfg := core.Config{Executors: ec}
	cfg.Executors.Commands = []core.ExecutorCommand{{
		Name: "abs_script", Kind: "script", Runtime: "python",
		Script: filepath.ToSlash(absolute),
	}}
	if _, err := LoadProfiles(cfg); err == nil {
		t.Error("配置文件里的档位不许指向 workspace 之外：越界必须启动失败")
	} else if !strings.Contains(err.Error(), "executors.commands[0]") {
		t.Errorf("配置侧的错误文案必须带上列表位置便于运维定位，实际 %v", err)
	}
}

func TestBuildProfilePathAnywhereAcceptsRootedAndClimbingPaths(t *testing.T) {
	ec := execConfigForMode(t)
	workspace := ec.Workspace

	t.Run("绝对路径", func(t *testing.T) {
		for _, written := range []string{"/srv/report/main.py", `D:\srv\report\main.py`, "../outside/run.sh"} {
			profile, err := BuildProfile(core.ExecutorCommand{
				Name: "loose", Kind: "script", Runtime: "python", Script: written,
			}, ec, PathAnywhere)
			if err != nil {
				t.Fatalf("宽松模式应接受 %q，实际 %v", written, err)
			}
			if !filepath.IsAbs(profile.ScriptPath) {
				t.Errorf("%q 解析结果应是绝对路径，实际 %q", written, profile.ScriptPath)
			}
			if withinDirectory(workspace, profile.ScriptPath) {
				t.Errorf("%q 不该被按 workspace 之内解释，实际 %q", written, profile.ScriptPath)
			}
			// relativeTo 只在"算不出相对路径"（Windows 跨盘符）时兜底成文件名，
			// 同盘越界时它老实给出 "../.." 形式。W07 的 path_display 因此不许拿 ScriptRel
			// 判"在不在 workspace 内"，要自己按 withinDirectory 判——这条两种形状都放过，
			// 是为了换机器时用例不闪。
			rel := profile.ScriptRel
			if rel != filepath.Base(profile.ScriptPath) && !strings.HasPrefix(rel, "..") {
				t.Errorf("%q 的相对写法应是文件名或上跳形式，实际 %q", written, rel)
			}
		}
	})

	// 模式只放宽"能在哪儿"，不重新定义"相对谁"：同一句相对写法在两种模式下必须同源。
	t.Run("相对写法仍以 workspace 为基准", func(t *testing.T) {
		cmd := core.ExecutorCommand{Name: "same_base", Kind: "script", Runtime: "python", Script: "scripts/report.py"}

		strict, err := BuildProfile(cmd, ec, PathWithinWorkspace)
		if err != nil {
			t.Fatalf("严格模式失败: %v", err)
		}
		loose, err := BuildProfile(cmd, ec, PathAnywhere)
		if err != nil {
			t.Fatalf("宽松模式失败: %v", err)
		}
		if strict.ScriptPath != loose.ScriptPath || strict.ScriptRel != loose.ScriptRel {
			t.Errorf("同一句相对写法在两种模式下应解析到同一处：strict=%q loose=%q", strict.ScriptPath, loose.ScriptPath)
		}
		if want := filepath.Join(workspace, "scripts", "report.py"); loose.ScriptPath != filepath.Clean(want) {
			t.Errorf("解析结果应在 workspace 之下，实际 %q", loose.ScriptPath)
		}
	})

	t.Run("cwd 留空仍是 workspace", func(t *testing.T) {
		for _, mode := range []PathMode{PathWithinWorkspace, PathAnywhere} {
			profile, err := BuildProfile(core.ExecutorCommand{
				Name: "cwd_case", Kind: "script", Runtime: "python", Script: "a.py",
			}, ec, mode)
			if err != nil {
				t.Fatalf("mode=%d 失败: %v", mode, err)
			}
			if profile.CwdPath != workspace {
				t.Errorf("mode=%d 的 cwd 应是 workspace，实际 %q", mode, profile.CwdPath)
			}
		}
	})

	t.Run("越界写法仍拒空值", func(t *testing.T) {
		for _, written := range []string{"", "   "} {
			if _, err := BuildProfile(core.ExecutorCommand{
				Name: "empty_script", Kind: "script", Runtime: "python", Script: written,
			}, ec, PathAnywhere); err == nil {
				t.Errorf("script=%q 在宽松模式下也必须被拒", written)
			}
		}
	})
}

// 非路径规则一律与模式无关：这条用例的反面是"宽松模式顺手放宽了别的判断"。
func TestPathModeOnlyAffectsPathFields(t *testing.T) {
	ec := execConfigForMode(t)

	for _, mode := range []PathMode{PathWithinWorkspace, PathAnywhere} {
		// 缺 runtime
		_, err := BuildProfile(core.ExecutorCommand{Name: "no_runtime", Kind: "script", Script: "a.py"}, ec, mode)
		if err == nil || !strings.Contains(err.Error(), "runtime") {
			t.Errorf("mode=%d 下缺 runtime 必须被拒，实际 %v", mode, err)
		}
		// 解释器不在白名单（绝对路径也一样拒）
		_, err = BuildProfile(core.ExecutorCommand{Name: "ruby", Kind: "script", Runtime: "ruby", Script: "/tmp/a.rb"}, ec, mode)
		if err == nil || !strings.Contains(err.Error(), "runtime_allow") {
			t.Errorf("mode=%d 下 runtime 白名单必须照判，实际 %v", mode, err)
		}
		// kind 与字段不配套
		_, err = BuildProfile(core.ExecutorCommand{Name: "mixed", Kind: "script", Runtime: "python", Script: "/tmp/a.py", Body: "json"}, ec, mode)
		if err == nil || !strings.Contains(err.Error(), "belongs to") {
			t.Errorf("mode=%d 下字段配套必须照判，实际 %v", mode, err)
		}
		// 档位名规则
		_, err = BuildProfile(core.ExecutorCommand{Name: "a.b", Kind: "script", Runtime: "python", Script: "/tmp/a.py"}, ec, mode)
		if err == nil {
			t.Errorf("mode=%d 下档位名规则必须照判", mode)
		}
	}
}

// binary 档位的 program 判定与模式无关：命中白名单就是 PATH 程序名。
func TestPathAnywhereDoesNotTurnProgramNameIntoPath(t *testing.T) {
	ec := execConfigForMode(t)

	for _, mode := range []PathMode{PathWithinWorkspace, PathAnywhere} {
		profile, err := BuildProfile(core.ExecutorCommand{Name: "use_bash", Kind: "binary", Program: "bash"}, ec, mode)
		if err != nil {
			t.Fatalf("mode=%d 失败: %v", mode, err)
		}
		if profile.ProgramName != "bash" || profile.ProgramPath != "" {
			t.Errorf("mode=%d 下 program 命中白名单应按 PATH 程序名处理，实际 name=%q path=%q",
				mode, profile.ProgramName, profile.ProgramPath)
		}
	}

	// 越界产物路径则按路径处理
	profile, err := BuildProfile(core.ExecutorCommand{Name: "outside_bin", Kind: "binary", Program: `D:\tools\etl.exe`}, ec, PathAnywhere)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if profile.ProgramName != "" || !strings.HasSuffix(profile.ProgramPath, "etl.exe") {
		t.Errorf("越界 program 应按路径处理，实际 name=%q path=%q", profile.ProgramName, profile.ProgramPath)
	}
}

// Probe 只认 Profile 里的绝对路径，不需要知道模式：这条是"W02 不放 Probe 一行"的依据。
func TestProbeConsumesLooseProfileWithoutChanges(t *testing.T) {
	ec := execConfigForMode(t)

	dir := t.TempDir()
	real := filepath.Join(dir, "present.py")
	if err := os.WriteFile(real, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatalf("预置脚本失败: %v", err)
	}

	profile, err := BuildProfile(core.ExecutorCommand{
		Name: "probe_ok", Kind: "script", Runtime: "python", Script: filepath.ToSlash(real),
	}, ec, PathAnywhere)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	result := Probe(profile)
	if !result.Available {
		t.Errorf("越界但真实存在的脚本应探测为可用，实际原因 %q", result.Reason)
	}

	missing := filepath.Join(dir, "absent.py")
	profile, err = BuildProfile(core.ExecutorCommand{
		Name: "probe_missing", Kind: "script", Runtime: "python", Script: filepath.ToSlash(missing),
	}, ec, PathAnywhere)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if result = Probe(profile); result.Available {
		t.Error("不存在的脚本应探测为不可用")
	} else if !strings.Contains(result.Reason, "absent.py") {
		t.Errorf("不可用的原因应指名脚本，实际 %q", result.Reason)
	}
}

// 宽松模式下 Render 与 ValidateSubmission 不需要知道模式：
// 命令行用的始终是解析后的绝对路径（D3：argv 直传、不经 shell）。
func TestRenderUsesAbsolutePathInBothModes(t *testing.T) {
	ec := execConfigForMode(t)

	cmd := core.ExecutorCommand{
		Name: "render_case", Kind: "script", Runtime: "python", Script: "/srv/report/main.py",
		Args:       []core.ExecutorArg{{Name: "day", Required: true, Pattern: `^(yesterday|today)$`}},
		ArgsRender: []string{"--day={day}"},
	}
	profile, err := BuildProfile(cmd, ec, PathAnywhere)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	sub, err := ValidateSubmission(profile, []byte(`{"args":{"day":"today"}}`))
	if err != nil {
		t.Fatalf("payload 校验失败: %v", err)
	}
	argv, err := profile.Render(sub)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if len(argv) != 3 || argv[0] != "python" || argv[2] != "--day=today" {
		t.Fatalf("argv 形状不符：%q", argv)
	}
	if !filepath.IsAbs(argv[1]) {
		t.Errorf("argv[1] 应是绝对路径（子进程 cwd 由 cmd.Dir 决定），实际 %q", argv[1])
	}
}
