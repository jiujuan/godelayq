// Package executor 把配置里声明的档位（executors.commands）变成可注册的执行任务。
//
// 依赖方向是本包 → core：配置结构体定义在 core/config.go（与其它配置结构同处），
// 这里只做"这条配置能不能用"的启动期校验，产出可直接使用的 Profile。
// core 不反过来依赖本包。
package executor

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"godelayq/core"
)

// HandlerKeyPrefix 是档位在调度器注册表里的键前缀：配置里的 name 变成注册键 "exec.<name>"。
// 这样档位与代码注册的处理函数共用同一份注册表（core.Scheduler.RegisterHandler），
// 提交校验、崩溃恢复、重试绑定都不需要新增机制。
//
// 取值来自 core.ExecPrefix（TASK-E17）：目录加载器要判断同一个前缀，而它不能反向依赖本包，
// 所以字面量只写在 core 一处，这里只是个同义名字。
const HandlerKeyPrefix = core.ExecPrefix

// Kind 是档位的执行方式。
type Kind string

const (
	KindScript Kind = "script" // 解释器 + workspace 内脚本
	KindBinary Kind = "binary" // workspace 内已编译产物，或 PATH 里的程序
	KindHTTP   Kind = "http"   // 配置里固定下来的 HTTP 请求
)

// DefaultArgPattern 是参数未声明 pattern 时套用的字符集：字母、数字与 . _ : / = , -，
// 长度 1..256，不含空格、换行与任何 shell 元字符。路径类参数够用，
// 但档位应为路径型参数写更窄的 pattern（见设计文档 §9 风险 3）。
const DefaultArgPattern = `^[A-Za-z0-9._:/=,-]{1,256}$`

// 名称字符集：档位名、参数名、环境变量名各自的合法写法。
var (
	profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	argNamePattern     = regexp.MustCompile(`^[a-z0-9_]+$`)
	envNamePattern     = regexp.MustCompile(`^[A-Z0-9_]+$`)
	placeholderPattern = regexp.MustCompile(`\{([A-Za-z0-9_-]*)\}?`)
)

// httpMethods 是允许档位声明的方法集合。GET/DELETE 带请求体没有意义，由 E15 在使用处再约束。
var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// httpBodyModes 是 payload.body 的处理方式。
var httpBodyModes = map[string]bool{"": true, "json": true, "raw": true, "none": true}

// ArgSpec 是校验过的参数声明。
type ArgSpec struct {
	Name        string
	Required    bool
	Default     string
	AllowDash   bool
	Secret      bool
	Pattern     *regexp.Regexp // 永不 nil：配置留空时已套用 DefaultArgPattern
	PatternText string         // 实际生效的正则，用于错误信息展示
}

// PositionalSpec 是校验过的位置参数规则。
// 值本身是否以 "-" 开头属于取值判断，由 E08 在使用处执行。
type PositionalSpec struct {
	Max         int
	Pattern     *regexp.Regexp
	PatternText string
}

// Profile 是一个通过全部启动期校验的档位，字段可直接使用，不需要再判合法性。
//
// 文件是否存在、程序是否在 PATH 里不属于本结构的职责：那决定的是"当前这台机器能不能跑"，
// 由探测（TASK-E03）负责，探测失败让档位标记为不可用，而不是让进程启动失败。
type Profile struct {
	Name      string
	Kind      Kind
	Workspace string

	// script 档位
	Runtime    string
	ScriptPath string // 绝对路径
	ScriptRel  string // 相对 workspace 的写法，供接口与日志展示

	// binary 档位
	ProgramName string // program 命中 runtime_allow 时的程序名（PATH 查找交给探测）
	ProgramPath string // program 是 workspace 内路径时的绝对路径
	ProgramRel  string // program 是 workspace 内路径时的相对写法
	FixedArgs   []string

	// 参数与执行约束
	Args        []ArgSpec
	ArgsRender  []string
	Positional  *PositionalSpec
	CwdPath     string
	CwdRel      string
	Env         map[string]string
	EnvAllow    []string
	Timeout     time.Duration
	MaxParallel int
	RetryOnExit []int

	// http 档位
	Method          string
	URLTemplate     string
	AllowedHosts    []string
	Headers         map[string][]string
	HeaderAllow     []string
	Body            string
	ExpectStatus    []int
	CaptureResponse bool
	MaxBodyBytes    int
	DenyPrivate     bool
}

// HandlerKey 返回注册进调度器的键。
func (p *Profile) HandlerKey() string { return HandlerKeyPrefix + p.Name }

