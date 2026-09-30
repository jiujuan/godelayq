package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 测试辅助：造一个以临时目录为 workspace 的配置，以及 workspace 内的真实文件。

func configWith(workspace string, commands ...core.ExecutorCommand) core.Config {
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.Commands = commands
	return cfg
}

// declareFile 在 workspace 下真实创建 rel（含父目录），返回写好的相对路径。
// E02 不要求文件存在（那属于 E03 的探测），但符号链接分支必须用真实存在的文件才能验证。
func declareFile(t *testing.T, workspace, rel string) string {
	t.Helper()

	absolute := filepath.Join(workspace, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(absolute), 0o750))
	require.NoError(t, os.WriteFile(absolute, []byte("#!/bin/sh\necho placeholder\n"), 0o750))
	return rel
}

func validScriptProfile(scriptRel string) core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:       "nightly_report",
		Kind:       "script",
		Runtime:    "node",
		Script:     scriptRel,
		Args:       []core.ExecutorArg{{Name: "day", Required: true, Pattern: "^(yesterday|today)$"}},
		ArgsRender: []string{"--day={day}"},
	}
}

func validBinaryProfile(programRel string) core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:       "etl_full",
		Kind:       "binary",
		Program:    programRel,
		FixedArgs:  []string{"--full"},
		Positional: &core.ExecutorPositional{Max: 3},
	}
}

func validHTTPProfile() core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:            "rebuild_index",
		Kind:            "http",
		Method:          "POST",
		URLTemplate:     "https://api.internal/v1/tenants/{tenant}/rebuild",
		AllowedHosts:    []string{"api.internal"},
		Args:            []core.ExecutorArg{{Name: "tenant", Required: true, Pattern: "^[a-z0-9-]{1,32}$"}},
		Body:            "json",
		ExpectStatus:    []int{200, 202},
		CaptureResponse: true,
	}
}

// mustLoad 断言加载成功，返回全部档位。
func mustLoad(t *testing.T, cfg core.Config) []*Profile {
	t.Helper()

	profiles, err := LoadProfiles(cfg)
	if err != nil {
		t.Fatalf("LoadProfiles failed unexpectedly: %v", err)
	}
	return profiles
}

// expectLoadError 断言加载失败，并检查错误信息里的定位前缀与关键片段。
func expectLoadError(t *testing.T, cfg core.Config, contains ...string) {
	t.Helper()

	_, err := LoadProfiles(cfg)
	if err == nil {
		t.Fatalf("expected a validation error, got none")
	}
	message := err.Error()
	if !strings.Contains(message, "executors.commands[") {
		t.Fatalf("error must locate the profile, got %q", message)
	}
	for _, want := range contains {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q must contain %q", message, want)
		}
	}
}

// TestLoadProfiles_FromConfigFile 走完整链路：配置文件 → core.LoadConfig → LoadProfiles。
//
// 这条是卡片 §7 的手工验证的可执行版本：服务端把 LoadProfiles 接进启动流程（TASK-E04）之后，
// 配置里一条越界的脚本路径就必须让进程起不来，并指出是哪一条档位。
func TestLoadProfiles_FromConfigFile(t *testing.T) {
	workspace := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
executors:
  enabled: true
  workspace: ` + filepath.ToSlash(workspace) + `
  commands:
    - name: deploy_hook
      kind: script
      runtime: bash
      script: ../../evil.sh
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := core.LoadConfig(path)
	require.NoError(t, err, "配置解码本身应该通过，问题在档位取值")

	_, err = LoadProfiles(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `executors.commands[0] "deploy_hook"`)
	assert.Contains(t, err.Error(), "../../evil.sh")
}

