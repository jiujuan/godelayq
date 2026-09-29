package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"godelayq/core"
)

// Submission 是 payload 解码之后的提交内容，只用来校验与渲染，不进任务快照
// （快照里存的是原始 payload 字节，见 core.JobSnapshot.Payload）。
//
// 它的字段集合就是 payload 允许的键集合：解码时按这张表逐个筛键，
// 所以 {"cmd":"ls"}、{"script":"evil.sh"}、{"url":"http://internal"} 这类"自己指定执行内容"的写法
// 在第一步就被拒绝，不需要等档位解释。这是设计文档 §2 的 D1/D2 两条口径的落地位置。
type Submission struct {
	// Args 是 script/binary 档位的具名参数值，键必须是档位声明过的参数名。
	Args map[string]string

	// Positional 是位置参数，值来自 payload 的 args._positional 保留键。
	// 单列一个字段而不是塞回 Args：Args 的值类型限定为字符串与数字，
	// 位置参数是一组字符串；线上格式仍与卡片一致（args 里的保留键）。
	Positional []string

	// Env 是 payload 请求注入的环境变量，键必须在档位的 env_allow 里。
	Env map[string]string

	// Params 是 http 档位的 URL 占位符值（占位符同样声明在 args 里）。
	Params map[string]string

	// Headers 是 payload 请求覆盖的请求头，键必须在档位的 header_allow 里。
	Headers map[string]string

	// Body 是 http 档位的请求体原文，形态由档位的 body 模式决定。
	Body json.RawMessage

	// Timeout 是 payload 请求的单次执行超时，写法同 Go 的时长字面量（"90s"）。
	Timeout string

	// TimeoutValue 是 Timeout 解析之后的时长，0 表示 payload 没请求。
	// 单列一个字段而不是让执行侧把文本再解析一遍：写法与上限已在同一次校验里判过，
	// 解析在这里不可能失败，执行侧拿到的是判过上限的那个值。
	TimeoutValue time.Duration
}

// positionalKey 是 args 里承载位置参数的保留键。它不是档位能声明的参数名
// （checkArgs 会拒绝声明这个名字），所以不会与具名参数撞车。
const positionalKey = "_positional"

// 提交内容的尺寸与字符规则。上限的存在是为了让"一次提交"有可预期的边界，
// 而不是等到落盘或执行时才发现问题。
const (
	// maxArgValueLen 是单个参数值的长度上限（按字符数，不是字节数）。
	maxArgValueLen = 256
	// maxSubmissionEnvEntries 是一次提交能注入的环境变量条数上限。
	maxSubmissionEnvEntries = 16
)

// ErrWrongKind 表示档位类型不走这条路径：http 档位没有命令行。
var ErrWrongKind = errors.New("profile kind does not produce a command line")

// submissionFieldNames 是 payload 允许的顶层键集合。档位类型还会进一步收窄它
// （见 ValidateSubmission 的 kind 分支），这里管的是"这个键根本不该出现在 payload 里"。
var submissionFieldNames = map[string]bool{
	"args": true, "env": true, "params": true, "headers": true, "body": true, "timeout": true,
}

// decodeStrict 解码一段 JSON 并给出可用的错误信息。
//
// 顶层键由调用方按 submissionFieldNames 逐个筛，不用 DisallowUnknownFields：
// 标准库报的是 json: unknown field "cmd"，键名在里面但说法对调用方没有意义，
// 而且它一遇错就整站停住，报不出"还有别的键也不该出现"。
// 数字不需要 UseNumber 保形：每个值都以 json.RawMessage 到手，原文自然留住。
func decodeStrict(raw json.RawMessage, target any, field string) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("payload key %q could not be decoded: %v", field, err)
	}
	return nil
}