// Arg 按名字取参数声明。
func (p *Profile) Arg(name string) (ArgSpec, bool) {
	for _, a := range p.Args {
		if a.Name == name {
			return a, true
		}
	}
	return ArgSpec{}, false
}

// HasSecretArgs 判断档位是否声明了至少一个 secret 参数。
//
// 读取接口用它决定两件事：要不要掩码，以及结果端点要不要把档位从 reader 收严
// （TASK-E16 §3.3）。判据只看声明，不看某一次提交有没有真的传了值——
// 收严档位是给"这个档位可能带凭据"这一类任务用的，不该随 payload 变。
func (p *Profile) HasSecretArgs() bool {
	for _, a := range p.Args {
		if a.Secret {
			return true
		}
	}
	return false
}

// ProgramDisplay 返回给接口与日志看的程序写法：相对 workspace 的路径优先，
// 其次是 PATH 程序名。绝对路径不外露，避免把服务器目录结构透给前端。
func (p *Profile) ProgramDisplay() string {
	switch {
	case p.ScriptRel != "":
		return p.ScriptRel
	case p.ProgramRel != "":
		return p.ProgramRel
	default:
		return p.ProgramName
	}
}

// LoadProfiles 校验 core 配置里的全部档位。
// 任一条不过即返回错误，错误信息以 executors.commands[i] 开头并带上档位名，
// 让运维能直接定位到是哪一条哪一项写错。档位列表为空是合法的（返回空切片）。
func LoadProfiles(cfg core.Config) ([]*Profile, error) {
	normalized := cfg.Normalized()
	ec := normalized.Executors

	if len(ec.Commands) == 0 {
		return nil, nil
	}

	workspace, err := resolveWorkspace(ec.Workspace)
	if err != nil {
		return nil, fmt.Errorf("executors.workspace %q is unusable as a profile root: %w", ec.Workspace, err)
	}

	runtimes := make(map[string]bool, len(ec.RuntimeAllow))
	for _, name := range ec.RuntimeAllow {
		runtimes[name] = true
	}

	profiles := make([]*Profile, 0, len(ec.Commands))
	declared := make(map[string]int, len(ec.Commands))
	for i := range ec.Commands {
		profile, err := buildProfile(&ec.Commands[i], i, workspace, runtimes, ec)
		if err != nil {
			return nil, err
		}
		if prev, dup := declared[profile.Name]; dup {
			return nil, profileError(i, profile.Name,
				"duplicate profile name (already declared at executors.commands[%d])", prev)
		}
		declared[profile.Name] = i
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// buildProfile 按第 3.4 节的规则校验单条档位。
func buildProfile(cmd *core.ExecutorCommand, index int, workspace string, runtimes map[string]bool, ec core.ExecutorsConfig) (*Profile, error) {
	if err := checkProfileName(cmd.Name, index); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(cmd.Name)

	kind := Kind(strings.TrimSpace(cmd.Kind))
	switch kind {
	case KindScript, KindBinary, KindHTTP:
	default:
		return nil, profileError(index, name,
			"kind %q is invalid, use script, binary or http", cmd.Kind)
	}

	if err := checkFieldsMatchKind(cmd, kind, index, name); err != nil {
		return nil, err
	}

	profile := &Profile{
		Name:            name,
		Kind:            kind,
		Workspace:       workspace,
		FixedArgs:       append([]string(nil), cmd.FixedArgs...),
		ArgsRender:      append([]string(nil), cmd.ArgsRender...),
		Env:             make(map[string]string, len(cmd.Env)),
		EnvAllow:        append([]string(nil), cmd.EnvAllow...),
		MaxParallel:     cmd.MaxParallel,
		RetryOnExit:     append([]int(nil), cmd.RetryOnExit...),
		AllowedHosts:    append([]string(nil), cmd.AllowedHosts...),
		HeaderAllow:     append([]string(nil), cmd.HeaderAllow...),
		Headers:         cmd.Headers,
		URLTemplate:     strings.TrimSpace(cmd.URLTemplate),
		Body:            strings.TrimSpace(cmd.Body),
		ExpectStatus:    append([]int(nil), cmd.ExpectStatus...),
		CaptureResponse: cmd.CaptureResponse,
		MaxBodyBytes:    cmd.MaxBodyBytes,
		DenyPrivate:     cmd.DenyPrivate == nil || *cmd.DenyPrivate,
	}

	if cmd.MaxParallel < 0 {
		return nil, profileError(index, name, "max_parallel must not be negative, got %d", cmd.MaxParallel)
	}
	if cmd.MaxParallel == 0 {
		profile.MaxParallel = 1
	}

	// 参数声明与渲染模板
	args, err := checkArgs(cmd.Args, index, name)
	if err != nil {
		return nil, err
	}
	profile.Args = args
	if err := checkArgsRender(profile.ArgsRender, args, index, name); err != nil {
		return nil, err
	}
	if cmd.Positional != nil {
		positional, err := checkPositional(cmd.Positional, index, name)
		if err != nil {
			return nil, err
		}
		profile.Positional = positional
	}

	// 环境变量、工作目录与超时
	for key, value := range cmd.Env {
		// viper 解码映射时会把键统一折成小写，所以配置里写的 REPORT_HOME 到这里已经是 report_home。
		// 环境变量名按惯例与 envNamePattern 都要求大写，configs/config.example.yaml 的示例也是大写：
		// 折回大写再校验，校验规则本身没变，变的只是"读进来时已被改写的写法"。
		envKey := strings.ToUpper(key)
		if err := checkEnvEntry(envKey, value, index, name); err != nil {
			return nil, err
		}
		profile.Env[envKey] = value
	}
	if err := checkNameList("env_allow", profile.EnvAllow, index, name); err != nil {
		return nil, err
	}
	for _, key := range profile.EnvAllow {
		if hasCredentialPrefix(key) {
			return nil, profileError(index, name,
				"env_allow must not contain %q: GODELAYQ_ prefixed keys hold server credentials", key)
		}
	}
	if err := checkFixedArgs(profile.FixedArgs, index, name); err != nil {
		return nil, err
	}

	cwd, err := resolveInside(workspace, cmd.Cwd, "cwd", index, name)
	if err != nil {
		return nil, err
	}
	profile.CwdPath = cwd
	profile.CwdRel = relativeTo(workspace, cwd)

	timeout, err := effectiveTimeout(cmd.Timeout, ec, index, name)
	if err != nil {
		return nil, err
	}
	profile.Timeout = timeout

	// 分档位的专属字段
	switch kind {
	case KindScript:
		if !runtimes[cmd.Runtime] {
			return nil, profileError(index, name,
				"runtime %q is not in executors.runtime_allow", cmd.Runtime)
		}
		profile.Runtime = cmd.Runtime
		script, err := resolveInside(workspace, cmd.Script, "script", index, name)
		if err != nil {
			return nil, err
		}
		profile.ScriptPath = script
		profile.ScriptRel = relativeTo(workspace, script)

	case KindBinary:
		// program 的两种写法：精确匹配 runtime_allow 视为 PATH 里的程序名，
		// 否则按 workspace 内的相对路径处理。歧义（例如 workspace 里正好有个叫 node 的文件）
		// 由配置作者用命名消除，这里保持单一判定顺序。
		if runtimes[cmd.Program] {
			profile.ProgramName = cmd.Program
		} else {
			program, err := resolveInside(workspace, cmd.Program, "program", index, name)
			if err != nil {
				return nil, err
			}
			profile.ProgramPath = program
			profile.ProgramRel = relativeTo(workspace, program)
		}

	case KindHTTP:
		if err := fillHTTPProfile(profile, cmd, ec, index, name); err != nil {
			return nil, err
		}
	}

	return profile, nil
}

// fillHTTPProfile 校验并填充 http 档位的专属字段。
func fillHTTPProfile(profile *Profile, cmd *core.ExecutorCommand, ec core.ExecutorsConfig, index int, name string) error {
	method := strings.TrimSpace(cmd.Method)
	if method != strings.ToUpper(method) {
		// 严格到大写：让同一份配置只有一种写法，避免 "post" 与 "POST" 在文档、告警与
		// 前端展示里被当成两个不同的档位
		return profileError(index, name, "method %q must be upper-case, e.g. POST", cmd.Method)
	}
	if !httpMethods[method] {
		return profileError(index, name, "method %q is invalid, use GET, POST, PUT, PATCH or DELETE", cmd.Method)
	}
	profile.Method = method

	if method != "GET" && method != "DELETE" {
		// 有请求体的方法必须说明 body 来源，否则"忘记发体"和"故意不发体"看起来一样
		if profile.Body == "" {
			return profileError(index, name, "body is required for method %s, use json, raw or none", method)
		}
	}
	if !httpBodyModes[profile.Body] {
		return profileError(index, name, "body %q is invalid, use json, raw or none", cmd.Body)
	}

	for _, status := range profile.ExpectStatus {
		if status < 100 || status > 599 {
			return profileError(index, name, "expect_status must be between 100 and 599, got %d", status)
		}
	}
	if cmd.MaxRedirects != 0 {
		// 跟随重定向会让 allowed_hosts 失去意义：目标地址由对方返回的 Location 决定
		return profileError(index, name,
			"max_redirects must be 0 (redirects are never followed), got %d", cmd.MaxRedirects)
	}
	if profile.MaxBodyBytes < 0 {
		return profileError(index, name, "max_body_bytes must not be negative, got %d", profile.MaxBodyBytes)
	}
	if profile.MaxBodyBytes == 0 {
		profile.MaxBodyBytes = ec.Output.MaxBytes
	}

	if err := checkAllowedHosts(profile.AllowedHosts, index, name); err != nil {
		return err
	}
	if err := checkNameList("header_allow", profile.HeaderAllow, index, name); err != nil {
		return err
	}
	if !profile.DenyPrivate {
		for _, host := range profile.AllowedHosts {
			if strings.HasPrefix(host, "*.") {
				return profileError(index, name,
					"deny_private_ranges false requires concrete hosts, wildcard %q is not allowed", host)
			}
		}
	}

	if err := checkURLTemplate(profile.URLTemplate, profile.AllowedHosts, profile.Args, index, name); err != nil {
		return err
	}
	return nil
}

// checkProfileName 校验档位名：字符集与长度。
func checkProfileName(raw string, index int) error {
	name := strings.TrimSpace(raw)
	if name == "" {
		return fmt.Errorf("executors.commands[%d]: name must not be empty", index)
	}
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("executors.commands[%d] %q: name must be 1-64 characters of letters, digits, underscore or hyphen", index, name)
	}
	return nil
}

// checkFieldsMatchKind 拒绝"填了不属于该 kind 的字段"。
// 留空是唯一的"不使用"表达：残留的值要么被后面的执行逻辑读到，
// 要么让运维误以为某个开关已经生效。必填项在这里一并检查。
func checkFieldsMatchKind(cmd *core.ExecutorCommand, kind Kind, index int, name string) error {
	reject := func(present bool, field, owner string) error {
		if !present {
			return nil
		}
		return profileError(index, name, "%s belongs to %s profiles, not %s", field, owner, kind)
	}
	var err error

	if kind != KindScript {
		if err = reject(cmd.Runtime != "", "runtime", "script"); err != nil {
			return err
		}
		if err = reject(cmd.Script != "", "script", "script"); err != nil {
			return err
		}
	}
	if kind != KindBinary {
		if err = reject(cmd.Program != "", "program", "binary"); err != nil {
			return err
		}
		if err = reject(len(cmd.FixedArgs) > 0, "fixed_args", "binary"); err != nil {
			return err
		}
	}
	if kind == KindHTTP {
		processFields := []struct {
			field   string
			present bool
		}{
			{"args_render", len(cmd.ArgsRender) > 0},
			{"positional", cmd.Positional != nil},
			{"env", len(cmd.Env) > 0},
			{"env_allow", len(cmd.EnvAllow) > 0},
			{"retry_on_exit", len(cmd.RetryOnExit) > 0},
			{"cwd", cmd.Cwd != ""},
		}
		for _, f := range processFields {
			if err = reject(f.present, f.field, "script/binary"); err != nil {
				return err
			}
		}
	}
	if kind != KindHTTP {
		httpFields := []struct {
			field   string
			present bool
		}{
			{"method", cmd.Method != ""},
			{"url_template", cmd.URLTemplate != ""},
			{"allowed_hosts", len(cmd.AllowedHosts) > 0},
			{"headers", len(cmd.Headers) > 0},
			{"header_allow", len(cmd.HeaderAllow) > 0},
			{"body", cmd.Body != ""},
			{"expect_status", len(cmd.ExpectStatus) > 0},
			{"capture_response", cmd.CaptureResponse},
			{"max_body_bytes", cmd.MaxBodyBytes != 0},
			{"max_redirects", cmd.MaxRedirects != 0},
			{"deny_private_ranges", cmd.DenyPrivate != nil},
		}
		for _, f := range httpFields {
			if err = reject(f.present, f.field, "http"); err != nil {
				return err
			}
		}
	}

	switch kind {
	case KindScript:
		if cmd.Script == "" {
			return profileError(index, name, "script is required for the script kind")
		}
	case KindBinary:
		if cmd.Program == "" {
			return profileError(index, name, "program is required for the binary kind")
		}
	case KindHTTP:
		if cmd.URLTemplate == "" {
			return profileError(index, name, "url_template is required for the http kind")
		}
	}
	return nil
}

// checkArgs 校验参数声明并预编译正则。
func checkArgs(declared []core.ExecutorArg, index int, name string) ([]ArgSpec, error) {
	args := make([]ArgSpec, 0, len(declared))
	seen := make(map[string]bool, len(declared))
	for i, arg := range declared {
		argName := strings.TrimSpace(arg.Name)
		if !argNamePattern.MatchString(argName) {
			return nil, profileError(index, name,
				"args[%d] name %q must be lower-case letters, digits or underscore", i, arg.Name)
		}
		if argName == positionalKey {
			// 保留键给位置参数用（见 args.go 的 takePositional）。允许声明这个名字的话，
			// 一个数组值会撞上"具名参数的值只能是字符串或数字"这条规则，报错看不出为什么。
			return nil, profileError(index, name,
				"args[%d] %q is reserved for positional values and cannot be declared", i, argName)
		}
		if seen[argName] {
			return nil, profileError(index, name, "args[%d] %q is declared twice", i, argName)
		}
		seen[argName] = true

		patternText := strings.TrimSpace(arg.Pattern)
		if patternText == "" {
			patternText = DefaultArgPattern
		}
		pattern, err := regexp.Compile(patternText)
		if err != nil {
			return nil, profileError(index, name, "args[%d] %q pattern is not a valid regexp: %v", i, argName, err)
		}
		if arg.Required && arg.Default != "" {
			return nil, profileError(index, name,
				"args[%d] %q cannot be both required and carry a default value", i, argName)
		}
		if arg.Default != "" && !pattern.MatchString(arg.Default) {
			return nil, profileError(index, name,
				"args[%d] %q default %q does not match its pattern %s", i, argName, arg.Default, patternText)
		}
		args = append(args, ArgSpec{
			Name: argName, Required: arg.Required, Default: arg.Default,
			AllowDash: arg.AllowDash, Secret: arg.Secret,
			Pattern: pattern, PatternText: patternText,
		})
	}
	return args, nil
}

// checkArgsRender 校验渲染模板：占位符必须已声明、括号必须闭合、不含 shell 元字符。
//
// 元字符这条不是因为执行要过 shell（它不过），而是为了让"以后加 shell 通道"这个念头
// 在配置层就先付出代价：模板里写了 $ 或 ; 的档位，一旦哪天真的过 shell 就直接变成可执行内容。
// 删除这条之前要先改设计文档。
func checkArgsRender(render []string, args []ArgSpec, index int, name string) error {
	declared := make(map[string]bool, len(args))
	for _, a := range args {
		declared[a.Name] = true
	}

	for i, item := range render {
		if strings.TrimSpace(item) == "" {
			return profileError(index, name, "args_render[%d] must not be empty", i)
		}
		if bad := firstShellSpecial(item); bad != "" {
			return profileError(index, name,
				"args_render[%d] %q contains the shell special character %q; templates are rendered into argv, this is not a shell channel", i, item, bad)
		}
		if err := checkBraces(item); err != nil {
			return fmt.Errorf("executors.commands[%d] %q args_render[%d]: %w", index, name, i, err)
		}
		for _, match := range placeholderPattern.FindAllStringSubmatch(item, -1) {
			if !declared[match[1]] {
				return profileError(index, name,
					"args_render[%d] %q references %q which is not declared in args", i, item, match[1])
			}
		}
	}
	return nil
}

// checkBraces 检查花括号成对：未闭合的 { 或多余的 } 都说明模板写错。
func checkBraces(item string) error {
	depth := 0
	for _, r := range item {
		switch r {
		case '{':
			if depth != 0 {
				return fmt.Errorf("nested { is not supported")
			}
			depth = 1
		case '}':
			if depth == 0 {
				return fmt.Errorf("unexpected } without a matching {")
			}
			depth = 0
		}
	}
	if depth != 0 {
		return fmt.Errorf("unclosed { in template")
	}
	return nil
}

// checkPositional 校验位置参数规则并预编译正则。
// 值本身是否以 "-" 开头属于取值判断，由 E08 在使用处执行。
func checkPositional(spec *core.ExecutorPositional, index int, name string) (*PositionalSpec, error) {
	if spec.Max < 1 || spec.Max > 16 {
		return nil, profileError(index, name, "positional.max must be between 1 and 16, got %d", spec.Max)
	}
	patternText := strings.TrimSpace(spec.Pattern)
	if patternText == "" {
		patternText = DefaultArgPattern
	}
	pattern, err := regexp.Compile(patternText)
	if err != nil {
		return nil, profileError(index, name, "positional.pattern is not a valid regexp: %v", err)
	}
	return &PositionalSpec{Max: spec.Max, Pattern: pattern, PatternText: patternText}, nil
}

func checkEnvEntry(key, value string, index int, name string) error {
	if !envNamePattern.MatchString(key) {
		return profileError(index, name, "env key %q must be upper-case letters, digits or underscore", key)
	}
	if hasCredentialPrefix(key) {
		return profileError(index, name,
			"env must not set %q: GODELAYQ_ prefixed keys hold server credentials", key)
	}
	if containsControl(value) {
		return profileError(index, name, "env value for %q must not contain control characters", key)
	}
	return nil
}

// checkNameList 校验白名单列表项：不能空白、不能首尾带空格、不能含控制字符。
// 这三种写法都会造出一个"配了但永远匹配不上"的项，比启动即报错更难排查。
func checkNameList(field string, list []string, index int, name string) error {
	for _, item := range list {
		if strings.TrimSpace(item) == "" {
			return profileError(index, name, "%s must not contain an empty entry", field)
		}
		if item != strings.TrimSpace(item) {
			return profileError(index, name, "%s entries must not have surrounding spaces, got %q", field, item)
		}
		if containsControl(item) {
			return profileError(index, name, "%s entries must not contain control characters, got %q", field, item)
		}
	}
	return nil
}

func checkFixedArgs(args []string, index int, name string) error {
	for i, arg := range args {
		if containsControl(arg) {
			return profileError(index, name, "fixed_args[%d] must not contain control characters", i)
		}
	}
	return nil
}

// checkAllowedHosts 校验主机白名单的写法。
func checkAllowedHosts(hosts []string, index int, name string) error {
	if len(hosts) == 0 {
		return profileError(index, name, "allowed_hosts must not be empty for the http kind")
	}
	for i, raw := range hosts {
		host := strings.TrimSpace(raw)
		if host == "" || host != raw {
			return profileError(index, name, "allowed_hosts[%d] %q must not be empty or padded", i, raw)
		}
		if strings.Contains(host, "://") {
			return profileError(index, name, "allowed_hosts[%d] %q must be a host, not a URL", i, host)
		}
		if strings.ContainsAny(host, "/?# @") {
			return profileError(index, name, "allowed_hosts[%d] %q must not contain a path, query or credentials", i, host)
		}
		if err := checkHostPattern(host); err != nil {
			return fmt.Errorf("executors.commands[%d] %q allowed_hosts[%d] %q: %w", index, name, i, host, err)
		}
	}
	return nil
}

// checkHostPattern 允许三种写法：普通主机名、host:port、*.<后缀>。
// 裸 "*" 与其它位置的 "*" 都拒绝——前者等于不设限，后者会匹配到 evil.example 这类域名。
func checkHostPattern(host string) error {
	name, port, hasPort := strings.Cut(host, ":")
	if hasPort {
		parsed, err := strconv.Atoi(port)
		if err != nil || parsed < 1 || parsed > 65535 {
			return fmt.Errorf("port %q is not a number between 1 and 65535", port)
		}
	}

	prefix := ""
	if strings.HasPrefix(name, "*.") {
		prefix, name = "*.", name[2:]
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("only the leading \"*.\" wildcard form is allowed")
	}
	if prefix == "*." && name == "" {
		return fmt.Errorf("wildcard needs a suffix, e.g. *.example.com")
	}
	if !isValidHostname(name) {
		return fmt.Errorf("%q is not a valid hostname", name)
	}
	return nil
}

func isValidHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	if net.ParseIP(name) != nil {
		// 直接写 IP 属于合法配置，但必须显式出现在 allowed_hosts 里（见设计文档 §5.4：
		// 写 IP 不能绕过地址范围判断，那一条在执行期检查）
		return true
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !letter && !(r == '-' && i != 0 && i != len(label)-1) {
				return false
			}
		}
	}
	return true
}

