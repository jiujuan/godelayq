# TASK-N05　executor：提交期的路径/URL 校验与执行侧渲染

- 所属阶段：M1 配置与档位
- 依赖任务：TASK-N04
- 涉及文件：`executor/adhoc.go`、`executor/args.go`、`executor/http.go`、`executor/proc.go`、
  `executor/args_test.go`、`executor/http_test.go`、`executor/adhoc_test.go`、`executor/proc_test.go`
- 预计规模：大

## 1. 任务目标

让内置 adhoc 档位真的按任务给出的位置执行：脚本类读 payload 顶层的 `script`，
HTTP 类读 payload 顶层的 `url`，两者都在**提交期**完成全部校验，非法值在
`POST /api/v1/jobs` 就拿 400，合法值在执行侧被组装成 argv 或请求。

本卡结束时 `{"type":"exec.php","payload":{"script":"D:/work/a.php"}}` 会真的跑那个文件；
`{"script":"D:/work/../windows/system32/cmd.exe"}`、`{"url":"http://127.0.0.1:8080/x"}`
这类取值各有明确的拒绝点。

## 2. 背景与当前问题

payload 的顶层键集合写死为六个：`args`/`env`/`params`/`headers`/`body`/`timeout`
（`executor/args.go:71-73`，未知键在 `:105-111` 被拒）。执行侧的 argv 前两项来自档位固定字段
（`Render`，`executor/args.go:531`，script 分支在 `:541` 用的正是 `p.Runtime, p.ScriptPath`），
URL 来自档位模板（`executor/http.go:247` `fillTemplate`，`AllowedHosts` 在 `:265` 处判定）。
所以"位置来自任务"需要同时打开三个口子：payload 键、argv 组装、URL 组装。

位置为什么不走 `args`/`params`（§D7，两条理由都要在代码注释里出现）：