// ValidateSubmission 按档位校验 payload，产出可直接用于 Render 的提交内容。
//
// 空 payload（缺省、null、{}）是合法的：取值全部来自档位声明的 default；
// 只有当档位还有 required 参数时才会因此报错，错误里列出缺哪一个。
//
// 返回的错误文本面向 HTTP 400 的 details（TASK-E16 使用），所以每条都带
// "哪个键、为什么、允许什么"；secret 参数的值一律换成 <redacted>，
// 校验不过也一样——错误信息是响应体的一部分，泄漏不该取决于成功还是失败。
func ValidateSubmission(p *Profile, payload []byte) (*Submission, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("payload must be one JSON object: %v", err)
	}
	for key := range fields {
		if !submissionFieldNames[key] {
			return nil, fmt.Errorf("payload key %q is not accepted by profile %q (allowed keys: %s)",
				key, p.Name, strings.Join(submissionKeys(p), ", "))
		}
	}

	sub := &Submission{
		Args:       map[string]string{},
		Params:     map[string]string{},
		Positional: []string{},
	}
	var rawArgs, rawParams map[string]json.RawMessage
	if err := decodeSubmissionFields(fields, sub, &rawArgs, &rawParams); err != nil {
		return nil, err
	}

	switch p.Kind {
	case KindHTTP:
		if len(rawArgs) > 0 || len(sub.Env) > 0 {
			return nil, fmt.Errorf("args and env belong to script/binary profiles, not http")
		}
		if err := fillValues(p, rawParams, sub.Params, "params"); err != nil {
			return nil, err
		}
		if err := checkSubmissionHeaders(p, sub.Headers); err != nil {
			return nil, err
		}
		if err := checkHTTPBody(p, sub.Body); err != nil {
			return nil, err
		}
	default:
		if len(rawParams) > 0 || len(sub.Headers) > 0 || len(sub.Body) > 0 {
			return nil, fmt.Errorf("params, headers and body belong to http profiles, not %s", p.Kind)
		}
		if err := fillValues(p, rawArgs, sub.Args, "args"); err != nil {
			return nil, err
		}
		positional, err := takePositional(p, rawArgs)
		if err != nil {
			return nil, err
		}
		sub.Positional = positional
		if err := checkSubmissionEnv(p, sub.Env); err != nil {
			return nil, err
		}
	}

	// 超时的两条规则在同一次解析里检查：写法是否合法、有没有超过档位允许的上限。
	// 解析结果留在 Submission 里给执行侧用，文本与值不各解析一遍。
	requested, err := checkTimeoutWithinProfile(p, sub.Timeout)
	if err != nil {
		return nil, err
	}
	sub.TimeoutValue = requested
	return sub, nil
}

// decodeSubmissionFields 把已经筛过键名的 payload 分到各自的容器里。
// 具名参数（args/params）的值留成 RawMessage，才能区分字符串与数字并拒绝其它类型。
func decodeSubmissionFields(fields map[string]json.RawMessage, sub *Submission,
	rawArgs, rawParams *map[string]json.RawMessage) error {
	for key, raw := range fields {
		var target any
		switch key {
		case "args":
			target = rawArgs
		case "params":
			target = rawParams
		case "env":
			target = &sub.Env
		case "headers":
			target = &sub.Headers
		case "timeout":
			target = &sub.Timeout
		case "body":
			if string(raw) != "null" {
				sub.Body = raw
			}
			continue
		}
		if err := decodeStrict(raw, target, key); err != nil {
			return err
		}
	}
	return nil
}

// submissionKeys 返回该档位的 payload 允许出现的顶层键，用于错误信息。
func submissionKeys(p *Profile) []string {
	if p.Kind == KindHTTP {
		return []string{"params", "headers", "body", "timeout"}
	}
	return []string{"args", "env", "timeout"}
}

// fillValues 把 JSON 值收成字符串，并逐个过档位规则。
// source 是解码用的 map（args 或 params），prefix 用来拼错误里的键名。
func fillValues(p *Profile, source map[string]json.RawMessage, target map[string]string, prefix string) error {
	for name, encoded := range source {
		if name == positionalKey {
			// 位置参数由 takePositional 处理：它是一组值，不是"字符串或数字"
			continue
		}

		value, err := scalarArgValue(encoded)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", prefix, name, err)
		}

		arg, declared := p.Arg(name)
		if !declared {
			return fmt.Errorf("%s.%s: profile %q %s", prefix, name, p.Name, declaredArgHint(p))
		}
		if err := checkArgValue(arg, value); err != nil {
			return fmt.Errorf("%s.%s: %w", prefix, name, err)
		}
		target[name] = value
	}

	for _, arg := range p.Args {
		if arg.Required {
			if _, provided := target[arg.Name]; !provided {
				return fmt.Errorf("%s.%s: required by profile %q but not provided",
					prefix, arg.Name, p.Name)
			}
		}
	}
	return nil
}

// scalarArgValue 只接受 JSON 字符串与数字，其余类型一律拒绝。
// 布尔值看起来无害（true/false），但它意味着 payload 的结构走偏了：
// 到了命令行里它只会变成 "true" 这个词，与调用方以为的"开关"不是一回事。
func scalarArgValue(encoded json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", fmt.Errorf("value is not a valid JSON string: %v", err)
		}
		return value, nil
	}

	// 数字取的是 JSON 原文：每个值都以 RawMessage 到手，1.50 不会被浮点往返写成 1.5
	if kind := jsonKindName(trimmed); kind == "a number" {
		return string(trimmed), nil
	}
	return "", fmt.Errorf("value type is not accepted: use a string or a number, got %s", jsonKindName(trimmed))
}