// checkURLTemplate 校验 URL 写法与占位符，并要求主机命中白名单。
func checkURLTemplate(raw string, allowedHosts []string, args []ArgSpec, index int, name string) error {
	if raw == "" {
		return profileError(index, name, "url_template must not be empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return profileError(index, name, "url_template must not contain whitespace")
	}

	// 占位符先换成一个不会引入结构的哨兵值，再交给 url.Parse 判断形状：
	// 直接解析 "{tenant}" 也能过，但主机与时序检查会因为路径里有大括号而失真。
	probe := placeholderPattern.ReplaceAllString(raw, "PLACEHOLDER")
	for _, match := range placeholderPattern.FindAllStringSubmatch(raw, -1) {
		declared := false
		for _, a := range args {
			if a.Name == match[1] {
				declared = true
				break
			}
		}
		if !declared {
			return profileError(index, name,
				"url_template references {%s} which is not declared in args", match[1])
		}
	}

	parsed, err := url.Parse(probe)
	if err != nil {
		return profileError(index, name, "url_template is not a valid URL: %v", err)
	}
	// 占位符只支持出现在路径段（TASK-E15 §3.2）。查询段的取值规则不一样：
	// 路径段里值不能含 `/ ? #`，而查询串本来就要用 `=` `&` 这类分隔符，
	// 一套字符集管两种位置迟早出错。写在配置阶段报错，比在执行阶段猜意图清楚。
	if cut := strings.IndexAny(raw, "?#"); cut >= 0 && placeholderPattern.MatchString(raw[cut:]) {
		return profileError(index, name,
			"url_template placeholders are only supported in the path segment, not in the query or fragment")
	}
	if parsed.Host == "" {
		return profileError(index, name, "url_template %q has no host", raw)
	}
	if parsed.User != nil {
		return profileError(index, name, "url_template must not carry user credentials")
	}

	switch parsed.Scheme {
	case "https":
	case "http":
		// 明文只允许本机回环，且必须显式出现在 allowed_hosts 里；
		// 其它场景一律要求 https，否则凭据与请求体在链路上是明文
		host := parsed.Hostname()
		if !net.ParseIP(host).IsLoopback() {
			return profileError(index, name,
				"url_template scheme http is only allowed for loopback hosts, got %q", host)
		}
		if !hostAllowed(parsed.Host, allowedHosts) {
			return profileError(index, name, "url_template host %q is not in allowed_hosts", parsed.Host)
		}
	default:
		return profileError(index, name, "url_template scheme %q is invalid, use https (or http for loopback)", parsed.Scheme)
	}

	if parsed.Scheme == "https" && !hostAllowed(parsed.Host, allowedHosts) {
		return profileError(index, name, "url_template host %q is not in allowed_hosts", parsed.Host)
	}
	return nil
}

// hostAllowed 判断 host（可带端口）是否命中白名单：精确匹配，或 "*." 后缀匹配。
// 这里只做字符串层面的判定，实际建连时的 IP 检查属于执行层（TASK-E15）。
func hostAllowed(host string, allowed []string) bool {
	name, port, hasPort := strings.Cut(host, ":")
	for _, entry := range allowed {
		entryName, entryPort, entryHasPort := strings.Cut(entry, ":")
		if hasPort != entryHasPort || port != entryPort {
			continue
		}
		if strings.HasPrefix(entryName, "*.") {
			suffix := entryName[1:] // 保留前面的点：只接受 x.example，不接受 evil-example
			if len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
				return true
			}
			continue
		}
		if strings.EqualFold(entryName, name) {
			return true
		}
	}
	return false
}

