# TASK-E15　HTTP 执行器与内网访问限制

- 所属阶段：M4 HTTP
- 依赖任务：TASK-E05、E06、E07、E08、E12
- 涉及文件：新增 `executor/http.go`、`executor/http_test.go`；`executor/proc.go`（Handler 分流按 kind）
- 预计规模：中偏大（安全判定的位置容易做错）

## 1. 任务目标

实现 `kind: http` 档位：向配置里固定下来的地址发起请求、按状态码判定成败、把响应状态与响应体摘要交回结果通道，并且保证它不能被用来访问配置之外的任何地址（包括内网地址）。

## 2. 背景与当前问题

`../../executor-design.md` §1 把 HTTP 列为三类执行能力之一。它和进程类执行器共用任务模型、结果结构、重试分类，但有两条独有风险：

1. 这类任务等同于"服务端代发请求"。如果不限制目标地址，任何能提交任务的人都可以借这个进程去访问内网服务、扫描端口、读取云主机的元数据地址（`169.254.169.254`）。
2. 响应体大小不可控。对方返回 200MB 的话，直接读进内存或写进产物都会出问题。

第 1 点的关键认知是：**只在拼 URL 时检查一次主机名是不够的**。攻击者可以准备一个域名，第一次解析到公网 IP 通过检查、真正建连时解析到 `127.0.0.1`。所以判定必须发生在拨号之前、针对实际要连接的 IP。

## 3. 要实现的功能

1. `executor/http.go`：

   ```go
   type HTTPRunner struct {
       profile *Profile
       client  *http.Client
       cfg     core.ExecutorsConfig
       logger  *slog.Logger
   }
   func NewHTTPRunner(p *Profile, cfg core.ExecutorsConfig, l *slog.Logger) (*HTTPRunner, error)
   func (r *HTTPRunner) Handler() core.Handler
   ```

   每个档位一个 `*http.Client`（超时与传输参数按档位配置，互不影响），`CheckRedirect` 返回 `http.ErrUseLastResponse`（不跟随重定向，与 E02 的 `max_redirects` 必须为 0 一致）。