// takePositional 取出 args._positional 保留键里的位置参数。
// 具名参数那一轮会跳过这个键（见 fillValues），所以它不会冒充成某个声明过的参数。
func takePositional(p *Profile, args map[string]json.RawMessage) ([]string, error) {
	encoded, present := args[positionalKey]
	if !present {
		return []string{}, nil
	}
	if p.Positional == nil {
		return nil, fmt.Errorf("args.%s: profile %q does not accept positional arguments", positionalKey, p.Name)
	}

	var values []string
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, fmt.Errorf("args.%s: must be an array of strings", positionalKey)
	}
	if len(values) > p.Positional.Max {
		return nil, fmt.Errorf("args.%s: %d values exceed the %d allowed by profile %q",
			positionalKey, len(values), p.Positional.Max, p.Name)
	}
	for i, value := range values {
		if err := checkPositionalValue(p.Positional, value); err != nil {
			return nil, fmt.Errorf("args.%s[%d]: %w", positionalKey, i, err)
		}
	}
	return values, nil
}

// checkArgValue 是具名参数取值的四条规则：长度、控制字符、模式、减号开头。
func checkArgValue(arg ArgSpec, value string) error {
	if utf8Len(value) > maxArgValueLen {
		return fmt.Errorf("value %s is %d characters, the limit is %d",
			displayArgValue(arg, value), utf8Len(value), maxArgValueLen)
	}
	if containsControl(value) {
		return fmt.Errorf("value %s must not contain control characters", displayArgValue(arg, value))
	}
	if !arg.Pattern.MatchString(value) {
		return fmt.Errorf("value %s does not match pattern %s", displayArgValue(arg, value), arg.PatternText)
	}
	if !arg.AllowDash && strings.HasPrefix(value, "-") {
		return fmt.Errorf("value %s starts with %q, which a program may read as an option; "+
			"set allow_dash on this argument if the value is meant to be a flag", displayArgValue(arg, value), "-")
	}
	return nil
}

// checkPositionalValue 与具名参数同规则，但减号开头没有例外：
// 位置参数没有名字可依循，档位也没有为它声明 allow_dash。
func checkPositionalValue(spec *PositionalSpec, value string) error {
	if utf8Len(value) > maxArgValueLen {
		return fmt.Errorf("value %q is %d characters, the limit is %d", value, utf8Len(value), maxArgValueLen)
	}
	if containsControl(value) {
		return fmt.Errorf("value %q must not contain control characters", value)
	}
	if !spec.Pattern.MatchString(value) {
		return fmt.Errorf("value %q does not match pattern %s", value, spec.PatternText)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("value %q starts with %q, which a program may read as an option", value, "-")
	}
	return nil
}

// displayArgValue 决定值以什么形态出现在错误信息里：secret 参数一律换成占位符。
func displayArgValue(arg ArgSpec, value string) string {
	if arg.Secret {
		return `"<redacted>"`
	}
	return strconv.Quote(value)
}

// checkSubmissionEnv 校验 payload 请求注入的环境变量。
//
// 三层：键名格式、必须在 env_allow 里、值不含控制字符。
// 档位自己用 env 固定下来的变量不接受覆盖——那是配置审查过的取值，
// 一次提交把它换掉等于绕过了白名单里最要紧的那部分。
func checkSubmissionEnv(p *Profile, env map[string]string) error {
	if len(env) > maxSubmissionEnvEntries {
		return fmt.Errorf("env has %d entries, the limit is %d", len(env), maxSubmissionEnvEntries)
	}
	for key, value := range env {
		if !envNamePattern.MatchString(key) {
			return fmt.Errorf("env.%s: name must be upper-case letters, digits or underscore", key)
		}
		if _, fixed := p.Env[key]; fixed {
			return fmt.Errorf("env.%s: fixed by profile %q and cannot be overridden", key, p.Name)
		}
		if !inNameList(key, p.EnvAllow) {
			return fmt.Errorf("env.%s: not in the profile's env_allow (%s)", key, strings.Join(p.EnvAllow, ", "))
		}
		if containsControl(value) {
			return fmt.Errorf("env.%s: value must not contain control characters", key)
		}
	}
	return nil
}