// effectiveTimeout 合成档位的生效超时。
func effectiveTimeout(requested time.Duration, ec core.ExecutorsConfig, index int, name string) (time.Duration, error) {
	max := ec.MaxTimeout
	if max <= 0 {
		max = core.DefaultExecMaxTimeout
	}
	if requested < 0 {
		return 0, profileError(index, name, "timeout must not be negative, got %v", requested)
	}
	if requested == 0 {
		base := ec.DefaultTimeout
		if base <= 0 {
			base = core.DefaultExecDefaultTimeout
		}
		if base > max {
			return 0, profileError(index, name,
				"executors.default_timeout %v exceeds executors.max_timeout %v", base, max)
		}
		return base, nil
	}
	if requested > max {
		return 0, profileError(index, name,
			"timeout %v exceeds executors.max_timeout %v", requested, max)
	}
	return requested, nil
}

// evalSymlinks 是给测试留的唯一注入点：Windows 上创建符号链接需要管理员权限或开发者模式，
// 没有这一步就没法在开发机上验证"链接指向 workspace 之外"这条拒绝路径。
// 生产路径永远是 filepath.EvalSymlinks。
var evalSymlinks = filepath.EvalSymlinks

// statPath 同样是为了让测试能模拟"链接存在且指向外部"的文件系统状态。
var statPath = os.Lstat