func TestLoadProfiles_KindsAndDefaults(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")
	program := declareFile(t, workspace, "bin/etl")

	profiles := mustLoad(t, configWith(workspace,
		validScriptProfile(script),
		validBinaryProfile(program),
		validHTTPProfile(),
	))
	if len(profiles) != 3 {
		t.Fatalf("expected 3 profiles, got %d", len(profiles))
	}

	scriptProfile, binaryProfile, httpProfile := profiles[0], profiles[1], profiles[2]

	// 注册键与展示名
	for _, tc := range []struct {
		got  Profile
		want string
	}{
		{*scriptProfile, "exec.nightly_report"},
		{*binaryProfile, "exec.etl_full"},
		{*httpProfile, "exec.rebuild_index"},
	} {
		if tc.got.HandlerKey() != tc.want {
			t.Errorf("HandlerKey() = %q, want %q", tc.got.HandlerKey(), tc.want)
		}
	}

	// 绝对路径与相对写法同时给出，绝对路径不外露
	if !filepath.IsAbs(scriptProfile.ScriptPath) {
		t.Errorf("ScriptPath must be absolute, got %q", scriptProfile.ScriptPath)
	}
	if scriptProfile.ScriptRel != "scripts/report.mjs" {
		t.Errorf("ScriptRel = %q, want %q", scriptProfile.ScriptRel, "scripts/report.mjs")
	}
	if scriptProfile.ProgramDisplay() != "scripts/report.mjs" {
		t.Errorf("ProgramDisplay() = %q", scriptProfile.ProgramDisplay())
	}
	if binaryProfile.ProgramRel != "bin/etl" {
		t.Errorf("ProgramRel = %q, want bin/etl", binaryProfile.ProgramRel)
	}
	if !filepath.IsAbs(binaryProfile.ProgramPath) {
		t.Errorf("ProgramPath must be absolute, got %q", binaryProfile.ProgramPath)
	}

	// 超时：档位留空即取全局默认
	if scriptProfile.Timeout != core.DefaultExecDefaultTimeout {
		t.Errorf("Timeout = %v, want default %v", scriptProfile.Timeout, core.DefaultExecDefaultTimeout)
	}
	// 并发上限：留空即 1
	if scriptProfile.MaxParallel != 1 {
		t.Errorf("MaxParallel = %d, want 1", scriptProfile.MaxParallel)
	}
	// cwd 留空即 workspace 本身。workspace 在加载时会解析符号链接（macOS 的 /var → /private/var，
	// Windows 的短名目录），所以断言用解析后的根目录比较。
	resolvedWorkspace, linkErr := filepath.EvalSymlinks(workspace)
	if linkErr != nil {
		resolvedWorkspace = workspace
	}
	if scriptProfile.CwdPath != filepath.Clean(resolvedWorkspace) {
		t.Errorf("CwdPath = %q, want workspace %q", scriptProfile.CwdPath, resolvedWorkspace)
	}
	if scriptProfile.Workspace != filepath.Clean(resolvedWorkspace) {
		t.Errorf("Workspace = %q, want %q", scriptProfile.Workspace, resolvedWorkspace)
	}
	// 未声明 pattern 的参数套用默认安全集，E08 可直接使用
	profiles2 := mustLoad(t, configWith(workspace, core.ExecutorCommand{
		Name: "plain", Kind: "script", Runtime: "bash", Script: declareFile(t, workspace, "a.sh"),
		Args: []core.ExecutorArg{{Name: "free"}}, ArgsRender: []string{"{free}"},
	}))
	if profiles2[0].Args[0].Pattern == nil || profiles2[0].Args[0].PatternText != DefaultArgPattern {
		t.Errorf("empty pattern must fall back to the default safe set, got %#v", profiles2[0].Args[0])
	}

	// HTTP 档位：deny_private_ranges 留空即禁止，max_body_bytes 留空即继承输出上限
	if !httpProfile.DenyPrivate {
		t.Error("deny_private_ranges must default to true")
	}
	if httpProfile.MaxBodyBytes != core.DefaultExecMaxOutputBytes {
		t.Errorf("MaxBodyBytes = %d, want inherited %d", httpProfile.MaxBodyBytes, core.DefaultExecMaxOutputBytes)
	}
	if httpProfile.MaxParallel != 1 {
		t.Errorf("http MaxParallel = %d, want 1", httpProfile.MaxParallel)
	}
}

