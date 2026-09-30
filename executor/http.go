package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"godelayq/core"
)

// HTTPRunner 是 kind:http 档位的执行主体：把 payload 变成一次真实的 HTTP 请求，
// 按状态码判定成败，把响应与请求头分别写进产物文件，把结论写进 job.Exec。
//
// 它与 Runner（进程类档位）共用结果结构（Result）、失败分类（exit.go 的那一张表）、
// 档位并发许可与产物存储，因此一个档位只存在一种 Runner：Register 按 kind 分流。
//
// 安全边界（卡片 §2 的第 1 条）分两层，缺一不可：
//  1. 请求的目标必须命中档位的 allowed_hosts —— 由 renderURL 在拼出 URL 之后立刻判；
//  2. 那个主机名解析出来的**每一个 IP** 都要过私网判断 —— 由 dialGuarded 在建连之前判。
//
// 第二层为什么必须在"自己解析之后"：只在拼 URL 时查一次主机名挡不住重绑定攻击
// （同一个域名，第一次解析到公网 IP 通过检查，真正建连时解析到 127.0.0.1）。
// 所以这里不用 net/http 默认的"把解析交给拨号器"路径，而是自己解析、自己按 IP 建连。
type HTTPRunner struct {
	profile   *Profile
	artifacts *ArtifactStore
	cfg       core.ExecutorsConfig
	logger    *slog.Logger

	client  *http.Client
	permits chan struct{}

	// lookupHost 是 DNS 解析的注入点（卡片 §5 要求）：把"解析到什么"与"要不要连"分开测，
	// 才能构造"第一次公网、第二次回环"这种重绑定场景，也不必依赖真实 DNS。
	// 默认实现走系统解析器，与 net/http 自己拨号时用的是同一个。
	lookupHost func(ctx context.Context, host string) ([]net.IPAddr, error)

	// rootCAs 是 TLS 校验的信任根，nil 表示系统根证书池。
	// 只给测试留的口子（§5.11 要求能验证"证书校验没被绕过"），配置里没有对应项。
	rootCAs *x509.CertPool
}

// NewHTTPRunner 为 http 档位构造执行器。
//
// 一个档位一个 *http.Client：超时与传输参数按档位各自的声明走，
// 共用一个 Client 会让"某个档位的连接池把另一个档位的请求排住"变成排查噩梦。
//
// cfg 传 core.Config.Normalized() 之后的 executors 一节，理由与 NewRunner 相同。
func NewHTTPRunner(p *Profile, a *ArtifactStore, cfg core.ExecutorsConfig, l *slog.Logger) (*HTTPRunner, error) {
	if l == nil {
		l = slog.Default()
	}
	if p.Kind != KindHTTP {
		return nil, fmt.Errorf("profile %q has kind %q, the http executor runs http profiles only", p.Name, p.Kind)
	}

	runner := &HTTPRunner{
		profile:   p,
		artifacts: a,
		cfg:       cfg,
		logger:    l,
		permits:   make(chan struct{}, permitCapacity(p.MaxParallel)),
		lookupHost: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		},
	}

	transport := &http.Transport{
		// 不配代理（卡片 §8：代理、mTLS、自定义 CA 都不在本期范围）。
		// 这里显式留空而不是继承 http.DefaultTransport 的 ProxyFromEnvironment：
		// 环境变量里的代理会另起一条到代理服务器的连接，而那条连接不在本卡的地址判断之内，
		// 等于给"借服务端访问内网"开了一条不经过检查的通路。
		Proxy:             nil,
		DialContext:       runner.dialContext,
		DialTLSContext:    runner.dialTLSContext,
		DisableKeepAlives: false,
		// 不强制 HTTP/2：DialTLSContext 存在时标准库只在 NextProtos 里写了 h2 才协商，
		// 保持 HTTP/1.1 让"一次请求一条可观测的连接"这条排查前提成立。
	}

	runner.client = &http.Client{
		Transport: transport,
		// 返回 ErrUseLastResponse 表示拿到重定向响应就停，不跟过去（见 refuseRedirect）。
		CheckRedirect: refuseRedirect,
		// client.Timeout 有意不设（卡片 §3.5）：超时只有一个来源——档位与 payload 算出的
		// 生效超时（见 Handler 里的 context.WithTimeout）。两处计时会让"为什么 30 秒就断了"
		// 变成两个候选答案，而且 client.Timeout 连请求体写一半也掐，事件里看起来像对端问题。
	}
	return runner, nil
}