// checkSubmissionHeaders 校验 payload 请求覆盖的请求头：键名必须在 header_allow 里
// （HTTP 头名不区分大小写，比对也不区分），值与控制字符的规则与 env 同。
// 条数上限沿用 env 的那一个：一次提交能带来的头数量不该没有边界。
func checkSubmissionHeaders(p *Profile, headers map[string]string) error {
	if len(headers) > maxSubmissionEnvEntries {
		return fmt.Errorf("headers has %d entries, the limit is %d", len(headers), maxSubmissionEnvEntries)
	}
	for key, value := range headers {
		if key != strings.TrimSpace(key) || key == "" || strings.ContainsAny(key, ": ") {
			return fmt.Errorf("headers.%q: not a valid header name", key)
		}
		if containsControl(value) || containsControl(key) {
			return fmt.Errorf("headers.%s: name and value must not contain control characters", key)
		}
		allowed := false
		for _, candidate := range p.HeaderAllow {
			if strings.EqualFold(candidate, key) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("headers.%s: not in the profile's header_allow (%s)",
				key, strings.Join(p.HeaderAllow, ", "))
		}
	}
	return nil
}

// checkHTTPBody 只回答"这个档位让不让带请求体"，体的内容交给 E15：
// 那里才知道 method、模板渲染与响应处理。
func checkHTTPBody(p *Profile, body json.RawMessage) error {
	if len(body) == 0 {
		return nil
	}
	if p.Body == "" || p.Body == "none" {
		return fmt.Errorf("body: profile %q declares body %q, it does not accept a request body", p.Name, p.Body)
	}
	return nil
}

// ParseTimeout 解析 payload 的超时写法。空串表示"没请求"，返回 0。
// E09 与 E16 都调它，避免同一个规则在两处各写一遍。
func ParseTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("timeout %q is not a duration: %v", raw, err)
	}
	if timeout < 0 {
		return 0, fmt.Errorf("timeout %v must not be negative", timeout)
	}
	return timeout, nil
}

// checkTimeoutWithinProfile 在提交期就把超上限的请求挡掉，不做静默夹取：
// 调用方以为自己的 10 分钟生效了、实际 5 分钟被杀，比当场拒绝更难解释。
// 档位自身的 timeout 已在加载时被夹进全局 max_timeout（见 effectiveTimeout），
// 所以和档位比一次就同时守住了上限。
//
// 返回第二个值是想让调用方少解析一次：写法合法与不超过上限都判过了，
// 这个时长就是可以用的。
func checkTimeoutWithinProfile(p *Profile, raw string) (time.Duration, error) {
	requested, err := ParseTimeout(raw)
	if err != nil {
		return 0, err
	}
	if requested > p.Timeout {
		return 0, fmt.Errorf("timeout %v exceeds the %v allowed by profile %q", requested, p.Timeout, p.Name)
	}
	return requested, nil
}

// EffectiveTimeout 合成一次执行真正生效的超时：payload 请求值优先，其次档位声明值，
// 再次全局默认值，最后被全局上限夹住。
//
// 结果永远大于 0——"没填超时"不等于"无限运行"（设计文档 §2 的 D5）：
// 一个没有期限的子进程会一直占着执行池的名额，取消不掉也等不到结论。
func (p *Profile) EffectiveTimeout(requested time.Duration, cfg core.Config) time.Duration {
	return p.timeoutWithin(cfg.Normalized().Executors, requested)
}

// timeoutWithin 是 EffectiveTimeout 的实现，入参已经是归一化过的那一节配置。
//
// 执行侧（TASK-E09）手上只有 core.ExecutorsConfig——装配时整份配置归一化过一次，
// 再传 core.Config 进来只会让同一条归一化逻辑跑两遍。两条路径的合成规则必须一致，
// 所以这里只拆参数，不复制规则。
func (p *Profile) timeoutWithin(ec core.ExecutorsConfig, requested time.Duration) time.Duration {
	floor := ec.DefaultTimeout
	if floor <= 0 {
		floor = core.DefaultExecDefaultTimeout
	}
	ceiling := ec.MaxTimeout
	if ceiling <= 0 {
		ceiling = core.DefaultExecMaxTimeout
	}

	value := p.Timeout
	if value <= 0 {
		value = floor
	}
	if requested > 0 {
		value = requested
	}
	if value > ceiling {
		value = ceiling
	}
	if value <= 0 {
		// 到不了这里：三个来源都已在上面被抬成正数。留着它是因为"永远大于 0"
		// 是本函数的对外承诺，不该依赖调用方传进来的配置是否规整。
		value = core.DefaultExecDefaultTimeout
	}
	return value
}