func TestLoadProfiles_ProgramNameInRuntimeAllow(t *testing.T) {
	workspace := t.TempDir()

	// program 精确命中 runtime_allow 时按 PATH 里的程序处理，不要求它是 workspace 内的文件
	profile := mustLoad(t, configWith(workspace, core.ExecutorCommand{
		Name: "jar_batch", Kind: "binary", Program: "java",
		FixedArgs: []string{"-jar", "app/app.jar"},
	}))[0]

	if profile.ProgramName != "java" {
		t.Errorf("ProgramName = %q, want java", profile.ProgramName)
	}
	if profile.ProgramPath != "" {
		t.Errorf("ProgramPath must stay empty for a PATH program, got %q", profile.ProgramPath)
	}
	if profile.ProgramDisplay() != "java" {
		t.Errorf("ProgramDisplay() = %q, want java", profile.ProgramDisplay())
	}
}

func TestLoadProfiles_PathEscape(t *testing.T) {
	workspace := t.TempDir()

	for _, tc := range []struct {
		name  string
		field string
		build func(value string) core.Config
	}{
		{"script climbs out", "script", func(v string) core.Config {
			cmd := validScriptProfile(v)
			return configWith(workspace, cmd)
		}},
		{"cwd climbs out", "cwd", func(v string) core.Config {
			cmd := validScriptProfile(declareFile(t, workspace, "ok.sh"))
			cmd.Cwd = v
			return configWith(workspace, cmd)
		}},
		{"program climbs out", "program", func(v string) core.Config {
			cmd := validBinaryProfile(v)
			return configWith(workspace, cmd)
		}},
	} {
		for _, value := range []string{"../escape.sh", "../../outside/file", "./inside/../../escape.sh"} {
			t.Run(tc.name+" "+value, func(t *testing.T) {
				expectLoadError(t, tc.build(value), tc.field)
			})
		}
	}

	t.Run("absolute script path", func(t *testing.T) {
		// 前导分隔符的写法在两个平台上都不是"相对 workspace"：Unix 下它是绝对路径，
		// Windows 下 Go 的 IsAbs 还会把它判成相对，所以校验按写法先拒掉。
		for _, value := range []string{"/tmp/evil.sh", `\tmp\evil.bat`, `\\server\share\evil.bat`} {
			expectLoadError(t, configWith(workspace, validScriptProfile(value)), "script", "relative")
		}
	})

	// 盘符写法只在 Windows 上是绝对路径
	if runtime.GOOS == "windows" {
		t.Run("windows drive letter", func(t *testing.T) {
			expectLoadError(t, configWith(workspace, validScriptProfile(`C:/Windows/evil.bat`)), "script", "relative")
		})
	}
}

func TestLoadProfiles_SymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("prepare target failed: %v", err)
	}

	link := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating symlinks is not permitted here: %v", err)
	}

	// 符号链接指向 workspace 之外时必须拒绝：这是脚本内容不受 workspace 约束的唯一绕行方式
	expectLoadError(t, configWith(workspace, validScriptProfile("linked.txt")), "symlink")

	inWorkspace := filepath.Join(workspace, "real.txt")
	if err := os.WriteFile(inWorkspace, []byte("ok"), 0o600); err != nil {
		t.Fatalf("prepare file failed: %v", err)
	}
	nested := filepath.Join(workspace, "nested-link")
	if err := os.Symlink(inWorkspace, nested); err != nil {
		t.Skipf("creating symlinks is not permitted here: %v", err)
	}
	if profiles := mustLoad(t, configWith(workspace, validScriptProfile("nested-link"))); len(profiles) != 1 {
		t.Fatalf("a symlink staying inside the workspace must be accepted")
	}
}