// refuseRedirect 是"永不跟随重定向"的实现。
//
// 跟随重定向等于把目标地址交给对端决定：Location 里写 127.0.0.1 就得去，
// allowed_hosts 与档位声明因此失去意义（E02 的 max_redirects 只允许 0 就是这个理由）。
// 3xx 因此是一次正常的执行结果，只是通常不在 expect_status 里，会被判成失败。
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// Handler 返回交给调度器的处理函数。
//
// 流程与进程档位对齐：校验 payload → 拼 URL 与请求 → 求生效超时 → 取档位许可 →
// 建产物文件 → 发请求收响应 → 关文件并把结论写进 job.Exec → 返回错误。
// 前半段任何一步失败都不发请求：那时连"要访问哪个地址"都还没定下来。
//
// 日志规范与设计文档 §7 同进程档位：只记 job_id、handler_key、档位名、状态码、耗时、是否截断。
// 参数值、请求体、完整 URL 都不进日志——URL 的路径段里可能就是参数值。
func (r *HTTPRunner) Handler() core.Handler {
	p := r.profile
	key := p.HandlerKey()

	return func(ctx context.Context, job *core.Job) (err error) {
		result := NewResult(p)

		// 摘要在每条返回路径上都要落进 job.Exec，permanent 与返回的错误同源（TASK-E12 的口径）。
		defer func() {
			result.Meta.Permanent = summaryPermanent(err)
			// 对端把参数值回显进响应体是最常见的泄露形态：预览按值掩掉再进摘要
			// （TASK-E16 §3.3 第 2 条）。产物文件保持原样，读它的门槛在 api 侧已收到提交档位。
			result.Meta.Preview = p.MaskSecretText(job.Payload, result.Meta.Preview)
			job.Exec = &result.Meta
			r.logRun(key, job, result.Meta)
		}()

		if r.artifacts == nil {
			return newFailure(p, failureNotAllowed, 0,
				"no artifact store is configured, response output has nowhere to go", nil)
		}

		sub, err := ValidateSubmission(p, job.Payload)
		if err != nil {
			return newFailure(p, failureInvalidSubmission, 0, "invalid submission", err)
		}

		target, targetLine, err := r.renderURL(sub)
		if err != nil {
			return newFailure(p, failureInvalidSubmission, 0, "cannot build the request URL", err)
		}

		request, body, err := r.newRequest(target, sub)
		if err != nil {
			return newFailure(p, failureInvalidSubmission, 0, "cannot build the request", err)
		}

		timeout := p.timeoutWithin(r.cfg, sub.TimeoutValue)
		if !acquirePermit(ctx, r.permits, timeout) {
			return permitFailure(p, ctx, timeout)
		}
		defer releasePermit(r.permits)

		// 自己再套一层超时：调度器给的上下文可能比档位算出的生效超时更长（恢复路径带回来的任务、
		// 或提交期没写进任务的情形）。绝不出现"无限等待"，与进程档位同一条理由。
		// 这是这一次请求唯一的超时来源：client.Timeout 没有设（§3.5），
		// 拨号与握手阶段也挂在同一个上下文上，两个计时只会让"为什么 30 秒就断了"有两个候选答案。
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		writer, err := r.artifacts.Open(job.ID, job.Attempts)
		if err != nil {
			// 没地方写结论就不发请求：请求已经到达对端却查不到任何记录，比不发更糟
			return newFailure(p, failureNotAllowed, 0, "cannot create the output files", err)
		}

		runErr := r.execute(execCtx, request, targetLine, body, writer, result)

		// permanent 在这里先落一次：meta.json 与任务快照要写同一份结论，而文件在返回之前就写完。
		// 外层 defer 会按同一个函数再算一遍，不存在两处各写一套规则。
		result.Meta.Permanent = summaryPermanent(runErr)

		info, closeErr := writer.Close()
		result.Meta.OutBytes = info.OutBytes
		result.Meta.ErrBytes = info.ErrBytes
		// 响应体超限的截断标记由 execute 先置好，这里只把"产物文件写满"那一层或进来：
		// 两条来源说的是同一件事（还有内容没采到），但只有前者能解释成"响应体超限"。
		result.Meta.Truncated = result.Meta.Truncated || info.Truncated
		result.Meta.Artifact = core.ArtifactAvailable
		// 索引登记与进程执行器同一位置：info 是刚关完的那份结论，与摘要同源。
		// 新增一种 Kind 就要在这里接上，否则那一档的产物在列表里整个看不见（见接口注释）。
		r.artifacts.recordIndex(KindHTTP, p.Name, info)
		if closeErr != nil {
			r.logger.Warn("executor output files did not close cleanly",
				"job_id", job.ID, "handler_key", key, "error", closeErr)
		}
		if metaErr := writer.WriteMeta(result.Meta); metaErr != nil {
			r.logger.Warn("executor artifact metadata was not written",
				"job_id", job.ID, "handler_key", key, "error", metaErr)
		}
		return runErr
	}
}