2. 请求构造：
   - URL：`profile.URLTemplate` 里的 `{name}` 占位符用 `submission.Params` 的值替换。替换前每个值已由 E08 的规则校验；本卡额外要求：值不允许含 `/ ? # @ : % \` 与空白（这类值会把 URL 结构改掉）。占位符替换后的值做 `url.PathEscape`（模板里占位符出现在路径段是主要用法；出现在查询段的用法本期不支持，配置里写了就报错）。
   - 主机与端口必须命中 `profile.AllowedHosts`（精确匹配或 `*.domain` 前缀通配，通配实现要防 `evil\.com` 式的伪匹配：用 `net/url` 解析后比对 `Host` 字段，不用字符串 `HasSuffix`）。
   - `Method`：只接受 `GET|POST|PUT|PATCH|DELETE`；GET/DELETE 带 body 时报错。
   - `Body`：`none` 忽略 payload 的 body；`json` 要求是合法 JSON 并原样发送、`Content-Type: application/json`；`raw` 允许任意 JSON 值序列化后的字节。请求头固定用 `profile.Headers`，`submission.Headers` 只能覆盖 `HeaderAllow` 里的键；被覆盖的键值若是 `secret` 参数则不参与日志。
   - `Host`、`Content-Length`、`Transfer-Encoding` 三个头禁止由 payload 设置（`http.Client` 会忽略大部分，但显式拒绝更清楚）。
3. 拨号层地址限制（本卡的核心）：
   - `Transport` 用自定义 `DialContext`：
     1. 先调本家的 `net.Resolver` 解析目标主机名，拿到将要去连的 IP 列表（**用 `LookupIPAddr` + 自己拨号**，或更简单：用 `.DialContext` 拿到的 `address` 已经解析过的 IP 直接判断——实现时二选一并在注释里说明，注意 `DialContext` 的 `address` 参数在 DNS 解析之前仍是主机名，因此"直接判断 address"是错的，必须自己解析）。
     2. 对每个候选 IP：`deny_private_ranges` 为 true 时拒绝回环、链路本地（含 `169.254.169.254`）、私网（`10/8`、`172.16/12`、`192.168/16`）、`100.64/10`、组播、未指定地址、IPv6 的 `::1`/`fc00::/7`/`fe80::/10`。用 `net.IP` 的 `IsLoopback`/`IsPrivate`/`IsLinkLocalUnicast`/`IsUnspecified`/`IsMulticast` 组合判断，不要自己写网段表（少写一段就是一个洞）。
     3. 全部候选被拒 → 返回错误，**不发起任何连接**。
     4. 允许时才用该 IP 拨号，并把 TLS 的 `ServerName` 设回原主机名（`Transport.TLSClientConfig` 或 `http.Transport` 的 `DialTLSContext`；否则 HTTPS 证书校验会因地址是 IP 而失败）。
   - 用 `ip:port` 直连（主机名本身就是 IP）的配置要在 E02 校验阶段就要求它出现在 `allowed_hosts` 里，并且拨号层照样过私网判断——避免"写 IP 就绕过检查"。
   - 每条拒绝都记一条 warn（含档位名与被拒 IP），这是排查"为什么调不通"的唯一线索，但不能把完整 URL（含参数值）打进日志。
4. 响应处理：
   - `MaxBodyBytes`（档位配置，且不超过 `executors.output.max_bytes`）：用 `io.LimitReader(r.Body, max+1)` 判断是否超限，超限则 `truncated=true`。
   - 状态判定：`resp.StatusCode` 命中 `ExpectStatus` → 成功；否则失败，`Permanent` 规则：`4xx`（除 `408`、`429`）→ 永久；`5xx`、`408`、`429`、连接层错误 → 可重试。与 E12 的分类表合并到同一个函数里，不要两处各写一套。
   - 结果写入：`ExecMeta.Kind="http"`、`HTTPStatus`、`DurationMs`、`OutBytes`（响应体字节数）、`Preview`（响应体尾部，走 E05 的 `SetPreview`）。
   - 产物文件：响应体写 `a<attempt>.out`，请求与响应的头（去掉敏感头）写 `a<attempt>.err`，方便排查；`meta.json` 沿用 E06。
   - 敏感头过滤清单：`Authorization`、`Cookie`、`Set-Cookie`、`X-Api-Key`、`Proxy-Authorization`（大小写不敏感），命中时值写成 `<redacted>`。
5. 上下文：请求必须带 `ctx`（`http.NewRequestWithContext`），使取消与超时能立即生效；`client.Timeout` 不设置，让 `Job.Timeout` 作为唯一超时来源（两个超时会让"为什么 30 秒就断了"难以判断）。

## 4. 实现步骤

1. 先写 `Transport` 与地址判断，单独用 `httptest` + 一个自建 resolver 测通（这是本卡最独立也最容易错的一块）。
2. 写请求构造与参数渲染（含 `PathEscape` 与非法字符拒绝）。
3. 写响应处理与结果落盘。
4. 把 `Handler()` 分流：`proc.go` 的注册链路按 `kind` 选 `Runner` 或 `HTTPRunner`（E04 的 `Register` 需要小改，写明改动点）。
5. 加第 5.6 条的端到端用例。

## 5. 测试要求

全部用 `net/http/httptest`，不访问外网。涉及 IP 判断的用例通过注入 resolver 完成（把 `lookupHost` 做成包级变量或结构体字段以便替换）。

1. `TestHTTP_Dial_RefusesPrivate`：主机名解析到 `127.0.0.1`、`10.0.0.5`、`169.254.169.254`、`::1`、`100.64.0.1` 各一条 → 请求失败且**服务端没收到任何请求**（用 `httptest` server 的命中计数断言为 0）。
2. `TestHTTP_Dial_Rebinding`：解析函数第一次返回公网 IP、第二次返回 `127.0.0.1`（模拟重绑定），断言仍然拒绝。这条是"为什么必须自己解析"的证据。
3. `TestHTTP_AllowedHostsMatch`：`allowed_hosts: ["api.example"]` 配 `https://api.example.evil.com/` → 拒绝；配 `*.example` 时 `a.example` 通过、`a.example.evil.com` 拒绝。
4. `TestHTTP_IPHostRequiresExplicitAllow`：`allowed_hosts` 不含 `127.0.0.1:port` 时直连 IP 被拒；加入后仍因私网判断被拒（除非显式 `deny_private_ranges: false`，而本卡要测这条组合确实能通，用于本机回环调试场景）。
5. `TestHTTP_StatusClassification`：`expect_status:[200,201]` 下 201 → 成功；400 → 失败且 `Permanent`；429/500 → 失败且可重试；连接被拒 → 可重试。
6. `TestHTTP_BodyCaptureLimit`：响应 5KB、`max_body_bytes:1KB` → 产物里 1KB、`Truncated==true`、`OutBytes` 记录的是实际落盘字节数（注释里说明这个字段的语义，别让人以为是响应全长；如果选择记录全长，则新增字段并同步 E05）。
7. `TestHTTP_RedirectNotFollowed`：服务端 302 → 客户端不跟随，结果按 302 判定（不在 `expect_status` 则失败）。
8. `TestHTTP_SecretHeaderNotLogged`：带 `Authorization` 固定头 → 产物 `.err` 与日志里都不出现明文。
9. `TestHTTP_BadMethodOrBody`：GET 带 body、`body: json` 但 payload 非合法 JSON、payload 试图设置 `Host` → 提交期或执行前拒绝，且不起连接。
10. `TestHTTP_CancelAndTimeout`：服务端 `sleep` 后响应；分别测取消与 `timeout` 到期 → Handler 在有界时间内返回、`ExitError`/等价错误的 `Cancelled`/`TimedOut` 正确（HTTP 版的分类要与进程版一致，供 E12 复用）。
11. `TestHTTP_TLSHostNamePreserved`：用 `httptest.NewTLSServer` + 允许 `127.0.0.1` + `deny_private_ranges:false`，断言证书校验按原主机名进行（否则会出现"绕过了 TLS 校验"的隐蔽漏洞；必要时允许测试注入 `RootCAs`）。