// TestLoadProfiles_SymlinkEscapeViaResolver 用注入点覆盖"解析结果落在 workspace 之外"这条分支。
//
// 上一条真实符号链接的用例在 Windows 上需要管理员权限或开发者模式，没权限时整段跳过；
// 这个版本不依赖平台权限，保证同一条判定在任何机器上都被执行过。
func TestLoadProfiles_SymlinkEscapeViaResolver(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")

	outside, err := filepath.Abs(filepath.Join(t.TempDir(), "escape.sh"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o600))

	inWorkspace := filepath.Join(workspace, filepath.FromSlash(script))
	original := evalSymlinks
	t.Cleanup(func() { evalSymlinks = original })

	t.Run("link pointing outside is refused", func(t *testing.T) {
		evalSymlinks = func(path string) (string, error) {
			if path == inWorkspace {
				return outside, nil
			}
			return filepath.EvalSymlinks(path)
		}
		expectLoadError(t, configWith(workspace, validScriptProfile(script)), "outside executors.workspace")
	})

	t.Run("link staying inside is accepted", func(t *testing.T) {
		evalSymlinks = func(path string) (string, error) {
			if path == inWorkspace {
				return inWorkspace, nil
			}
			return filepath.EvalSymlinks(path)
		}
		profiles := mustLoad(t, configWith(workspace, validScriptProfile(script)))
		if len(profiles) != 1 {
			t.Fatalf("expected the profile to load")
		}
	})
}

func TestLoadProfiles_MissingFileIsNotAProfileError(t *testing.T) {
	// 文件是否真的存在属于探测（TASK-E03），不属于档位校验：
	// 配置先写、脚本后部署是常见顺序，这种写法应该让档位显示"不可用"，而不是让进程起不来。
	workspace := t.TempDir()

	profile := mustLoad(t, configWith(workspace, validScriptProfile("scripts/absent.mjs")))[0]
	if profile.ScriptPath == "" {
		t.Fatal("ScriptPath must still be resolved for an absent file")
	}
	if _, err := os.Stat(profile.ScriptPath); !os.IsNotExist(err) {
		t.Fatalf("the test expects a non-existent script, stat said %v", err)
	}
}

func TestLoadProfiles_UnknownRuntime(t *testing.T) {
	workspace := t.TempDir()

	cmd := validScriptProfile(declareFile(t, workspace, "scripts/report.mjs"))
	cmd.Runtime = "ruby"
	expectLoadError(t, configWith(workspace, cmd), "ruby", "runtime_allow")

	// 空白与首尾空格的解释器名同样拒绝：那会造出一个永远匹配不上的白名单项
	cmd = validScriptProfile(declareFile(t, workspace, "scripts/report.mjs"))
	cmd.Runtime = "node "
	expectLoadError(t, configWith(workspace, cmd), "runtime", "runtime_allow")
}

func TestLoadProfiles_ArgRules(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")

	base := func(mutate func(*core.ExecutorCommand)) core.Config {
		cmd := validScriptProfile(script)
		mutate(&cmd)
		return configWith(workspace, cmd)
	}

	t.Run("duplicate arg name", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Args = []core.ExecutorArg{{Name: "day"}, {Name: "day"}}
			c.ArgsRender = nil
		}), "declared twice")
	})
	t.Run("bad arg name", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Args = []core.ExecutorArg{{Name: "Day-1"}}
			c.ArgsRender = nil
		}), "lower-case")
	})
	t.Run("invalid regexp", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Args = []core.ExecutorArg{{Name: "day", Pattern: "^([a-z]$"}}
			c.ArgsRender = []string{"--day={day}"}
		}), "not a valid regexp")
	})
	t.Run("default does not match pattern", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Args = []core.ExecutorArg{{Name: "day", Default: "tomorrow", Pattern: "^(yesterday|today)$"}}
			c.ArgsRender = []string{"--day={day}"}
		}), "default \"tomorrow\" does not match")
	})
	t.Run("required with default", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Args = []core.ExecutorArg{{Name: "day", Required: true, Default: "today"}}
			c.ArgsRender = []string{"--day={day}"}
		}), "both required and carry a default")
	})
	t.Run("render references undeclared arg", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.ArgsRender = []string{"--missing={nope}"}
		}), "not declared in args")
	})
	t.Run("render contains shell metacharacters", func(t *testing.T) {
		for _, item := range []string{"--day={day};rm", "--day={day}|x", "--day={day}$IFS", "--day={day>`x`", "--day={day>\n"} {
			expectLoadError(t, base(func(c *core.ExecutorCommand) {
				c.ArgsRender = []string{item}
			}), "shell special character")
		}
	})
	t.Run("render brace is unbalanced", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.ArgsRender = []string{"--day={day"}
		}), "unclosed {")
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.ArgsRender = []string{"--day=day}"}
		}), "unexpected }")
	})
	t.Run("positional rules", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Positional = &core.ExecutorPositional{Max: 0}
		}), "between 1 and 16")
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Positional = &core.ExecutorPositional{Max: 17}
		}), "between 1 and 16")
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Positional = &core.ExecutorPositional{Max: 3, Pattern: "($"}
		}), "not a valid regexp")
	})
	t.Run("max_parallel negative", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.MaxParallel = -1
		}), "max_parallel must not be negative")
	})
}