// logRun 记一次执行的收尾日志。字段集合与进程档位同一体例：档位名可以外泄，参数值不可以。
// 这里没有 exit_code 一项：HTTP 档位没有退出码可言，状态码才是它的结论。
func (r *HTTPRunner) logRun(key string, job *core.Job, meta core.ExecMeta) {
	r.logger.Info("executor run finished",
		"job_id", job.ID,
		"handler_key", key,
		"profile", meta.Profile,
		"kind", meta.Kind,
		"http_status", meta.HTTPStatus,
		"duration_ms", meta.DurationMs,
		"truncated", meta.Truncated)
}

// renderURL 把档位模板里的 {name} 换成 payload 提供的参数值，并检查结果的合法性。
//
// 值本身在提交期已经过两道检查：E08 的档位规则（默认字符集、必填、正则），
// 以及 http 专属的那一条（不能含会改掉 URL 结构的字符，见 urlParamForbidden）。
// 这里仍然重新解析并核对主机，理由是防御性的：模板与值拼完之后的字符串才是真正要访问的地址，
// 只检查片段不足以证明整体没问题。
//
// 第二个返回值是给产物文件看的"同一地址的打码写法"：声明为 secret 的参数值换成 <secret>。
// 真正发出去的请求用第一个值。产物文件与 jobs.json 同等级（都在磁盘上，而磁盘上的原文
// 不归响应层的掩码管，见 TASK-E16 §3.3 第 3 条），
// 但把口令抄进一份"给人排查用的请求记录"里没有收益，能不留就不留。
func (r *HTTPRunner) renderURL(sub *Submission) (*url.URL, string, error) {
	target, err := r.fillTemplate(sub.Params)
	if err != nil {
		return nil, "", err
	}

	recorded := target.Redacted()
	rendered := target.String()
	if masked := r.maskSecrets(rendered, sub.Params); masked != rendered {
		// 打码只替换"转义之后的值"，不再走一遍渲染：两条渲染路径迟早算出两个样子，
		// 而产物里那份地址必须和真正发出去的请求长得一样，否则排查时对不上
		recorded = masked
	}
	return target, recorded, nil
}

// fillTemplate 渲染并校验真正要访问的地址。
func (r *HTTPRunner) fillTemplate(values map[string]string) (*url.URL, error) {
	rendered, err := r.renderRaw(values)
	if err != nil {
		return nil, err
	}

	target, err := url.Parse(rendered)
	if err != nil {
		return nil, fmt.Errorf("rendered url is not a valid URL: %w", err)
	}
	if target.Host == "" || target.Scheme != "http" && target.Scheme != "https" {
		// 不回显 rendered：模板带值之后可能是 /orders/<口令>，错误文本会进事件与接口
		return nil, errors.New("rendered url has no valid scheme or host")
	}
	if target.User != nil {
		// 模板阶段已拒绝凭据，值里也过不了 urlParamForbidden（含 @ 与 :）；这条是第三层
		return nil, errors.New("request URL must not carry user credentials")
	}
	if !hostAllowed(target.Host, r.profile.AllowedHosts) {
		return nil, fmt.Errorf("host %q is not in the profile's allowed_hosts", target.Host)
	}
	return target, nil
}

// renderRaw 做占位符替换，并把没填上的占位符报出来。
func (r *HTTPRunner) renderRaw(values map[string]string) (string, error) {
	p := r.profile

	rendered := placeholderPattern.ReplaceAllStringFunc(p.URLTemplate, func(match string) string {
		name := match[1 : len(match)-1]
		value, ok := values[name]
		if !ok {
			// 没给值的可选参数留原样，交给下面的"还有花括号"检查统一报出来：
			// 静默替换成空串会让 /orders/ 这种地址看起来是一次合法请求，实际访问的是另一个资源
			return match
		}
		return url.PathEscape(value)
	})
	if strings.ContainsAny(rendered, "{}") {
		return "", fmt.Errorf("url_template %q has placeholders that the payload did not fill", p.URLTemplate)
	}
	return rendered, nil
}