## 6. 完成标准（DoD）

- [ ] 所有拒绝都发生在建连之前，有用例证明服务端没收到请求。
- [ ] 私网判断使用 `net.IP` 的既有方法，代码里没有手写网段表。
- [ ] HTTPS 的证书校验没有被绕过（第 5.11 条）。
- [ ] 状态码与错误的重试分类与 E12 共用同一函数。
- [ ] 响应体有硬上限，超限标记 `truncated`。
- [ ] `http` 档位在 `enabled=false` 时不存在；探测阶段（E03）不会因为网络不可达而报错。
- [ ] 不新增第三方依赖（只用标准库 `net/http`、`net`、`crypto/tls`）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./executor -run 'HTTP' -race -v
go test ./... -race
```

手工：配一个指向 `https://httpbin.org/status/500`（或本机另一个 `httptest` 服务）的档位，触发一次，检查 `GET /jobs/:id/result` 里能看到状态码与响应体尾部；再把 URL 换成 `http://127.0.0.1:8080/api/v1/health`（本服务自己），确认被拒且日志里有拒绝原因。

## 8. 不在本任务范围

- 不做 mTLS 客户端证书、代理配置、自定义 CA（需要时另立卡片）。
- 不支持 `${var}` 形式的环境变量插值到 URL。
- 不做"响应体解析后按字段判定成败"（本期只看状态码）。
- 不做请求重试以外的退避策略调整（复用 E12 与既有退避）。
- 不把 HTTP 执行器变成通用代理：`url`、`host`、`method` 都不允许由 payload 决定。

## 9. 风险与回滚

- 风险：地址判断的实现方式（自己解析再拨号 vs 依赖 `DialContext` 传入的 address）做错，防护会静默失效。第 3.3 条把它写成必查项，并要求测试用"服务端未收到请求"来证明，而不是只看返回错误。
- 风险：允许 `deny_private_ranges: false` 为本地回环调试保留一个例外配置，等于关掉这道防线。要求：显式设 false 时 E02 校验必须同时要求 `allowed_hosts` 全为具体主机名（E02 第 3.4 已含此规则），并在启动时记一条 warn；文档（E19）里写明这只允许在开发机使用。
- 风险：`Preview` 取响应体尾部，对 JSON 错误响应可能只看到半截。可接受（完整内容在产物文件里），但 `/result` 的默认 `from` 对本 kind 应偏向 `head`；本卡在代码里留一个 `preferredStreamDirection` 提示并在 E18 使用，避免前端各自猜。
- 回滚：`http.go` 独立文件；`Register` 的分流改动是一处 `switch`，回滚后 `http` 档位在 E02 校验阶段就会因为"kind 未实现"被拒（要求 E02 在校验时把 `kind:http` 当已支持处理，因此回滚后要同步把 kind 判定改回来——记录在提交信息里）。