// Render 按档位的 args_render 模板拼出 argv，包含 argv[0] 与被审过的脚本/产物路径。
//
// 返回值是 []string，直接交给 exec.Cmd 的 argv。代码里没有任何"拼成一条字符串再解析"的路径，
// 也没有 sh -c / cmd /c：这是 D3（不经过 shell）的实现位置。含空格的值仍然是一个元素，
// 这一点由 executor/args_test.go 的用例断言。
//
// payload 只提供值，不提供任何结构：模板写死在配置里，未使用的已声明参数不会多出 "--x="。
func (p *Profile) Render(sub *Submission) ([]string, error) {
	if sub == nil {
		return nil, errors.New("render: submission is required")
	}

	var argv []string
	switch p.Kind {
	case KindScript:
		// 解释器名交给执行侧（E09 可以用探测得到的绝对路径替换它）；
		// 脚本一定用绝对路径：子进程的工作目录由 cmd.Dir 决定，相对路径会指向别处。
		argv = []string{p.Runtime, p.ScriptPath}
	case KindBinary:
		if p.ProgramPath != "" {
			argv = []string{p.ProgramPath}
		} else {
			argv = []string{p.ProgramName}
		}
		argv = append(argv, p.FixedArgs...)
	case KindHTTP:
		return nil, fmt.Errorf("%w: profile %q is an http profile, its request is built by the http executor",
			ErrWrongKind, p.Name)
	default:
		return nil, fmt.Errorf("%w: profile %q has unknown kind %q", ErrWrongKind, p.Name, p.Kind)
	}

	values := p.renderValues(sub)
	for _, template := range p.ArgsRender {
		item, err := renderArgItem(p, template, values)
		if err != nil {
			return nil, err
		}
		argv = append(argv, item)
	}
	return append(argv, sub.Positional...), nil
}

// renderValues 把"payload 提供的值"与"档位声明的默认值"合成一张渲染用的表。
// 只有档位声明过的参数才会出现在这里：Render 的模板引用的键在 E02 就校验过。
func (p *Profile) renderValues(sub *Submission) map[string]string {
	values := make(map[string]string, len(p.Args))
	for _, arg := range p.Args {
		if arg.Default != "" {
			values[arg.Name] = arg.Default
		}
	}
	for name, value := range sub.Args {
		values[name] = value
	}
	return values
}

// renderArgItem 替换一项模板里的占位符。
//
// 替换之后仍留着大括号就报错：值里带 { 或 } 会让第二次替换读到别的东西，
// 而"读起来像占位符的普通文本"在参数值里没有任何合法用途。
func renderArgItem(p *Profile, template string, values map[string]string) (string, error) {
	rendered := template
	for _, match := range placeholderPattern.FindAllStringSubmatch(template, -1) {
		name := match[1]
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("args_render %q references {%s} which has no value: "+
				"profile %q declares no default and the payload did not provide it",
				template, name, p.Name)
		}
		if strings.ContainsAny(value, "{}") {
			return "", fmt.Errorf("args_render %q: value of {%s} contains a brace, which would be read as a placeholder",
				template, name)
		}
		rendered = strings.Replace(rendered, match[0], value, 1)
	}

	if strings.ContainsAny(rendered, "{}") {
		return "", fmt.Errorf("args_render %q still contains a brace after rendering", template)
	}
	return rendered, nil
}

// declaredArgNames 列出档位声明的参数名，供错误信息给出"允许什么"。
func declaredArgNames(p *Profile) []string {
	names := make([]string, 0, len(p.Args))
	for _, arg := range p.Args {
		names = append(names, arg.Name)
	}
	return names
}

// declaredArgHint 把"这个档位接受哪些参数"说成一句完整的话。
// 一个参数都没声明时要单独成句，否则句尾挂着空的 (declared: )，读起来像是报错本身坏了。
func declaredArgHint(p *Profile) string {
	names := declaredArgNames(p)
	if len(names) == 0 {
		return "declares no arguments, so the payload must not carry any"
	}
	return fmt.Sprintf("does not declare this argument (declared: %s)", strings.Join(names, ", "))
}

func inNameList(name string, list []string) bool {
	for _, candidate := range list {
		if candidate == name {
			return true
		}
	}
	return false
}

// utf8Len 按字符数而非字节数计长：上限想约束的是"调用方写了多少个字符"，
// 按字节数算会让中文参数值显得比实际短三分之一。
func utf8Len(value string) int {
	return len([]rune(value))
}

// jsonKindName 把一段 JSON 文本的形态说成人名，用于值类型被拒时的提示。
func jsonKindName(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "empty"
	}
	switch trimmed[0] {
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	default:
		if _, err := strconv.ParseFloat(string(trimmed), 64); err == nil {
			return "a number"
		}
		return "an unusable value"
	}
}