// maskSecrets 把渲染好的地址里那些声明为 secret 的参数值换成占位文本。
//
// 替换的对象是 url.PathEscape 之后的形态：渲染时每个值都按这一种写法进模板，
// 因此按同一个函数算出的片段去替换就能对上位次，不必再解析一次 URL。
// 空值跳过：替换一个空串等于在每个位置插入占位文本。
// 返回值与入参相同表示这一次提交没有 secret 参数在用，调用方保留原来的写法即可。
func (r *HTTPRunner) maskSecrets(rendered string, values map[string]string) string {
	for _, spec := range r.profile.Args {
		if !spec.Secret {
			continue
		}
		value, ok := values[spec.Name]
		if !ok || value == "" {
			continue
		}
		rendered = strings.ReplaceAll(rendered, url.PathEscape(value), maskedURLValue)
	}
	return rendered
}

// maskedURLValue 是产物文件里替掉敏感参数值的文本。
// 长度与原值无关，因此看产物的人不会以为"/verylongtoken"那样的路径真的存在。
const maskedURLValue = "<secret>"

// newRequest 按档位与方法组装请求，返回请求与将要发出的请求体字节。
//
// 请求体单独返回：产物文件里要写它的字节数，而 http.Request.Body 是一次性读的流。
// 上下文由 execute 挂上（那里才有本次执行的超时上下文）。
func (r *HTTPRunner) newRequest(target *url.URL, sub *Submission) (*http.Request, []byte, error) {
	p := r.profile

	body, err := r.requestBody(sub)
	if err != nil {
		return nil, nil, err
	}
	if len(body) > 0 && (p.Method == http.MethodGet || p.Method == http.MethodDelete) {
		// E02 只要求"有体量的方法声明 body 来源"，GET/DELETE 声明的是 none；
		// payload 硬塞一个 body 时不在这里拒绝，net/http 会照发，对端怎么处理全凭它自己。
		return nil, nil, fmt.Errorf("method %s does not accept a request body for profile %q", p.Method, p.Name)
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	// 这里不挂上下文：本次执行的超时上下文由 execute 附上（request.WithContext），
	// 那之前它不会被发出去，挂一个更早的上下文只会多出"两个上下文谁说话"的问题。
	request, err := http.NewRequest(p.Method, target.String(), reader)
	if err != nil {
		// 到这里模板、方法、URL 都已判过，出错只能是标准库层面的问题
		return nil, nil, fmt.Errorf("cannot build the request: %w", err)
	}

	request.Header = cloneHeaders(p.Headers)
	for name, value := range sub.Headers {
		request.Header.Set(name, value)
	}
	if len(body) > 0 && request.Header.Get("Content-Type") == "" {
		// 只有 json 模式能确定类型；raw 模式由档位自己声明的固定头决定（没声明就不带）
		if p.Body == "json" {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if len(body) > 0 {
		// Content-Length 交给标准库算：手工设过会让 keep-alive 连接在长度对不上时被重置，
		// 而那副样子在对端日志里像网络问题。这里显式删掉任何来自档位配置的取值。
		request.Header.Del("Content-Length")
	}
	request.ContentLength = int64(len(body))
	return request, body, nil
}

// requestBody 按档位的 body 模式取出要发的字节。
//
// E08 已经拒掉"档位不收 body 却传了 body"的情形，这里判的是内容形态：
//   - none：恒为空；
//   - json：必须是合法的 JSON 对象或数组（配置注释里的口径），原样发送，不做重排；
//   - raw：任意合法 JSON 值，原样发送。
//
// 不重新序列化 JSON 而是用原始字节：重排会改动键序与数字精度，
// 一旦签名或校验建在请求体字节上，"我们发出去的不是你给的那份"就说不清。
func (r *HTTPRunner) requestBody(sub *Submission) ([]byte, error) {
	switch r.profile.Body {
	case "", "none":
		return nil, nil
	case "json", "raw":
		if len(sub.Body) == 0 {
			// 档位声明了体的来源，不代表这一次必须带：body 在 payload 里是可选项。
			// 不带就是没有请求体，与 none 同样处理。
			return nil, nil
		}
		if !json.Valid(sub.Body) {
			return nil, fmt.Errorf("body must be valid JSON for profile %q", r.profile.Name)
		}
		if r.profile.Body == "json" {
			trimmed := bytes.TrimSpace(sub.Body)
			if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
				return nil, fmt.Errorf("body must be a JSON object or array for profile %q", r.profile.Name)
			}
		}
		return append([]byte(nil), sub.Body...), nil
	default:
		return nil, fmt.Errorf("profile %q declares an unknown body mode %q", r.profile.Name, r.profile.Body)
	}
}

// cloneHeaders 复制档位声明的固定请求头。
// 必须复制：payload 的覆盖如果写回 profile.Headers，会永久改掉这个档位的固定头。
//
// 键名用 Add 而不是直接写进 map：配置解码出来的键是小写（viper 归一化），
// 直接写 map 会把 `x-source` 这种小写形式原样发到对端与产物里，
// 而 payload 那一侧走的是 Set，同一个档位两种头名写法。Add 按 HTTP 惯例归一成形。
func cloneHeaders(source map[string][]string) http.Header {
	cloned := make(http.Header, len(source))
	for name, values := range source {
		for _, value := range values {
			cloned.Add(name, value)
		}
	}
	return cloned
}

// sensitiveHeaderNames 是写进产物文件之前要打码的请求/响应头。
// 大小写不敏感比较，按 HTTP 头的习惯归一后再查。
var sensitiveHeaderNames = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"proxy-authorization": true,
}

// redactValue 是打码之后写进产物的占位文本。
// 保留"有这个头"这个事实：排查时"对方没收到 Authorization"与"收到了但值我们没写"是两回事。
const redactValue = "<redacted>"

func isSensitiveHeader(name string) bool {
	return sensitiveHeaderNames[strings.ToLower(strings.TrimSpace(name))]
}

// execute 发请求、收响应、写产物，返回分类后的失败（成功为 nil）。
//
// ctx 是 Handler 算出生效超时之后那个上下文，请求整个生命周期挂在它上面：
// 取消（用户 Cancel、优雅关闭）与超时都由它传达给 net/http，因此不需要第二个计时器。
// targetLine 是这次请求地址的打码写法（由 renderURL 给出），只用于写产物。
func (r *HTTPRunner) execute(ctx context.Context, request *http.Request, targetLine string, body []byte,
	writer *ArtifactWriter, result *Result) error {

	p := r.profile

	// 请求头先落盘再发：请求一旦到达对端，事后失败也得留下"我们发过什么"。
	// 这一段不写请求体内容（卡片 §3.4 只要求头与体字节数）：
	// 请求体里可能是明文口令，整份写进产物文件不是本卡能默认的事。
	recordRequest(writer, request, targetLine, len(body))

	started := time.Now()
	resp, doErr := r.client.Do(request.WithContext(ctx))
	result.Meta.DurationMs = time.Since(started).Milliseconds()

	if doErr != nil {
		transportErr := unwrapURLError(doErr)
		recordFailure(writer, transportErr)
		return classifyHTTPError(p, transportErr, ctx.Err())
	}
	defer resp.Body.Close()

	result.Meta.HTTPStatus = resp.StatusCode
	recordResponse(writer, resp)

	maxBytes := r.bodyLimit()
	captured, truncated, readErr := readCapped(resp.Body, maxBytes, p.CaptureResponse, writer)
	if readErr != nil {
		// 状态码已经拿到并记下：读体失败是"结论有了、正文没拿全"，不是整次执行没结论
		recordFailure(writer, readErr)
		result.Meta.Truncated = true
		return classifyHTTPError(p, unwrapURLError(readErr), ctx.Err())
	}
	if truncated {
		result.Meta.Truncated = true
	}
	if p.CaptureResponse {
		r.fillBodyPreview(result, captured)
	}

	if statusExpected(p, resp.StatusCode) {
		return nil
	}
	return newStatusFailure(p, failureHTTPStatus, resp.StatusCode,
		fmt.Sprintf("unexpected status %s", strconv.Itoa(resp.StatusCode)), nil)
}

// bodyLimit 是响应体的读取上限：档位声明值与全局 output.max_bytes 取小的那个。
// 两者都在 E01/E02 有默认值，为 0 的情况按标准默认值兜住，避免"0 表示不限"这种误读。
func (r *HTTPRunner) bodyLimit() int {
	limit := r.profile.MaxBodyBytes
	if global := r.cfg.Output.MaxBytes; global > 0 && (limit <= 0 || global < limit) {
		limit = global
	}
	if limit <= 0 {
		limit = core.DefaultExecMaxOutputBytes
	}
	return limit
}

// readCapped 读响应体：最多取 maxBytes 字节。
//
// 多读一个字节用来区分"正好这么大"与"后面还有"，因此判定是 read > max（卡片 §3.4 的口径）。
// capture 为 false 时内容只计数不落盘，也不给预览：档位明确声明了不保留响应体。
func readCapped(body io.Reader, maxBytes int, capture bool, writer *ArtifactWriter) (data []byte, truncated bool, err error) {
	limited := io.LimitReader(body, int64(maxBytes)+1)
	if !capture {
		// 仍然要读完（在上限之内）：连接要正常收尾，且不读就等于把对端的体量限制交给我们猜
		counted, err := io.ReadAll(limited)
		if err != nil {
			return nil, false, err
		}
		return nil, len(counted) > maxBytes, nil
	}

	data, err = io.ReadAll(limited)
	if err != nil {
		return data, false, err
	}
	if len(data) > maxBytes {
		truncated = true
		data = data[:maxBytes]
	}
	if _, err := writer.Stdout().Write(data); err != nil {
		return data, truncated, err
	}
	return data, truncated, nil
}

// fillBodyPreview 用响应体尾部填摘要里的预览。
//
// 不走 Result.SetPreview：那个方法按"脚本失败时最有用的信息在 stderr"的规则优先取 stderr，
// 而 http 档位的 .err 文件里写的是头信息（卡片 §3.4），按那条规则预览会变成一串头名。
// 裁剪仍用 core.TrimExecPreview，与进程档位、事件侧、接口侧算出同一个字符串。
func (r *HTTPRunner) fillBodyPreview(result *Result, body []byte) {
	limit := r.previewLimit()
	if limit <= 0 {
		result.Meta.Preview = ""
		return
	}
	result.Meta.Preview = core.TrimExecPreview(string(body), limit)
}

// previewLimit 返回摘要与事件里预览的字节上限（executors.output.inline_preview）。
func (r *HTTPRunner) previewLimit() int {
	if r.cfg.Output.InlinePreview <= 0 {
		return core.DefaultExecInlinePreview
	}
	return r.cfg.Output.InlinePreview
}

// statusExpected 回答"这个状态码算不算成功"：档位给了 expect_status 就按它，
// 留空表示只接受 2xx（E01 的字段注释口径）。
func statusExpected(p *Profile, status int) bool {
	if len(p.ExpectStatus) == 0 {
		return status >= 200 && status < 300
	}
	for _, want := range p.ExpectStatus {
		if want == status {
			return true
		}
	}
	return false
}

// classifyHTTPError 把发请求阶段的错误归到类别，并按 classifyExit 那一张表填重试标记。
//
// 顺序与进程路径同一条理由：上下文先结束（超时/取消）优先于错误本身的内容——
// 是我们先撤了，对端的报错只是撤消的副产品。
// 地址被策略拒掉单列一类：它形似连接失败，但重试不会让策略变宽，白占执行名额。
func classifyHTTPError(p *Profile, doErr error, ctxErr error) *ExitError {
	if doErr == nil {
		return nil
	}

	switch {
	case ctxErr != nil:
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return newFailure(p, failureTimeout, 0, "timed out", doErr)
		}
		return newFailure(p, failureInterrupted, 0, "cancelled", doErr)
	case errors.Is(doErr, errAddressRefused):
		// 不给 reason：doErr 的文本已经写着"被档位的网络策略拒掉"和那个地址，
		// 再加一句同类描述，事件与日志里就是同一句话的两份写法。
		return newStatusFailure(p, failureAddressRefused, 0, "", doErr)
	default:
		return newStatusFailure(p, failureTransport, 0, "request failed", doErr)
	}
}