## 10. 实现记录（2026-09-30）

落地文件：新增 `executor/http.go`（`HTTPRunner`、`NewHTTPRunner`、`Handler`、请求构造与地址渲染、
响应处理与产物写入、拨号层地址判定、`PreferredResultDirection`）与新增 `executor/http_test.go`
（15 条用例：§5.1…§5.11 各一条，另加连接层失败、`capture_response:false`、
错误档位分流与输出起点提示四条）；
改动 `executor/exit.go`（三个 http 失败类别、`httpStatusRetryable`、`newStatusFailure`）、
`executor/args.go`（`urlParamForbidden`、`checkURLParamValues`，以及 `checkSubmissionHeaders`
拒掉由执行器决定的三个请求头）、`executor/profile.go`（`checkURLTemplate` 拒绝出现在 `?` 或 `#`
之后的占位符）、`executor/proc.go`（并发许可的三个方法改成包内函数、KindHTTP 守卫文本）、
`executor/register.go`（按 kind 分流的 `handlerFor`、`warnRelaxedAddressPolicy`）与
`executor/register_test.go`、`executor/proc_test.go`。

**没有新增配置键**：§3 用到的每一项都已在 E01 定义、E02 校验，本卡只是让它们真的起作用。

### 与卡片的偏离与补充

1. **`NewHTTPRunner` 多一个 `*ArtifactStore` 参数**（§3.1 的签名里没有）。响应体与头信息要落到
   E06 的产物文件，存储必须传进来；传 `nil` 时执行直接判失败并说明"没有产物存储，响应没处可写"，
   与 `Runner` 同一条口径（这条判在 `Handler` 第一行，请求不会发出）。
2. **§3.3 的两个候选取"自己解析、按解析出的 IP 建连"，TLS 走 `DialTLSContext` 而不是
   `Transport.TLSClientConfig`**。原因写在 `dialTLSContext` 的注释里：按 IP 建连之后标准库从
   URL 主机名推 TLS 的 `ServerName`，主机名本身就是 IP 时推出来是空的，证书校验会对着一个空目标做。
   自己握手时把档位声明的主机名交给 `tls.Config.ServerName`：域名走 SNI，IP 字面量不发 SNI
   并按证书的 IP 主题备用名校验。`tls.Config` 只设 `ServerName` 与 `RootCAs` 两个字段，
   代码里不存在 `InsecureSkipVerify` 的写法（DoD 第三条）。
3. **§5.2 的重绑定用例做成了反向**（第一次答案给回环、第二次给私网），断言改成两条：
   `lookups==1`（解析只发生一次，第二份 DNS 答案没有机会被用到）与"防线开/关两次对照"
   （同一条档位关掉防线能连上本地服务、开着防线在建连前拒掉且对端零命中）。
   按卡片原文（第一次公网）写会让用例去拨一个可路由地址，测试因此变慢且结果取决于机器网络。
4. **§3.3 第 2 步的网段判断全部用 `net.IP` 的既有方法，只有一段例外**：RFC 6598 的
   `100.64.0.0/10` 标准库没有对应方法，卡片又明确把它列进拒绝清单，于是显式写了一个 `net.IPNet`
   （`sharedAddressSpace`）。DoD 第二条据此读作"除这一段之外没有手写网段表"。
   IPv4 映射地址（`::ffff:127.0.0.1`）在 §5.1 的用例里单独一条：`net.IP` 的方法会先折成 IPv4 再判。
5. **`ExecMeta.OutBytes` 记的是落盘字节数，不是响应全长**（§3.4 要求二选一并写注释）。
   超限判定用 `io.LimitReader(body, max+1)`，读到的比上限多一个字节即 `truncated=true`，
   写进产物的是前 `max` 字节。没有新增"响应全长"字段。
6. **预览不走 `Result.SetPreview`**（§3.4 让它填 `Preview`）：那个方法按"脚本失败时最有用的信息在
   stderr"的规则优先取 stderr，而 http 档位的 `.err` 写的是头信息，照那条规则预览会变成一串头名。
   改由 `fillBodyPreview` 用 `core.TrimExecPreview` 裁响应体尾部，裁剪口径与进程档位、事件侧、
   接口侧仍是同一个函数。`capture_response: false` 时不写预览。