// resolveInside 把一个相对路径解析到 workspace 内，并保证写法与解析结果都不越界。
//
// 两层检查：
//  1. 词法层面拒绝绝对路径与 ".."，拼接后再比一次前缀；
//  2. 路径存在时再做一次符号链接解析，防止 workspace 里的链接指向外部文件。
//     路径不存在时不做这一步（EvalSymlinks 对不存在的路径会报错），
//     "文件缺失"由探测负责（TASK-E03），不在这里让进程启动失败。
func resolveInside(workspace, relative, field string, index int, name string) (string, error) {
	if relative == "" {
		if field == "cwd" {
			return workspace, nil
		}
		return "", profileError(index, name, "%s must not be empty", field)
	}
	cleaned := filepath.FromSlash(strings.TrimSpace(relative))
	// 绝对路径的判定各平台不同（Windows 的 IsAbs 要求盘符或 UNC，"/tmp/x" 在那边只算"根目录相对"），
	// 所以先按写法拒绝任何以分隔符开头的值：它在任何平台上都不是"相对 workspace"的写法。
	if strings.HasPrefix(cleaned, string(filepath.Separator)) || strings.HasPrefix(relative, "/") || strings.HasPrefix(relative, "\\") {
		return "", profileError(index, name, "%s %q must be relative to executors.workspace, not start with a separator", field, relative)
	}
	if filepath.IsAbs(cleaned) {
		return "", profileError(index, name, "%s %q must be relative to executors.workspace", field, relative)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || strings.Contains(cleaned, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return "", profileError(index, name, "%s %q must not climb out of executors.workspace", field, relative)
	}

	joined, err := filepath.Abs(filepath.Join(workspace, cleaned))
	if err != nil {
		return "", profileError(index, name, "%s %q cannot be resolved: %v", field, relative, err)
	}
	if !withinDirectory(workspace, joined) {
		return "", profileError(index, name, "%s %q resolves outside executors.workspace", field, relative)
	}
	if info, statErr := statPath(joined); statErr == nil {
		// 只在路径存在时做符号链接复核：EvalSymlinks 对不存在的路径会报错，
		// 而"文件还没部署"应该由探测给出可解释的不可用状态，不是让进程起不来。
		if real, linkErr := evalSymlinks(joined); linkErr == nil && !withinDirectory(workspace, real) {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", profileError(index, name,
					"%s %q is a symlink pointing outside executors.workspace", field, relative)
			}
			return "", profileError(index, name,
				"%s %q resolves outside executors.workspace", field, relative)
		}
	}
	return filepath.Clean(joined), nil
}