// unwrapURLError 把 *url.Error 剥回内层错误。
//
// 必须剥：url.Error 的文本形如 `Get "https://host/path/with/param-values": dial ...`，
// 带着完整 URL，而路径段里可能就是参数值（口令进 URL 是常见写法）。
// 本包要进日志与产物的只有内层那条自己构造的错误，它的文本里没有 URL。
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// recordRequest 把请求行与请求头写进 .err 流（敏感头打码，地址用打码那一份写法）。
func recordRequest(writer *ArtifactWriter, request *http.Request, targetLine string, bodyLen int) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "> %s %s\r\n", request.Method, targetLine)
	fmt.Fprintf(&buf, "> request body bytes: %d\r\n", bodyLen)
	writeHeaderBlock(&buf, ">", request.Header)
	_, _ = io.WriteString(writer.Stderr(), buf.String())
}

// recordResponse 把状态行与响应头写进 .err 流（敏感头打码）。
func recordResponse(writer *ArtifactWriter, resp *http.Response) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "< %s\r\n", resp.Status)
	writeHeaderBlock(&buf, "<", resp.Header)
	_, _ = io.WriteString(writer.Stderr(), buf.String())
}

// recordFailure 把发请求阶段的错误追加到 .err 流末尾。
func recordFailure(writer *ArtifactWriter, err error) {
	_, _ = io.WriteString(writer.Stderr(), fmt.Sprintf("! request failed: %v\r\n", err))
}