7. **产物 `.err` 里不写请求体内容，只写字节数**（§3.4 只要求"请求与响应的头"）。
   请求体可能就是明文口令或业务数据，整份抄进一份给人排查用的记录里不是本卡能默认的事。
8. **`.err` 的请求行用打码后的地址**：声明为 `secret` 的参数值在请求行里写成 `<secret>`
   （`maskSecrets` 替换的是转义之后的值，因此不需要第二条渲染路径）。§3.2 只要求固定头打码，
   这一层是多做的。**响应体里回显的参数值没有打码**——那属于 E16：
   它的 §3.3 第 2 条与 §9 已经把"输出内容由对端产生、可能包含敏感值"列为自己的口径，
   处理方式是 `/result` 收严档位加 `redaction_note`，冒烟 B 组最后一条现场就是这个样子。
9. **超时只有一个来源**（§3.5）：`client.Timeout` 不设，档位与 payload 算出的生效超时在 `Handler` 里
   用 `context.WithTimeout` 套一次，拨号、握手、读体全挂在这一个上下文上。
   请求构造用的是 `http.NewRequest` + `execute` 里的 `request.WithContext(ctx)`，
   与 §3.5 写的 `http.NewRequestWithContext` 是同一机制：构造请求时还没有本次执行的上下文，
   那层超时是算完生效超时之后才产生的。
10. **"GET/DELETE 带请求体"在执行期拒**（§3.2 要求报错，没规定在哪一层）：E02 允许 GET 档位声明
    `body: json`（它只要求非 GET/DELETE 必须声明 body 来源），所以这一条落在 `newRequest`，
    判在拼完请求体之后、建连之前。冒烟 B 组第四条确认对端零命中。
11. **重试分类与 E12 共用同一张表**（§3.4、DoD 第四条）：`classifyExit` 新增
    `failureHTTPStatus`、`failureAddressRefused`、`failureTransport` 三个类别，`code` 这个参数的含义
    随类别切换（进程路径是退出码，http 路径是状态码）。
    状态码不进 `newFailure` 的 `exitCode` 位置——那个值会写进摘要的 `exit_code`，
    把 502 记成"进程退出码 502"是一条假信息，因此另造 `newStatusFailure`，摘要里只填 `http_status`。
12. **策略拒绝与连接失败分两类**：`errAddressRefused` 哨兵把"被网络策略拦下"从"连不上"里分出来，
    前者算永久失败（策略不会自己变，重试只是白占执行名额），后者整类可重试（§3.4）。
    `*url.Error` 一律先剥掉再落日志与产物：它的文本含完整 URL，而路径段里可能就是参数值。
13. **不设代理**：`Transport.Proxy` 显式留 `nil` 而不是继承 `ProxyFromEnvironment`。
    环境变量里的代理会另起一条到代理服务器的连接，而那条连接不在本卡的地址判断之内。
    §8 把"代理配置"列在范围外，这里的取值是"明确不开"。同时不强制 HTTP/2、保留 keep-alive。
14. **§4 第 4 步的分流写法**：`Register` 用 `handlerFor(profile, ...)` 按 kind 选执行主体，
    两条路共用一个 `profileHandler` 接口，注册代码只有一处。`Runner` 里那条 KindHTTP 守卫保留为
    兜底，文本从"归 TASK-E15"改成说明性的 `http profiles are executed by the http executor`，
    E09 期那条用例的断言同步改了这一个字符串。
15. **§9 第三条的 `PreferredResultDirection` 已经在代码里**（http 返回 `head`、其余返回 `tail`），
    但本期没有接进 `GET /api/v1/executors` 的响应：多一个字段就是改接口契约，
    归 E18 与前端一起定。已登记到 E18 卡与 E19 卡。
    因此本卡的用例只断言函数本身，接口上暂时读不到它。
16. **§9 第二条要求的启动 warn 已实现**：`warnRelaxedAddressPolicy` 在注册完成之后按档位列出
    `deny_private_ranges: false` 的 http 档位（`profiles=exec.a,exec.b`），配一条用例
    `TestRegister_WarnsWhenAddressPolicyDisabled`。E02 那条"这种取值下不许写通配主机"的规则
    只挡住一半写法，具体内网主机名的配置是能启动的，所以需要这一行。