// resolveWorkspace 取 workspace 的绝对形式；目录存在时一并解析符号链接，
// 这样后面的成员检查比较的是真实根目录。目录还不存在不算错误。
func resolveWorkspace(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("must not be empty")
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "", err
	}
	if real, linkErr := evalSymlinks(abs); linkErr == nil {
		return filepath.Clean(real), nil
	}
	return abs, nil
}

// withinDirectory 判断 target 是否等于或在 root 之内。
// root 补上结尾分隔符再比较，避免 ./work 误配 ./workspace。
// Windows 的文件系统不区分大小写，那里改用不区分大小写的比较。
func withinDirectory(root, target string) bool {
	if equalsPath(root, target) {
		return true
	}
	prefix := root + string(filepath.Separator)
	if runtime.GOOS == "windows" {
		return len(target) >= len(prefix) &&
			strings.EqualFold(target[:len(prefix)], prefix)
	}
	return strings.HasPrefix(target, prefix)
}

func equalsPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// relativeTo 把绝对路径换算成相对 workspace 的写法（正斜杠），供接口与日志展示。
func relativeTo(workspace, absolute string) string {
	rel, err := filepath.Rel(workspace, absolute)
	if err != nil {
		return filepath.Base(absolute)
	}
	return filepath.ToSlash(rel)
}