1. HTTP 的整条 URL 放进 `params` 必然被拒：占位符值禁字符集 `urlParamForbidden`
   含 `:`、`/`、`?`、`#`、`@`、`%`、`\` 与空白（`executor/args.go:406-434`）。
2. 位置是"跑哪个可执行体"的身份，不是参数值。`web-profile-design.md:38` 的 D7 正是按
   这条区分身份与参数，把它塞进 `args` 会让那条边界在概念上先塌掉。

## 3. 要实现的功能

### 3.1 payload 顶层两个新键

| 项 | 位置 | 改动 |
| --- | --- | --- |
| 键集合 | `executor/args.go:71-73` | 加 `"script"`、`"url"` |
| 键归属 | `ValidateSubmission`（`:96`）的 `switch p.Kind` 之前 | 只有 `p.Adhoc` 的档位接受这两个键；非 adhoc 档位带了要报错 |
| 错误文案 | `submissionKeys`（`:197`） | 允许键列表按档位算：普通档位不含 `script`/`url`，adhoc 档位只含本类型那一个 + `timeout`。这条必须精确，否则运维会误读成"新版本不支持这个写法" |
| 承接字段 | `Submission`（`:23`） | 加 `Script string`、`URL string` |

归属规则（每条都要有用例）：`exec.php`/`exec.python`/`exec.shell` 只接受 `script`，
带 `url` 报错；`exec.http` 只接受 `url`，带 `script` 报错；两者都必填（缺失的 400 文案
点名缺哪个键）；`args`/`params`/`env`/`headers` 对 adhoc 档位仍然按 kind 归属判
（现有 `:123-152` 那一套不动），`timeout` 照旧。

### 3.2 脚本位置校验（新函数，放 `executor/adhoc.go`）

```go
// checkAdhocScript 判"这条任务要跑哪个文件"，返回解析后的绝对路径。
func checkAdhocScript(p *Profile, raw string, ec core.ExecutorsConfig) (string, error)
```

依次判（每步的拒绝理由不同，错误文本要能区分）：

1. 非空、trim 后仍非空。
2. 无控制字符：复用 `containsControl`（`executor/profile.go:1138`）。
3. 无 shell 元字符：复用 `firstShellSpecial`（`:1129`）。
   即便 argv 直传不过 shell（`executor/args.go:529` 的 D3），这一步仍然保留：
   它是档位参数值一直在用的那条守卫，去掉它等于让 adhoc 的参数比用户档位的参数更宽。
4. 能算出绝对路径：复用 `resolveAnywhere`（`:959`）。
   Windows 反斜杠、盘符、`..` 都是合法写法（`PathAnywhere` 的语义）。
5. 命中 `executors.adhoc.path_prefixes` 之一（相对路径按 `executors.workspace` 解析）；
   空列表按不限处理（§12 P1）。用 `withinDirectory`（`:1054`）判前缀，
   不要用字符串 `HasPrefix`——后者会把 `D:/work/scripts-evil` 算进 `D:/work/scripts` 之内。
6. 扩展名匹配（`executors.adhoc.require_extension` 为 true 时）：
   php→`.php`；python→`.py`；shell→`.sh`/`.bash`。大小写不敏感（`.PHP` 要能过）。
7. 文件存在且是普通文件：复用 `fileCheckReason`（`executor/probe.go:115`）。
   目录、不存在、无权限各给不同理由。**这一步失败是 400 而不是排队后失败**：
   提交时文件不在，等到执行时更不可能在，早拒早好。

返回值写进 `Submission.Script`，同时把绝对路径记进执行日志（沿用 `Runner.logRun`，
`executor/proc.go:325`）与产物文件名口径，不改它们的既有形状。

### 3.3 URL 校验（同文件）

```go
// checkAdhocURL 判"这条任务要打到哪里"，返回规范化后的地址。
func checkAdhocURL(p *Profile, raw string, ec core.ExecutorsConfig) (*url.URL, error)
```

依次判：非空 → 无空白与控制字符 → `url.Parse` → scheme 只允许 `http`/`https`
→ 不许带 `user:pass` → `Host` 非空 → 命中 `executors.adhoc.url_hosts`
（空=不限；判定复用 `hostAllowed`，`executor/profile.go:885`）→ 返回 `u.String()` 的规范化写法。

**地址范围守卫不在这里做**：DNS 解析结果要在真正建连前判才有意义，
现成的 `allowedIP`/`refusalReason`（`executor/http.go:705`、`:744`）已经负责，
`DenyPrivate` 由 N04 在构造时按 `url_allow_private` 取反写进档位。
错误文本不回显整条 URL（沿用 `fillTemplate` 的同一条理由，`executor/http.go:258` 的注释：
地址里可能带口令）。

### 3.4 执行侧渲染

| 位置 | 改动 |
| --- | --- |
| `executor/args.go:531` `Render` 的 `KindScript` 分支 | `p.Adhoc` 时 argv 前两项用 `[]string{p.Runtime, sub.Script}`；非 adhoc 走 `:541` 原路径逐字不变 |
| `executor/http.go:247` `fillTemplate` | `p.Adhoc` 时把 `sub.URL` 当作已渲染结果，其余三道判据（scheme、凭据、`hostAllowed`）**继续跑**——不能因为"提交期判过"就在这里短路，执行期这层是防"任务入队后配置变了" |
| `executor/http.go:230` `renderURL` | 产物里记录的打码写法对 adhoc 用同一份地址（`target.Redacted()` 已有），不新增第二条路径 |
| `executor/args.go:556` 之后的 `ArgsRender` 循环 | adhoc 档位没有 `ArgsRender` 与 `Positional`（N04 构造时就不给），本卡只确认它们为空时循环自然跳过 |

### 3.5 永久失败分类

位置在执行期消失（文件被删、DNS 解析失败）应按"参数问题"归永久失败不重试，
沿用 `core.ExecMeta.Permanent`（`core/job.go:111`）与既有失败分类表（`executor/exit.go`、
`executor/result.go`）。这一条如果做不到，就把"执行期文件不存在"归到既有不可重试那一类并在 §10 记录。

## 4. 实现步骤

1. 在 `executor/adhoc_test.go` 先写 §3.2、§3.3 的表驱动用例（通过组 + 逐条拒绝组），全红。
2. 改 payload 键集合与 `Submission`（§3.1），跑一遍 `go test ./executor` 确认既有 payload 用例零改动。
3. 实现 `checkAdhocScript`、`checkAdhocURL`（§3.2、§3.3），转绿。
4. 改 `Render` 与 `fillTemplate`（§3.4），补 `executor/args_test.go`、`executor/http_test.go`
   的 adhoc 用例，同时断言"非 adhoc 档位的 argv/URL 结果与改动前一致"。
5. 失败分类（§3.5）。
6. `gofmt -w` 改动文件；`go test ./executor -race`；`go build ./... && go vet ./...`。

## 5. 测试要求

| 组 | 用例 |
| --- | --- |
| 键归属 | `exec.php` 带 `url` → 400；`exec.http` 带 `script` → 400；缺位置键 → 400 且文案点名键；普通档位带 `script` → 400 且允许键列表里**不含** `script` |
| 路径通过 | 绝对路径、盘符、`..` 写法、反斜杠、带空格、中文目录（注意：中文在路径里合法，与任务名称规则无关）各一条 |
| 路径拒绝 | 空串、含 `\n`、含 `;`/`|`/`&&`、指向目录、不存在、前缀不匹配、前缀相似目录（`scripts-evil` 那条）、扩展名不符、`require_extension=false` 时同一取值通过（两条对照） |
| URL 通过 | `https://api.example.com/healthz`、带端口、带查询串、`url_hosts` 命中、`*.example.com` 通配命中 |
| URL 拒绝 | `ftp://…`、`http://user:pass@host/`、空白、`javascript:alert(1)`、无主机、主机不在 `url_hosts`、`url_allow_private=false` 时打 `http://127.0.0.1:8080/` 与 `http://169.254.169.254/`（走 `refusalReason`，断言错误文本与实际建连次数：不建连） |
| argv | `exec.php` 的 argv 恰为 `[php, <绝对路径>]`（含空格路径仍是一个元素，照 `executor/args_test.go` 既有断言体例） |
| 回归 | 用户 script 档位的 argv 用例一条不改全绿；用户 http 档位的 URL 用例一条不改全绿 |
| 超时 | `payload.timeout` 对 adhoc 仍受 `executors.max_timeout` 约束（`executor/args.go:487` `EffectiveTimeout`） |
| 掩码 | adhoc 不声明 secret 参数，因此位置原文允许出现在响应与产物里；用一条用例把这条事实钉住，避免后续误加掩码 |