// writeHeaderBlock 按字典序写头，键名归一化之后判敏感。
// 排序是为了让产物可 diff：map 的遍历顺序每次不同，同一份请求两次落盘会写成两个样子。
func writeHeaderBlock(buf *bytes.Buffer, mark string, header http.Header) {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if isSensitiveHeader(name) {
			for range header[name] {
				fmt.Fprintf(buf, "%s %s: %s\r\n", mark, name, redactValue)
			}
			continue
		}
		for _, value := range header[name] {
			fmt.Fprintf(buf, "%s %s: %s\r\n", mark, name, value)
		}
	}
}

// 地址限制（卡片 §3.3 的核心）：以下四个方法构成"建连之前先判 IP"这一道防线。

// errAddressRefused 标记"地址是被本卡的策略拒掉的"，与真正的连接失败分开：
// 前者重试没有意义（策略不会自己变），后者有（TASK-E15 §3.4）。
var errAddressRefused = errors.New("address refused by the profile's network policy")

func (r *HTTPRunner) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, _, err := r.dialGuarded(ctx, network, addr)
	return conn, err
}

// dialTLSContext 自己完成 HTTPS 的连接与握手。
//
// 为什么走这条而不是 DialContext + Transport.TLSClientConfig：
// 按 IP 建连之后，标准库从 URL 主机名推 TLS 的 ServerName，而它推不出来的情形
// （主机名是 IP）会让证书校验变成一个空的目标。这里由我们自己把档位声明的主机名交给
// tls.Config.ServerName，两条都能对上：
//   - 主机名是域名：SNI 与证书里的 DNS 名一致；
//   - 主机名是 IP 字面量：crypto/tls 见到 IP 不发 SNI，校验按证书的 IP 主题备用名走。
//
// 两条都不成立时才放行——即"绕过了证书校验"这种情况在这里没有代码路径可走：
// RootCAs 之外的校验开关一个都没设。
func (r *HTTPRunner) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, host, err := r.dialGuarded(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	secure := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: r.rootCAs})
	if err := secure.HandshakeContext(ctx); err != nil {
		_ = secure.Close()
		return nil, fmt.Errorf("tls handshake with %s failed: %w", host, err)
	}
	return secure, nil
}