17. **E07/E08 登记的两条 http 专属守卫在本卡补上**：`urlParamForbidden`（占位符值不得含
    `/ ? # @ : %` 与空白、反斜杠，见 `checkURLParamValues`）与 `checkURLTemplate` 拒绝出现在
    `?` 或 `#` 之后的占位符。§3.2 那句"占位符出现在查询段的用法本期不支持，配置里写了就报错"由此成立。
18. **并发许可从 `Runner` 的方法改成包内函数**（`acquirePermit`/`permitFailure`/`releasePermit`）：
    两条执行路径要用同一个"同一档位同时执行几个"的名额语义，留在 `Runner` 上就得复制一份。
19. **档位并发默认值、许可等待时长、产物命名与 `meta.json` 全部复用进程路径那一份实现**，
    http 侧没有另开一套。
20. **冒烟跑出来的一个缺陷已当场修掉**：首轮 B 组第一条的产物里固定请求头写成
    `x-source: godelayq-smoke`（小写）。原因是配置解码会把映射键折成小写（E08 记录过同一个现象），
    而 `cloneHeaders` 原来把键名原样放进 `http.Header` 这个 map，于是线路上与产物里都是小写；
    payload 那一路走的是 `Set`，同一个档位出现两种头名写法。改成 `Add` 之后两处都按 HTTP 惯例归一，
    `TestHTTP_SecretHeaderNotLogged` 加了一条"小写声明的头要以归一形式落盘"的断言，
    并重跑一次 B 组第一条确认线路上的对端也收到 `X-Source`。

### 验证结果

Windows 本机（`10.0.26200`，go1.26.4 windows/amd64）：

| 命令 | 结果 |
| --- | --- |
| `go build ./... && go vet ./...` | 通过 |
| `go test ./... -race -count=1` | api 84.6s、cmd/server 5.5s、core 12.5s、executor 22.0s 全 ok |
| `go test ./executor -run HTTP -race -count=3` | 15 条用例三轮全 PASS（拨号与 TLS 用例对时序敏感，按 DoD 要求重复跑） |
| `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64` 的 `go build` + `go vet` | 通过 |
| `go build -tags dashboard ./...` | 通过 |
| 既有用例 | `args_test.go`、`profile_test.go`、`exit_test.go`、`probe_test.go`、`api` 与 `cmd/server` 的断言一字未改，全绿；只有 `TestRunner_HttpProfileIsNotExecutedByProcessRunner` 改了断言的字符串（见第 14 条） |

冒烟（Windows 真实服务端 + REST，独立配置在临时目录；对端是本机 `127.0.0.1:18099` 的一个
Python `ThreadingHTTPServer`，它把每个到达的请求记进 `target.log`，"对端有没有收到"就数这一行；
提任务用静态 token，`executors.required_role: operator`）：

**A. 地址防线与状态码分类**

| 档位取值 | 现象 |
| --- | --- |
| `callback_ok`：`deny_private_ranges: false` + `http://127.0.0.1:18099/ok` | success；`exec.http_status=200`、`out_bytes=51`、`preview` 是响应体；`/result?stream=out` 给出响应体，`?stream=err` 给出 `> GET ...`、`> request body bytes: 0`、`< 200 OK` 与两侧头 |
| `callback_guarded`：同一 URL，`deny_private_ranges` 留默认（开） | failed、`permanent: true`、`http_status` 不写；对端 `target.log` 零新增行；`.err` 末尾 `! request failed: address refused by the profile's network policy: 127.0.0.1 (loopback address)`；服务端一条 `level=WARN msg="executor http target address refused" profile=callback_guarded host=127.0.0.1 address=127.0.0.1 reason="loopback address"` |
| `callback_500`：对端返 500，`expect_status: [200]`，`max_retries=2` | 三次执行（`retry_count=2/2`），退避照 E12 递增；每次都记 `http_status=500` 与响应体尾部；最终 failed 且 `permanent` 不为真 |
| `callback_404`：对端返 404，`max_retries=2` | 一次执行就落终态（`retry_count=0/2`）、`permanent: true`；对端只收到 1 个请求 |
| `callback_redirect`：对端返 302 带 `Location: /ok` | failed、`http_status=302`、`permanent: true`；对端只收到 `/redirect`，`/ok` 没有因为跟重定向而被访问 |