## 6. 完成标准（DoD）

1. §3.1 的四项归属规则逐条有用例，错误文案精确到"该档位实际接受哪些键"。
2. 非法路径/URL 全部在 `ValidateSubmission` 这一步被拒（提交期），
   用例证明"400 时任务没有进堆"。
3. `Render` 与 `fillTemplate` 的 adhoc 分支不改变非 adhoc 的任何输出（既有用例零改动为证据）。
4. `hostAllowed` 与 `refusalReason` 两层在执行期仍然生效：
   用例证明执行期改写配置后（或构造时 `url_hosts` 收紧后）不会真的外连。
5. 位置在执行期消失时的重试行为按 §3.5 落定，并有测试证据或 §10 记录。
6. `go test ./executor ./core -race -timeout 30m` 绿；`go build ./...`、`go vet ./...` 无输出。

## 7. 验收方式

```bash
go test ./executor -run "Adhoc|Render|Submission" -v
go test ./executor -race -timeout 30m
go build ./... && go vet ./...
```

手工（临时目录，本机有 bash 时）：在临时 workspace 写一个打印参数的 `.sh` 文件，
用 `exec.shell` 提交带该路径的任务，确认产物文件里有它的输出；
再用一个不存在的路径提交，确认 400 且响应文本说明"文件不存在"。

## 8. 不在本任务范围

- 不改 `/executors` 响应形状（N06）。
- 不改前端（N07）。
- 不做"列出可选脚本"的接口（README S-4）。
- 不给 adhoc 增加 `args`/`env` 能力（本期位置键之外不开放参数注入，见 §8 设计文档的"没动的"第 3 条）。
- 不改 `executors.commands` 档位的 payload 规则。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| 提交期判过、执行期短路 | 入队后改配置就能绕过主机与地址守卫 | §3.4 明确要求执行期三道判据继续跑；DoD 第 4 条 |
| 前缀判断用 `HasPrefix` | 相似目录被放行 | DoD/用例专门有一条 `scripts-evil` |
| 错误文本回显整条 URL 或路径 | 凭据与内部路径进响应与日志 | 沿用 `executor/http.go:258` 那条不打回显的原则；路径类允许回显（它是配置事实的一部分），但只回显解析后的绝对路径而不是原文 |
| 位置键与 `args` 混用造成误读 | 用户以为可以 `{"args":{"script":…}}` | 用例断言这种写法被拒且文案指向 `script` 顶层键 |

回滚：把 §3.4 的两个分支去掉即恢复原行为，payload 键集合的扩展不会生效
（非 adhoc 档位拒 `script`/`url`，而 adhoc 档位不再被 N04 注册）。

## 10. 实现记录（执行时补写）