// dialGuarded 解析出要连的主机、过一遍地址判断，然后按选中的 IP 建连。
// 返回的 host 是档位声明里那个主机名（TLS 要用它，不能用 IP）。
func (r *HTTPRunner) dialGuarded(ctx context.Context, network, addr string) (net.Conn, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("dial address %q is not host:port: %w", addr, err)
	}

	ip, err := r.allowedIP(ctx, host)
	if err != nil {
		return nil, host, err
	}

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	if err != nil {
		// 错误文本里只到主机与端口：这一层还没有 URL 可言，参数值不会经由这里进日志
		return nil, host, fmt.Errorf("cannot connect to %s: %w", net.JoinHostPort(host, port), err)
	}
	return conn, host, nil
}

// allowedIP 从主机名对应的地址里挑出第一个允许建连的 IP。
//
// 全部候选都被拒掉时报错，并且不发起任何连接——这是 DoD 第一条的实现位置。
// 逐个尝试而不是"只要有一个公网地址就连"：标准库的拨号器会在多个地址之间自行重试，
// 那种重试会绕开这里，第一个被拒的 IP 也可能在它的重试里被连上。
func (r *HTTPRunner) allowedIP(ctx context.Context, host string) (net.IP, error) {
	if literal := net.ParseIP(host); literal != nil {
		if reason := r.refusalReason(literal); reason != "" {
			r.logRefusal(host, literal, reason)
			return nil, fmt.Errorf("%w: %s (%s)", errAddressRefused, literal, reason)
		}
		return literal, nil
	}

	addrs, err := r.lookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s: no address records", host)
	}

	var refused []string
	for i := range addrs {
		ip := addrs[i].IP
		if reason := r.refusalReason(ip); reason != "" {
			r.logRefusal(host, ip, reason)
			refused = append(refused, fmt.Sprintf("%s(%s)", ip, reason))
			continue
		}
		return ip, nil
	}
	return nil, fmt.Errorf("%w: every address of %s was refused: %s",
		errAddressRefused, host, strings.Join(refused, ", "))
}