// ValidatePayloadKeys 检查 payload 里出现的顶层键是否属于该档位。
//
// 本方法只看键名，值本身的校验属于 E08，因此提交路径用的是 ValidateSubmission
// （它内部调本方法再往下走值检查）。单列出来是给只需要键名的调用方用的：
// 错误文案要列出"这个档位接受哪几个键"，而那与执行逻辑无关。
func (p *Profile) ValidatePayloadKeys(keys []string) error {
	var allowed []string
	switch p.Kind {
	case KindHTTP:
		allowed = []string{"params", "headers", "body", "timeout"}
	default:
		allowed = []string{"args", "env", "timeout"}
	}

	for _, key := range keys {
		known := false
		for _, a := range allowed {
			if a == key {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("payload key %q is not accepted by profile %q (allowed: %s)",
				key, p.Name, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// profileError 统一错误前缀，让每条信息都能定位到 executors.commands 的哪一项。
func profileError(index int, name, format string, args ...any) error {
	if name == "" {
		return fmt.Errorf("executors.commands[%d]: %s", index, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("executors.commands[%d] %q: %s", index, name, fmt.Sprintf(format, args...))
}

// firstShellSpecial 返回字符串里出现的第一个 shell 元字符，没有则返回空串。
func firstShellSpecial(item string) string {
	for _, r := range item {
		if strings.ContainsRune("\\;|&`$><\n\r", r) {
			return strconv.QuoteRune(r)
		}
	}
	return ""
}

func containsControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r)
	})
}

func hasCredentialPrefix(key string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(key)), "GODELAYQ_")
}