落地：`executor/adhoc.go`（`takeAdhocLocation`/`checkAdhocScript`/`checkAdhocURL`/
`firstPathSpecial`/`adhocPathPrefixes`）、`executor/args.go`（`Submission.Script`/`URL`、
`submissionFieldNames`（`:93`）加两键、`decodeSubmissionFields`（`:218`）跳过位置键、
`submissionKeys`（`:235`）的 adhoc 分支、`ValidateSubmission`（`:148`）插入 `takeAdhocLocation`、
`Render`（`:587`）的 argv 分支）、`executor/http.go`（`resolveTarget`（`:251`）与
`checkTarget`（`:267`）：模板路径与任务路径共用同一份地址判据）、
`executor/profile.go`（`Profile.AdhocPathPrefixes`）、新测试 `executor/adhoc_validation_test.go`（12 个顶层用例）。

| # | 与卡面的偏离 | 原因 |
| --- | --- | --- |
| 1 | `checkAdhocScript` / `checkAdhocURL` 都不收 `ec core.ExecutorsConfig`，签名是 `(p *Profile, raw string)` | 判据需要的三样（workspace、允许的目录、扩展名 / 主机名单）在 N04 构造时已经落在 `Profile` 字段上。再传一份 `ec` 就成了"档位读字段、校验读配置"两个来源，而同一次执行里这两份可以不一样（`ValidateSubmission` 的签名也没有这个参数，为它加参数要动到 api 与执行侧全部调用点） |
| 2 | §3.2 第 3 步"复用 `firstShellSpecial`"改成新函数 `firstPathSpecial`（`executor/adhoc.go:367`），字符集只少一个反斜杠 | 卡面自相矛盾：§5 的路径通过组要求"反斜杠"通过，而 `firstShellSpecial`（`executor/profile.go:1204`）把 `\` 列为 shell 元字符——Windows 的绝对路径必带它。其余八个字符（分号、竖线、与号、反引号、美元符、大于号、小于号、换行回车）一个没减，argv 直传不过 shell 那条底线（D3）也没动 |
| 3 | `checkAdhocURL` 返回 `string`（规范化写法），不是 §3.3 签名的 `*url.URL` | `Submission` 里存的都是能直接比较与序列化的简单值，payload 掩码与任务快照按字符串处理；执行侧 `resolveTarget` 会在 `checkTarget` 里再解析一次并判 scheme、凭据与名单，`*url.URL` 在提交侧没有任何消费者 |
| 4 | "另一类型的位置键"与"缺位置键"两条规则合成一段循环（`takeAdhocLocation` 的 `:213`） | 于是 `{"type":"exec.php","payload":{"url":"..."}}` 的 400 文本说的是"url 不被 php 接收、这条档位收 script 与 timeout"，而不是"缺 script"。卡面 §3.1 要求两条各有一处用例——`TestAdhocSubmission_KeyOwnership` 两条都在，措辞按实际输出断言 |
| 5 | `decodeSubmissionFields` 对 `script`/`url` 走 `continue`（`executor/args.go:218`），`submissionFieldNames` 里加这两键只为了让它们走到"这条档位不接受这个键"那句文本上 | 若在解码阶段按形状解码，普通档位收到 `"script":{...}` 会先报出 JSON 类型错误，把更有用的那句挡掉。这也是本卡返工过一次的地方：漏掉 `continue` 时 `TestValidateSubmission_UnknownKeys` 报 `json: Unmarshal(nil)` |
| 6 | §3.4 表里的 `fillTemplate` 没有保留，改动落成新函数 `resolveTarget` | adhoc 分支与模板分支合完之后 `fillTemplate` 只剩两行、与 `resolveTarget` 的非 adhoc 分支逐字相同。留两份等于把"真正要访问的地址怎么判"复制成两条链，`renderURL` 只需要一个入口 |
| 7 | 执行期的主机名单判断由 `ValidateSubmission` 重跑那一遍先拦下（`is not in executors.adhoc.url_hosts`），`checkTarget` 的那句（`is not in the profile's allowed_hosts`）在同一调用链上随后再判 | 卡面 §3.4 要求执行期三道判据继续跑——确实继续跑，只是先撞上的是重校验那道。用例 `TestHTTPRunner_AdhocHostGuardRunsAtExecution` 断言的是结果：没有连接、没有解析、永久失败、摘要与错误同源 |
| 8 | §3.5 落定在"重跑判据 → invalid submission → 永久失败"，不是"起进程失败" | 执行侧对 payload 重跑的是同一个 `takeAdhocLocation`，文件在执行期消失时根本走不到 `cmd.Start`：不起进程、不留产物文件。用例因此断言 `job.Exec.Artifact != core.ArtifactAvailable`，而不是去读 `a1.meta.json` |
| 9 | `checkTarget` 的名单判断写成"有名单才判"（`len(AllowedHosts) > 0`），不写成"内置档位才判" | 空名单只可能来自 `executors.adhoc.url_hosts` 留空（`executors.commands` 的 http 档位必须有非空名单，那条判据在 `checkAllowedHosts`，一字未改）。写成"有名单才判"不可能放松普通档位，写成"内置才判"会被复制成两份规则 |