func TestLoadProfiles_EnvAndFixedArgRules(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")

	base := func(mutate func(*core.ExecutorCommand)) core.Config {
		cmd := validScriptProfile(script)
		mutate(&cmd)
		return configWith(workspace, cmd)
	}

	t.Run("env key with credential prefix", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Env = map[string]string{"GODELAYQ_SERVER_AUTH_TOKEN": "x"}
		}), "GODELAYQ_ prefixed keys hold server credentials")
	})
	t.Run("env key with an unusable character", func(t *testing.T) {
		// 小写不再是被拒的理由：viper 会把映射键折成小写，档位读进来时先折回大写再校验
		// （见 TestLoadProfiles_ConfigFileEnvKeys）。减号这类字符仍然拒绝。
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Env = map[string]string{"report-home": "/srv"}
		}), "upper-case letters")
	})
	t.Run("env value with control character", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.Env = map[string]string{"REPORT_HOME": "a\nb"}
		}), "control characters")
	})
	t.Run("env_allow entry is padded", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.EnvAllow = []string{" TRACE "}
		}), "surrounding spaces")
	})
	t.Run("env_allow entry is empty", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.EnvAllow = []string{""}
		}), "empty entry")
	})
	t.Run("fixed_args control character", func(t *testing.T) {
		expectLoadError(t, configWith(workspace, func() core.ExecutorCommand {
			cmd := validBinaryProfile(declareFile(t, workspace, "bin/etl"))
			cmd.FixedArgs = []string{"--full\x00"}
			return cmd
		}()), "control characters")
	})
}