// refusalReason 给出这个地址被拒绝的理由，允许时返回空串。
//
// 判断全部走 net.IP 的既有方法（DoD 第二条：代码里没有手写网段表）。
// 唯一显式写出的段是 RFC 6598 的 100.64.0.0/10：标准库没有对应方法，
// 而卡片 §3.3 把它列进拒绝清单（运营商级 NAT 的地址段里常有内部服务与元数据入口）。
//
// deny_private_ranges=false 时这里一律放行：那是"在开发机上打自己的回环服务"的显式例外，
// 代价是这道防线整体关闭（E02 已要求这种取值下 allowed_hosts 不能写通配，启动时另记 warn）。
func (r *HTTPRunner) refusalReason(ip net.IP) string {
	if ip == nil {
		return "not an IP address"
	}
	if !r.profile.DenyPrivate {
		return ""
	}

	switch {
	case ip.IsUnspecified():
		// 0.0.0.0 与 :: 不能作目标：连上去的行为由操作系统决定，各家不一样
		return "unspecified address"
	case ip.IsLoopback():
		return "loopback address"
	case ip.IsPrivate():
		// RFC 1918 的 10/8、172.16/12、192.168/16 与 IPv6 的 fc00::/7
		return "private range address"
	case ip.IsLinkLocalUnicast():
		// 169.254/16 含云主机的元数据地址 169.254.169.254，fe80::/10 同理
		return "link-local address"
	case ip.IsMulticast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return "multicast address"
	case inSharedAddressSpace(ip):
		return "shared address space (100.64.0.0/10)"
	default:
		return ""
	}
}

// sharedAddressSpace 是 RFC 6598 的 100.64.0.0/10。见 refusalReason 的注释：
// 这是本文件里唯一一段显式写出来的网段，标准库没有对应判断方法。
var sharedAddressSpace = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

func inSharedAddressSpace(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && sharedAddressSpace.Contains(v4)
}

// logRefusal 记一条被拒地址的 warn。
//
// 这是"为什么调不通"的唯一现场线索（卡片 §3.3 第 4 条）。
// 字段里只有档位名、主机与被拒 IP：完整 URL 的路径段可能带着参数值，不进日志。
func (r *HTTPRunner) logRefusal(host string, ip net.IP, reason string) {
	r.logger.Warn("executor http target address refused",
		"profile", r.profile.Name,
		"handler_key", r.profile.HandlerKey(),
		"host", host,
		"address", ip.String(),
		"reason", reason)
}

// PreferredResultDirection 返回接口读该档位输出时的建议起点：
// http 档位的正文是响应体，出错信息通常在开头，因此建议 head；
// 进程档位的建议是 tail（脚本的结论通常在末尾）。
//
// 这是给 E18 的提示，不是判定：读端点的 from 参数仍然由调用方决定。
// 没有这个提示的话，前端只能对 http 也默认取尾部，而 200 字节以下的 JSON 错误体
// 被掐头去尾之后刚好剩下看不懂的那半截（卡片 §9 第三条）。
func PreferredResultDirection(p *Profile) string {
	if p.Kind == KindHTTP {
		return ResultDirectionHead
	}
	return ResultDirectionTail
}

// 输出起点建议的两个取值，与 GET /jobs/:id/result 的 from 参数同一套词。
const (
	ResultDirectionTail = "tail"
	ResultDirectionHead = "head"
)
