package executor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"godelayq/core"
)

// 内置自由执行档位（adhoc）：四条"跑哪个文件、打到哪里由任务自己说"的档位。
//
// 它们与 executors.commands 里的档位是同一个东西——同一个构造函数、同一套探测、
// 同一个处理函数、同一个执行池——区别只有一处：位置字段不在定义里，而在任务的 payload 里。
// 走这条路而不是新造一种执行通路，是为了让并发许可、超时上限、产物落盘、失败分类、
// 重试与崩溃恢复那一大套既有能力一条都不必重写（设计文档 §D5）。
//
// 整节由 executors.adhoc.enabled 控制，默认关闭（§D11）。

// 四条内置档位的注册键，去掉 exec. 前缀之后的档位名。
const (
	AdhocPHPName     = "php"
	AdhocPythonName  = "python"
	AdhocShellName   = "shell"
	AdhocHTTPName    = "http"
	adhocLocationKey = "script"
	adhocLocationURL = "url"

	// 位置的两种形态，接口原样给前端（Profile.AdhocLocationKind）：
	// 前端据此决定输入框与提示文案，不再自己按注册键猜。
	adhocKindPath = "path"
	adhocKindURL  = "url"
)

// adhocSpec 是一条内置档位的静态定义。
type adhocSpec struct {
	// name 是档位名；注册键是 exec.<name>。
	// 档位名字符集不含点（core/executor_profile_store.go 的 profileNamePattern），
	// 所以 "exec.adhoc.php" 那种写法不合法，这里用四条短名字。
	name string
	kind Kind
	// runtime 是解释器名。空是一个占位记号：只有 shell 那一条用空串，
	// 实际取值来自 executors.adhoc.shell_runtime（见 adhocSpecs）。
	runtime string
	// extensions 是脚本位置允许的扩展名（小写、含点）；nil 表示不要求。
	extensions []string
	// locationKey 是任务写位置时用的 payload 顶层键。
	locationKey string
	// locationKind 是位置的种类：path | url，接口与界面据此决定输入框形态。
	locationKind string
	// httpMethod / httpBody 只对 http 那条有意义。
	// 默认 POST + json：健康检查与回调类需求都要能带体；
	// 只想要 GET 的部署可以照 executors.commands 另建一条普通档位（设计文档 §12 P5）。
	httpMethod string
	httpBody   string
}

// adhocSpecs 给出四条定义，顺序固定（接口与启动日志的输出因此稳定）。
func adhocSpecs(ec core.ExecutorsConfig) []adhocSpec {
	extensions := func(names ...string) []string {
		if !ec.Adhoc.WantExtensionCheck() {
			// 关掉要求时这里给空：判据只有一处，N05 那边看见空列表就是"不要求"，
			// 不必再各自读一遍配置。
			return nil
		}
		return names
	}

	return []adhocSpec{
		{
			name: AdhocPHPName, kind: KindScript, runtime: "php",
			extensions: extensions(".php"), locationKey: adhocLocationKey, locationKind: adhocKindPath,
		},
		{
			name: AdhocPythonName, kind: KindScript, runtime: "python",
			extensions: extensions(".py"), locationKey: adhocLocationKey, locationKind: adhocKindPath,
		},
		{
			// shell 那条的解释器由配置给（默认 bash），所以这里留空占位，
			// 由 AdhocProfiles 换成 executors.adhoc.shell_runtime 的取值。
			name: AdhocShellName, kind: KindScript,
			extensions: extensions(".sh", ".bash"), locationKey: adhocLocationKey, locationKind: adhocKindPath,
		},
		{
			name: AdhocHTTPName, kind: KindHTTP,
			locationKey: adhocLocationURL, locationKind: adhocKindURL,
			httpMethod: "POST", httpBody: "json",
		},
	}
}

// AdhocSkip 是一条没被构造出来的内置档位与原因。
type AdhocSkip struct {
	// HandlerKey 是那条内置档位本来要占用的注册键。
	HandlerKey string
	// Reason 是可以直接进日志与接口的说明。
	Reason string
}