func TestLoadProfiles_HttpRules(t *testing.T) {
	workspace := t.TempDir()

	base := func(mutate func(*core.ExecutorCommand)) core.Config {
		cmd := validHTTPProfile()
		mutate(&cmd)
		return configWith(workspace, cmd)
	}

	t.Run("redirects are never followed", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.MaxRedirects = 2 }), "max_redirects must be 0")
	})
	t.Run("bare wildcard host", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.AllowedHosts = []string{"*"} }), "allowed_hosts")
	})
	t.Run("host with path fragment", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.AllowedHosts = []string{"evil.com/#x"} }), "must not contain a path")
	})
	t.Run("host is a url", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.AllowedHosts = []string{"https://api.internal"} }), "not a URL")
	})
	t.Run("missing allowed hosts", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.AllowedHosts = nil }), "allowed_hosts must not be empty")
	})
	t.Run("plain http outside loopback", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "http://api.internal/v1/x"
		}), "http is only allowed for loopback hosts")
	})
	t.Run("loopback http needs explicit allow", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "http://127.0.0.1:9090/v1/x"
		}), "not in allowed_hosts")
	})
	t.Run("status out of range", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.ExpectStatus = []int{999} }), "between 100 and 599")
	})
	t.Run("bad method", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.Method = "post" }), "method")
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.Method = "TRACE" }), "method")
	})
	t.Run("url placeholder not declared", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "https://api.internal/v1/{missing}"
		}), "not declared in args")
	})
	t.Run("url carries credentials", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "https://user:pass@api.internal/v1/x"
		}), "user credentials")
	})
	t.Run("body required for methods that send one", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.Body = "" }), "body is required")
		expectLoadError(t, base(func(c *core.ExecutorCommand) { c.Body = "form" }), "body \"form\" is invalid")
	})
	t.Run("wildcard host with private ranges allowed", func(t *testing.T) {
		off := false
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.AllowedHosts = []string{"*.internal"}
			c.DenyPrivate = &off
		}), "requires concrete hosts")
	})
	t.Run("loopback http with allow is accepted", func(t *testing.T) {
		off := false
		profile := mustLoad(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "http://127.0.0.1:9090/v1/x"
			c.AllowedHosts = []string{"127.0.0.1:9090"}
			c.DenyPrivate = &off
		}))[0]
		if profile.DenyPrivate {
			t.Error("explicit deny_private_ranges false must be honoured")
		}
	})
	t.Run("suffix wildcard matches only real suffixes", func(t *testing.T) {
		profile := mustLoad(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "https://a.internal/v1/x"
			c.AllowedHosts = []string{"*.internal"}
		}))
		if len(profile) != 1 {
			t.Fatal("a.internal must match *.internal")
		}
		// evil 前缀不该被当成子域
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.URLTemplate = "https://a.internal.evil.com/v1/x"
			c.AllowedHosts = []string{"*.internal"}
		}), "not in allowed_hosts")
	})
	t.Run("port must be numeric", func(t *testing.T) {
		expectLoadError(t, base(func(c *core.ExecutorCommand) {
			c.AllowedHosts = []string{"api.internal:abc"}
		}), "not a number")
	})
}

func TestLoadProfiles_NameCharset(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")

	for _, tc := range []struct {
		name   string
		mutate func(*core.ExecutorCommand)
		want   string
	}{
		{"name with space", func(c *core.ExecutorCommand) { c.Name = "a b" }, "letters, digits, underscore or hyphen"},
		{"empty name", func(c *core.ExecutorCommand) { c.Name = "" }, "name must not be empty"},
		{"dotted name", func(c *core.ExecutorCommand) { c.Name = "a.b" }, "letters, digits, underscore or hyphen"},
		{"overlong name", func(c *core.ExecutorCommand) { c.Name = strings.Repeat("n", 65) }, "1-64 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := validScriptProfile(script)
			tc.mutate(&cmd)
			expectLoadError(t, configWith(workspace, cmd), tc.want)
		})
	}

	t.Run("duplicate names collapse to one key", func(t *testing.T) {
		first := validScriptProfile(script)
		second := validScriptProfile(script)
		second.Runtime = "bash"
		expectLoadError(t, configWith(workspace, first, second), "duplicate profile name", "executors.commands[0]")
	})
}

// 档位之间重名由本任务检查；档位名与代码注册的处理函数重名属于注册环节（TASK-E04），
// 因为那需要调度器的注册表才能判断。
func TestLoadProfiles_DuplicateDetectionBoundary(t *testing.T) {
	workspace := t.TempDir()
	cmd := validScriptProfile(declareFile(t, workspace, "scripts/report.mjs"))
	cmd.Name = "payment_check"

	profile := mustLoad(t, configWith(workspace, cmd))[0]
	if profile.HandlerKey() != "exec.payment_check" {
		t.Fatalf("the name alone must be accepted here, got %q", profile.HandlerKey())
	}
}

