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