// AdhocProfiles 按配置构造那四条内置档位。
//
// 返回的第二个值是"因为配置或本机环境而没构造出来的条目"，调用方负责把它说出去
// （executor.Register 的计数与启动日志，见 TASK-N04 §3.3）。
// 没构造出来不是错误：把 runtime_allow 收成只有 bash 的部署，
// 应当安静地只剩 Shell 与 HTTP 两条，而不是启动失败。
//
// 构造仍然走 buildProfile（profile.go 那一份），所以字段组合、解释器白名单、
// 超时上限、主机写法这些判据与配置文件里的档位完全相同——
// 唯一多出来的东西是 Profile.Adhoc 这个标记，它让档位定义里的位置字段可以留空
// （判据在 checkFieldsMatchKind 与 fillHTTPProfile 的 adhoc 分支，各只放宽两处）。
//
// executors.adhoc.enabled 为 false 时返回空切片：那时这四条键根本不存在，
// 接口、页面与 /job-types 都看不出任何变化（设计文档 §D11）。
func AdhocProfiles(cfg core.Config) ([]*Profile, []AdhocSkip, error) {
	normalized := cfg.Normalized()
	ec := normalized.Executors
	if !ec.Adhoc.Enabled {
		return nil, nil, nil
	}

	workspace, err := resolveWorkspace(ec.Workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("executors.workspace %q is unusable as a profile root: %w", ec.Workspace, err)
	}
	runtimes := runtimeSet(ec.RuntimeAllow)

	profiles := make([]*Profile, 0, 4)
	var skipped []AdhocSkip
	for _, spec := range adhocSpecs(ec) {
		runtime := spec.runtime
		if spec.kind == KindScript && runtime == "" {
			runtime = ec.Adhoc.ShellRuntime
		}
		if spec.kind == KindScript && !runtimes[runtime] {
			skipped = append(skipped, AdhocSkip{
				HandlerKey: HandlerKeyPrefix + spec.name,
				Reason: fmt.Sprintf("runtime %q is not in executors.runtime_allow, "+
					"the built-in profile cannot use it (add it to the allow list or use a declared profile instead)",
					runtime),
			})
			continue
		}

		command, err := adhocCommand(spec, ec, runtime)
		if err != nil {
			return nil, nil, fmt.Errorf("built-in profile %q is unusable: %w", spec.name, err)
		}
		profile, err := buildProfile(&command, singleProfileIndex, workspace, runtimes, ec, PathAnywhere, true)
		if err != nil {
			// 走到这里属于定义写错：内置条目是自己拼出来的，没有任何用户输入参与。
			// 报错而不是跳过，否则一份看起来"打开了 adhoc"的配置会少一条而无人知晓。
			return nil, nil, fmt.Errorf("built-in profile %q is unusable: %w", spec.name, err)
		}
		profile.AdhocLocationKey = spec.locationKey
		profile.AdhocLocationKind = spec.locationKind
		profile.AdhocExtensions = append([]string(nil), spec.extensions...)
		if spec.kind == KindScript {
			prefixes, err := adhocPathPrefixes(ec.Adhoc.PathPrefixes, workspace)
			if err != nil {
				return nil, nil, fmt.Errorf("built-in profile %q is unusable: %w", spec.name, err)
			}
			profile.AdhocPathPrefixes = prefixes
		}
		profiles = append(profiles, profile)
	}
	return profiles, skipped, nil
}

// adhocPathPrefixes 把 executors.adhoc.path_prefixes 换成绝对写法。
//
// 相对路径以 executors.workspace 为基准——与档位自己的 script 字段同一条基准，
// 换一根基准会让同一份配置在不同机器上指向两个目录（resolveAnywhere 的注释讲的正是这件事）。
// 空列表原样返回空：那是"不限目录"这个取值，不是"没配"。
func adhocPathPrefixes(declared []string, workspace string) ([]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	prefixes := make([]string, 0, len(declared))
	for _, raw := range declared {
		absolute, err := resolveAnywhere(workspace, raw, "path_prefixes", singleProfileIndex, AdhocShellName)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, absolute)
	}
	return prefixes, nil
}