func TestLoadProfiles_KindFieldMismatch(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")
	program := declareFile(t, workspace, "bin/etl")

	t.Run("script carries binary fields", func(t *testing.T) {
		cmd := validScriptProfile(script)
		cmd.Program = program
		expectLoadError(t, configWith(workspace, cmd), "program belongs to binary profiles, not script")
	})
	t.Run("script carries http fields", func(t *testing.T) {
		cmd := validScriptProfile(script)
		cmd.Method = "GET"
		expectLoadError(t, configWith(workspace, cmd), "method belongs to http profiles, not script")
	})
	t.Run("binary carries script fields", func(t *testing.T) {
		cmd := validBinaryProfile(program)
		cmd.Runtime = "node"
		expectLoadError(t, configWith(workspace, cmd), "runtime belongs to script profiles, not binary")
	})
	t.Run("binary carries http fields", func(t *testing.T) {
		cmd := validBinaryProfile(program)
		cmd.AllowedHosts = []string{"api.internal"}
		expectLoadError(t, configWith(workspace, cmd), "allowed_hosts belongs to http profiles, not binary")
	})
	t.Run("http carries process fields", func(t *testing.T) {
		cmd := validHTTPProfile()
		cmd.Cwd = "sub"
		expectLoadError(t, configWith(workspace, cmd), "cwd belongs to script/binary profiles, not http")
	})
	t.Run("required field per kind", func(t *testing.T) {
		cmd := validScriptProfile(script)
		cmd.Script = ""
		expectLoadError(t, configWith(workspace, cmd), "script is required")

		binary := validBinaryProfile(program)
		binary.Program = ""
		expectLoadError(t, configWith(workspace, binary), "program is required")

		http := validHTTPProfile()
		http.URLTemplate = ""
		expectLoadError(t, configWith(workspace, http), "url_template is required")
	})
	t.Run("unknown kind", func(t *testing.T) {
		cmd := validScriptProfile(script)
		cmd.Kind = "container"
		expectLoadError(t, configWith(workspace, cmd), "kind \"container\" is invalid")
	})
	t.Run("missing kind", func(t *testing.T) {
		cmd := validScriptProfile(script)
		cmd.Kind = ""
		expectLoadError(t, configWith(workspace, cmd), "kind \"\" is invalid")
	})
}

func TestLoadProfiles_TimeoutRules(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")

	cfg := configWith(workspace, func() core.ExecutorCommand {
		cmd := validScriptProfile(script)
		cmd.Timeout = 2 * time.Hour // 超过默认上限 30m
		return cmd
	}())
	expectLoadError(t, cfg, "exceeds executors.max_timeout")

	negative := configWith(workspace, func() core.ExecutorCommand {
		cmd := validScriptProfile(script)
		cmd.Timeout = -time.Minute
		return cmd
	}())
	expectLoadError(t, negative, "timeout must not be negative")

	// 档位内的合法超时被原样采用
	inside := configWith(workspace, func() core.ExecutorCommand {
		cmd := validScriptProfile(script)
		cmd.Timeout = 90 * time.Second
		return cmd
	}())
	if got := mustLoad(t, inside)[0].Timeout; got != 90*time.Second {
		t.Errorf("Timeout = %v, want 90s", got)
	}

	// 全局 default_timeout 大于 max_timeout 属于配置矛盾，档位再合法也救不回来
	broken := configWith(workspace, validScriptProfile(script))
	broken.Executors.DefaultTimeout = 2 * time.Hour
	broken.Executors.MaxTimeout = time.Minute
	expectLoadError(t, broken, "default_timeout")
}