**B. 请求体、请求头与上限**

| 用例 | 现象 |
| --- | --- |
| `callback_post`（`method: POST`、`body: json`、固定头 `X-Source`、`header_allow: [X-Trace-Id]`，payload 给 `body` 与 `headers`） | success；对端回显 `content_type=application/json`、`body_bytes=18`（与请求体字节数一致）；`.err` 里 `> Content-Type: application/json`、`> X-Source: godelayq-smoke`、`> X-Trace-Id: trace-smoke-2` |
| 同一档位 payload 覆盖不在 `header_allow` 里的头 | 执行前拒绝、对端零命中；事件 `job.failed` 的 error 文本 `invalid submission: headers.X-Source: not in the profile's header_allow (X-Trace-Id)` |
| 同一档位 payload 的 `body` 是裸字符串 | 执行前拒绝、对端零命中（`json` 模式只接受对象或数组） |
| `callback_ok`（GET、`body: none`）payload 硬塞 `body` | 执行前拒绝、对端零命中 |
| `callback_big`（`max_body_bytes: 1024`，对端返 6035 字节） | success、`out_bytes=1024`、`truncated: true`；`/result?stream=out` 长度是 1024 |
| `callback_nocapture`（`capture_response: false`） | success、`out_bytes=0`、`preview` 空；`.err` 仍写两侧头 |
| `callback_secret`（`args` 里 `token` 声明 `secret: true` + 固定 `Authorization` 头） | success；`.err` 请求行是 `> GET http://127.0.0.1:18099/orders/<secret>`、`> authorization: <redacted>`；日志与产物里没有固定头的值。响应体把参数值回显出来了（`{"order": "SUPERsecret123", ...}`），`preview` 因此带着它——见第 8 条，这条归 E16 |

**C. 超时**

`callback_slow`（档位 `timeout: 2s`，对端睡 10 秒）：1.998 秒返回，failed、
`http_status` 不写、`permanent` 不为真（超时可重试），`.err` 末尾
`! request failed: context deadline exceeded`。取消路径只在单元测试里覆盖
（`TestHTTP_CancelAndTimeout/cancel`）：Git Bash 无法向 Windows 进程发 SIGTERM，
手工构造"用户点取消"需要另开一个脚本在 2 秒内打到 `POST /jobs/:id/cancel`，
本轮没有做（见"未验证"）。

### 未验证与遗留

- Linux/macOS 的真实运行与那一侧的 `-race`（WSL2 缺 gcc），与其他卡片同一条遗留。
- 真实外网目标（§7 写的 `https://httpbin.org/...`）没有跑：冒烟一律用本机对端，
  不依赖外部网络与证书链。
- 手工取消路径未跑（见 C）。
- 接口上读不到 `PreferredResultDirection`（第 15 条），归 E18。
- 响应体回显 `secret` 参数值的掩码与 `/result` 收严档位，归 E16（第 8 条）。
- 冒烟里看到一条与本卡无关的既有日志写法值得跟进：档位超时触发的执行在 `core` 的事件里记成
  `msg="job timed out" timeout=0s`，那个 `timeout` 取的是任务自己的超时字段（本次为空），
  不是真正生效的档位超时。属于 E09 之前的既有行为，登记待处理。
- **重试的产物会被后一次覆盖**（A 组 `callback_500` 现场）：`core/job.go` 的 `CloneForRetry`
  不把 `Attempts` 带给重试副本，于是三次执行的 `job.Attempts` 都是 1，
  产物文件名一直是 `a1.out`/`a1.err`，后一次把前一次的内容盖掉，接口上 `?attempt=1` 只能读到
  最后一次。E06 §2 写明"每次尝试必须按 attempt 分文件，否则第二次尝试会覆盖第一次"，
  而 `TestArtifactWriter_PerAttemptFiles` 只证明存储能容纳 1/2/3 三个独立文件——
  调度器这一侧从来没给出过 2 和 3。进程档位同样如此，不是本卡引入的行为，
  改法在 `core`（要么带 `Attempts`，要么让重试副本用 `RetryCount+1` 编号），登记待处理。