// takeAdhocLocation 取出并校验任务给出的执行位置，结果写进 Submission。
//
// 这里是"位置"唯一的判据位置：提交期（api 的 gateExecutorSubmission）与执行期
// （Runner 与 HTTPRunner 各自再跑一遍 ValidateSubmission）走的是同一条函数，
// 所以不存在"接口放过、执行时才拒"的第二套规则。执行期再判一次不是重复劳动：
// 入队与触发之间隔着一段时间，配置与文件系统在那段时间里都会变。
//
// 非内置档位调用它只做一件事：如果 payload 里带了 script 或 url，给出可以操作的拒绝文本
// （"这两个键属于内置自由执行档位"），否则原样返回。
func takeAdhocLocation(p *Profile, fields map[string]json.RawMessage, sub *Submission) error {
	if !p.Adhoc {
		for _, key := range []string{adhocLocationKey, adhocLocationURL} {
			if _, present := fields[key]; present {
				// 文本与"未知键"那条同句式（ValidateSubmission 的 :130 一带）：
				// 让"这条档位不接受这个键"只有一种说法，用例与使用者都不必区分两种近义错误。
				return fmt.Errorf("payload key %q is not accepted by profile %q (allowed keys: %s)",
					key, p.Name, strings.Join(submissionKeys(p), ", "))
			}
		}
		return nil
	}

	for _, key := range []string{"args", "env", "params", "headers", "body", adhocLocationKey, adhocLocationURL} {
		if key == p.AdhocLocationKey {
			continue
		}
		if _, present := fields[key]; present {
			return fmt.Errorf("payload key %q is not accepted by profile %q: the built-in profile takes %q and timeout only",
				key, p.Name, p.AdhocLocationKey)
		}
	}

	raw, present := fields[p.AdhocLocationKey]
	if !present {
		return fmt.Errorf("payload must give %q: profile %q runs the file or the address the job names",
			p.AdhocLocationKey, p.Name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("payload key %q must be a string: %v", p.AdhocLocationKey, err)
	}

	switch p.AdhocLocationKind {
	case adhocKindPath:
		absolute, err := checkAdhocScript(p, value)
		if err != nil {
			return err
		}
		sub.Script = absolute
		return nil

	case adhocKindURL:
		target, err := checkAdhocURL(p, value)
		if err != nil {
			return err
		}
		sub.URL = target
		return nil

	default:
		return fmt.Errorf("profile %q has an unknown location kind %q", p.Name, p.AdhocLocationKind)
	}
}

// checkAdhocScript 判"这条任务要跑哪个文件"，返回解析后的绝对路径。
//
// 判据依次是：非空 → 无控制字符 → 无 shell 元字符 → 能算出绝对路径 →
// 落在 executors.adhoc.path_prefixes 之内（空列表=不限）→ 扩展名匹配（要求开着时）→
// 文件存在且是普通文件。每条的拒绝理由互不相同，运维从错误文本就能看出该改哪一处。
//
// 复用的判据有三条：containsControl 是档位参数值一直在用的那一条，
// resolveAnywhere 是 TASK-W02 那一份"允许本机任意路径"的解析，
// fileCheckReason 是探测用的那一份文件判据。
// shell 元字符那一条改用 firstPathSpecial（见其注释）：位置是身份不是参数值，
// Windows 的绝对路径离不开反斜杠。
//
// 文件不存在在这里就拒（400），而不是排队后失败：提交时文件不在，等到触发那一刻
// 更不可能在，早拒早好。
func checkAdhocScript(p *Profile, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("payload key %q must not be empty: profile %q needs the script to run",
			adhocLocationKey, p.Name)
	}
	if containsControl(value) {
		return "", fmt.Errorf("payload key %q must not contain control characters", adhocLocationKey)
	}
	if special := firstPathSpecial(value); special != "" {
		return "", fmt.Errorf("payload key %q must not contain %q", adhocLocationKey, special)
	}

	absolute, err := resolveAnywhere(p.Workspace, value, adhocLocationKey, singleProfileIndex, p.Name)
	if err != nil {
		return "", err
	}

	if len(p.AdhocPathPrefixes) > 0 {
		inside := false
		for _, prefix := range p.AdhocPathPrefixes {
			// 用目录边界判断而不是字符串前缀：/srv/scripts-evil/a.sh 不在 /srv/scripts 之内，
			// 而 HasPrefix 会说它在。
			if withinDirectory(prefix, absolute) {
				inside = true
				break
			}
		}
		if !inside {
			return "", fmt.Errorf("script %q is outside the directories this server allows: %s",
				displayOf(relativeTo(p.Workspace, absolute), absolute), strings.Join(p.AdhocPathPrefixes, ", "))
		}
	}

	if len(p.AdhocExtensions) > 0 {
		extension := strings.ToLower(filepath.Ext(absolute))
		if !slicesContains(p.AdhocExtensions, extension) {
			return "", fmt.Errorf("script %q has extension %q, profile %q runs %s files",
				displayOf(relativeTo(p.Workspace, absolute), absolute), extension, p.Name,
				strings.Join(p.AdhocExtensions, ", "))
		}
	}

	if reason, ok := fileCheckReason("script", absolute, relativeTo(p.Workspace, absolute)); !ok {
		return "", fmt.Errorf("payload key %q: %s", adhocLocationKey, reason)
	}
	return absolute, nil
}