func TestLoadProfiles_EmptyCommandsIsLegal(t *testing.T) {
	profiles, err := LoadProfiles(configWith(t.TempDir()))
	if err != nil {
		t.Fatalf("an empty command list must not be an error: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("expected no profiles, got %d", len(profiles))
	}
}

func TestProfile_ValidatePayloadKeys(t *testing.T) {
	workspace := t.TempDir()
	profiles := mustLoad(t, configWith(workspace,
		validScriptProfile(declareFile(t, workspace, "scripts/report.mjs")),
		validHTTPProfile(),
	))
	scriptProfile, httpProfile := profiles[0], profiles[1]

	accepted := []struct {
		profile *Profile
		keys    []string
	}{
		{scriptProfile, nil},
		{scriptProfile, []string{"args", "env", "timeout"}},
		{httpProfile, []string{"params", "headers", "body", "timeout"}},
	}
	for _, tc := range accepted {
		if err := tc.profile.ValidatePayloadKeys(tc.keys); err != nil {
			t.Errorf("keys %v on %s must be accepted: %v", tc.keys, tc.profile.Kind, err)
		}
	}

	rejected := []struct {
		profile *Profile
		key     string
	}{
		{scriptProfile, "cmd"},
		{scriptProfile, "script"},
		{scriptProfile, "url"},
		{scriptProfile, "params"},
		{httpProfile, "url"},
		{httpProfile, "args"},
		{httpProfile, "env"},
	}
	for _, tc := range rejected {
		err := tc.profile.ValidatePayloadKeys([]string{tc.key})
		if err == nil {
			t.Fatalf("payload key %q on %s must be rejected", tc.key, tc.profile.Kind)
		}
		if !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "not accepted") {
			t.Errorf("error must name the offending key, got %q", err)
		}
	}
}

func TestProfile_ArgLookup(t *testing.T) {
	workspace := t.TempDir()
	profile := mustLoad(t, configWith(workspace,
		validScriptProfile(declareFile(t, workspace, "scripts/report.mjs")),
	))[0]

	arg, ok := profile.Arg("day")
	if !ok {
		t.Fatal("arg day must be found")
	}
	if !arg.Required || arg.PatternText != "^(yesterday|today)$" {
		t.Errorf("unexpected arg spec %#v", arg)
	}
	if _, ok := profile.Arg("missing"); ok {
		t.Error("unknown arg must not resolve")
	}
}

// 错误信息必须能定位到具体档位，并带上可 grep 的键名，否则配置多了以后无从下手。
func TestProfileErrorPrefix(t *testing.T) {
	workspace := t.TempDir()
	cmd := validScriptProfile(declareFile(t, workspace, "scripts/report.mjs"))
	cmd.Timeout = 48 * time.Hour

	_, err := LoadProfiles(configWith(workspace, cmd))
	if err == nil {
		t.Fatal("expected an error")
	}
	want := fmt.Sprintf("executors.commands[0] %q", cmd.Name)
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error must start with %q, got %q", want, err.Error())
	}
}

// TestLoadProfiles_ConfigFileEnvKeys 走配置文件这一路：viper 解码映射时把键折成小写，
// 而 configs/config.example.yaml 的写法与 envNamePattern 都是大写环境变量名。
// 这一条固定"从 YAML 读进来仍然按大写校验并保存"，否则示例配置自己就加载不过。
func TestLoadProfiles_ConfigFileEnvKeys(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
executors:
  enabled: true
  workspace: ` + filepath.ToSlash(workspace) + `
  commands:
    - name: nightly_report
      kind: script
      runtime: node
      script: scripts/report.mjs
      env: { REPORT_HOME: /srv/report }
      env_allow: [TRACE_ID]
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := core.LoadConfig(path)
	require.NoError(t, err)
	profiles := mustLoad(t, cfg)

	assert.Equal(t, map[string]string{"REPORT_HOME": "/srv/report"}, profiles[0].Env)
	assert.Equal(t, []string{"TRACE_ID"}, profiles[0].EnvAllow)
}

// TestHandlerKeyPrefixMatchesCoreLoader 检查注册键与加载器边界用的是同一个前缀（E17 DoD 第三条）：
// 字面量只写在 core.ExecPrefix 一处，加载器默认拒绝的正好是档位实际注册出来的那些名字。
func TestHandlerKeyPrefixMatchesCoreLoader(t *testing.T) {
	assert.Equal(t, core.ExecPrefix, HandlerKeyPrefix, "两处名字要指着同一个值")

	profile := &Profile{Name: "nightly_report"}
	key := profile.HandlerKey()

	assert.Equal(t, "exec.nightly_report", key)
	assert.True(t, core.IsExecHandlerKey(key),
		"注册键要被加载器的边界判断认出来，否则默认拒绝管不到真正能执行的名字")
}