DoD 核对：

1. §3.1 四项归属规则逐条有用例：`TestAdhocSubmission_KeyOwnership`（五种组合）+
   `TestSubmissionKeys_ListedByProfileKind`（内置两档各返回 `["script","timeout"]` / `["url","timeout"]`，
   普通 script/http 档位的允许键列表分别仍是 `args, env, timeout` 与 `params, headers, body, timeout`，
   不含 `script`/`url`）。
2. 非法路径/URL 全在 `ValidateSubmission` 拒：`TestAdhocSubmission_ScriptLocationRejected`（12 条）、
   `TestAdhocSubmission_URLLocationRejected`（9 条）。"400 时任务没有进堆"由 api 侧的门禁保证（N02 已把
   `gateExecutorSubmission` 改成按 `HandlerKey` 取档位并复用同一次 `ValidateSubmission`），
   本卡的执行侧证据是 `TestRunner_AdhocScriptGoneAtExecutionIsPermanent`：判据失败时不起进程、不留产物。
3. `Render` 与地址链的非 adhoc 输出未变：`TestRender_NonAdhocUnchanged` 断言 `[bash, <绝对脚本>, --day=today]`，
   既有用例零改动，`go test ./executor ./api ./core` 全绿（executor 24.1s / api 23.1s / core 19.6s）。
4. 执行期两层地址守卫仍生效：`TestHTTPRunner_AdhocRefusesPrivateAddress`（127.0.0.1 与 169.254.169.254 各一条，
   `errAddressRefused` + `hits==0` + 一条 warn；对照条 `url_allow_private=true` 时 `hits==1`）与
   `TestHTTPRunner_AdhocHostGuardRunsAtExecution`（收紧名单后 `hits==0`、`lookups==0`）。
5. 位置在执行期消失的重试行为按上表第 8 条落定，用例为 `TestRunner_AdhocScriptGoneAtExecutionIsPermanent`。
6. `go build ./...`、`go vet ./...` 无输出；`go test -race -timeout 30m ./executor ./api ./core` 绿
   （executor 27.2s / api 222.8s / core 21.7s，2026-10-06）。

变异反向验证两次（`go test -overlay`，中间态副本在 `n05probe`，真实工作树未改）：

1. 把 `checkAdhocScript` 的 `withinDirectory(prefix, absolute)` 换成 `strings.HasPrefix(absolute, prefix)` ⇒
   `TestAdhocSubmission_PathPrefixesLimitTheDirectory` 在 `scripts-evil` 那条报
   `An error is expected but got nil`（风险表第 2 条的判据确有牙）。
2. 把 `adhocCommand` 的 `denyPrivate := !ec.Adhoc.URLAllowPrivate` 换成 `denyPrivate := false` ⇒
   `TestHTTPRunner_AdhocRefusesPrivateAddress/169.254.169.254` 报 `expected 0 actual 1`（真建了连），
   并失去那条 warn；同组的 `TestAdhocProfiles_PrivateRangeSwitchReachesProfile` 同时变红。

手工验收（卡片 §7，临时目录）：`TestRunner_AdhocScriptRunsJobPath` 已经把这一条自动化了——
`exec.shell` 跑 `<workspace>/jobs one/print path.sh`，脚本 `printf 'ran:%s\n' "$0"`，
产物 `.out` 里读到 `ran:C:\...\jobs one\print path.sh`，路径含空格仍是一个 argv 元素。
"用不存在的路径提交拿 400"由 `TestAdhocSubmission_ScriptLocationRejected` 的"文件不存在"那一条覆盖
（错误文本 `script file "..." does not exist`）。

本卡新增用例清单（`executor/adhoc_validation_test.go`）：路径通过组 7 种写法、路径拒绝组 12 条、
限定目录与相似前缀目录、扩展名要求可关（对照两条）、URL 通过组 5 条、URL 拒绝组 9 条（含不回显整条地址）、
键归属 5 组、`submissionKeys` 三档对照、超时上限三条、adhoc argv 两条、非 adhoc argv 回归、
私网拒连两条 + 放开对照、执行期名单收紧、产物里的地址原样、掩码不吞位置、执行期文件消失的永久失败。