// checkAdhocURL 判"这条任务要打到哪里"，返回规范化后的地址写法。
//
// 三道判据与档位模板那一条链完全相同（scheme、凭据、主机白名单），差别只在主机名单
// 来自 executors.adhoc.url_hosts 而不是档位自己的 allowed_hosts，且空列表表示不限主机。
// 地址范围守卫（回环、私网、链路本地、组播、100.64.0.0/10）不在这里判：
// DNS 结果要在真正建连前判才有意义，那一层在 HTTPRunner.allowedIP 上，本卡不动它。
//
// 错误文本不回显整条地址：地址里可能带口令，而这条错误会进响应、事件与日志
// （与 checkTarget 里"不回显 rendered"那一条同一个理由）。
func checkAdhocURL(p *Profile, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("payload key %q must not be empty: profile %q needs the address to request",
			adhocLocationURL, p.Name)
	}
	if containsControl(value) || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("payload key %q must not contain whitespace or control characters", adhocLocationURL)
	}

	target, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("payload key %q is not a valid URL", adhocLocationURL)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", fmt.Errorf("payload key %q must be an http or https URL, got scheme %q",
			adhocLocationURL, target.Scheme)
	}
	if target.Host == "" {
		return "", fmt.Errorf("payload key %q has no host", adhocLocationURL)
	}
	if target.User != nil {
		return "", fmt.Errorf("payload key %q must not carry user credentials", adhocLocationURL)
	}
	// 空列表 = 不限主机（设计文档 §D9 第②层的放宽）；非空时复用档位那一份 hostAllowed，
	// 通配后缀与端口的写法因此与 executors.commands 的 http 档位完全同一条判据。
	if len(p.AllowedHosts) > 0 && !hostAllowed(target.Host, p.AllowedHosts) {
		return "", fmt.Errorf("host %q is not in executors.adhoc.url_hosts", target.Host)
	}
	return target.String(), nil
}

// firstPathSpecial 是 firstShellSpecial 去掉反斜杠之后的那一份字符集。
//
// 位置与参数值的区别在这里第一次变成代码上的区别：档位参数值一律禁反斜杠，
// 而任务给的脚本位置在 Windows 上必须是 `C:\work\a.php` 这种写法——
// 同一条判据套上去会让内置档位只能接受正斜杠路径，卡片 §5 的"反斜杠"通过用例就过不了。
// 其余八个字符一个都没减：分号、竖线、与号、反引号、美元符、大于号、小于号与换行回车全部照拒，
// 它们在两个平台上都不是路径应该有的样子，而 argv 直传不过 shell 这条底线不变（D3）。
func firstPathSpecial(item string) string {
	for _, r := range item {
		if strings.ContainsRune(";|&`$><\n\r", r) {
			return strconv.QuoteRune(r)
		}
	}
	return ""
}

// slicesContains 是一个小集合判断：内置档位的扩展名列表只有 1-2 项，
// 不值得为它引一个 sort/二进制查找。
func slicesContains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// adhocCommand 把一条内置定义拼成 `core.ExecutorCommand`，交给既有的构造函数去判。
//
// 拼而不是直接构造 Profile 是为了守住"一份规则"（web-profile-design.md 的 D3）：
// 配置文件里的档位、页面上建的档位、以及这四条内置档位，
// 全部经过 buildProfile 的同一套字段组合、解释器白名单、超时上限与主机写法判据。
// 位置字段一律留空——那是这四条与其余档位的唯一差别，由 buildProfile 的 adhoc 分支放过。
func adhocCommand(spec adhocSpec, ec core.ExecutorsConfig, runtime string) (core.ExecutorCommand, error) {
	switch spec.kind {
	case KindScript:
		if runtime == "" {
			return core.ExecutorCommand{}, fmt.Errorf("no runtime is available for it")
		}
		return core.ExecutorCommand{
			Name:    spec.name,
			Kind:    string(spec.kind),
			Runtime: runtime,
			// Timeout 留空：走 executors.default_timeout，与配置文件里没写 timeout 的档位同一条。
		}, nil

	case KindHTTP:
		// 地址范围守卫是这次放宽里唯一没动的那一层（设计文档 §D9 第③层）：
		// DenyPrivate 取 executors.adhoc.url_allow_private 的反面，默认禁止回环与私网。
		denyPrivate := !ec.Adhoc.URLAllowPrivate
		return core.ExecutorCommand{
			Name:            spec.name,
			Kind:            string(spec.kind),
			Method:          spec.httpMethod,
			Body:            spec.httpBody,
			AllowedHosts:    append([]string(nil), ec.Adhoc.URLHosts...),
			DenyPrivate:     &denyPrivate,
			Timeout:         ec.Adhoc.HTTPTimeout,
			CaptureResponse: false,
		}, nil

	default:
		return core.ExecutorCommand{}, fmt.Errorf("unknown kind %q", spec.kind)
	}
}
