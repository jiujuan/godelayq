# TASK-N08　文档收口、全仓验证与端到端实测

- 所属阶段：M4 收口
- 依赖任务：TASK-N01 … TASK-N07 全部
- 涉及文件：`docs/api.md`、`README.md`、`docs/design/job-name-type-and-adhoc-execution.md`（状态行）、
  `docs/design/executor-design.md` 与 `docs/design/web-profile-design.md`（冲突段的指针）、
  `web/src/content/job-template.md`（复核）、本目录 `README.md` 与各卡的 §10
- 预计规模：中

## 1. 任务目标

把 N01…N07 的成果收口成"使用者按文档做能一次跑通"的状态：
四份使用者/设计文档与新事实一致，全仓验证一次跑绿并逐条留原始输出，
四类自由执行类型各建一条真实任务跑到终态，后续项与已知限制逐条登记。

## 2. 背景与当前问题

本系列动的是接口语义（名称/类型）、配置面（一整节新键）、执行面（提交期给出的路径与 URL）
三件事，文档分散在四个地方：

| 文档 | 会失配的说法 |
| --- | --- |
| `docs/api.md` | 创建任务请求体、`GET /jobs` 查询参数、`/executors` 响应、角色档位表（`:86` 一带列了 viewer 能读的端点） |
| `README.md` | 功能列表与"任务类型 = 注册的处理函数"这类说法 |
| `docs/design/executor-design.md` | 档位身份不可变、脚本必须在 workspace 内（`:99`、`:145` 一带） |
| `docs/design/web-profile-design.md` | D5/D7 那两条与本文 §8 直接相邻（`:38`） |

`web-profile-design.md` 收口时（W09）用的是同一套做法：在冲突文档顶部加状态与"冲突处理"指针，
而不是把两份内容合并——本卡沿用。

## 3. 要实现的功能

### 3.1 设计文档

1. `docs/design/job-name-type-and-adhoc-execution.md` 顶部状态行改成"已实施（TASK-N01…N07 落地）"，
   并按 §D1…§D15 补一段"落地位置"（体例照 `web-profile-design.md` §2 末尾那段）。
2. 与 `executor-design.md`、`web-profile-design.md` 冲突的三条（身份不可变 D7、
   脚本必须落在 workspace、http 档位 `allowed_hosts` 不许空）在**那两份文档里各加一句指针**：
   "自由执行档位（`executors.adhoc`）是这一条的显式例外，见 `job-name-type-and-adhoc-execution.md` §8"。
   不改那两份的原文，也不删任何既有决策条目。

### 3.2 `docs/api.md`

| 节 | 要写清 |
| --- | --- |
| 创建任务 | `name` 与 `type` 的两种写法（带 `type` → 名称是标签、套字符规则；不带 → 名称兼作注册键、行为与旧版一致），三个请求示例照设计文档 §4.2 抄 |
| 任务响应 | `type` 字段可为空（旧写法），前端与脚本要按 `HandlerKey` 语义理解 |
| `GET /jobs` | 新增 `?type=`；`?name=` 的语义变成"按标签精确匹配" |
| `PUT /jobs/:id` | 名称与类型都不可改，传了不同值 → 400 |
| `/executors` | `adhoc` 与 `location` 两项；`GET /job-types` 里会出现 `exec.php` 这类键 |
| 角色与档位 | 提交自由执行类型需要 `executors.required_role`（默认 admin）；打开 `executors.adhoc.enabled` 后的风险说明一句 |
| 批量 | 两种写法混在一个批次里的逐条结论 |

### 3.3 `README.md` 与界面文档

- `README.md`：功能列表补"任务名称是标签、类型决定执行、四类自由执行类型"；
  配置示例段落同步 `executors.adhoc` 的关键默认值。
- `web/src/content/job-template.md`：复核 N07 的改写没有残留旧说法
  （`grep -rn "已注册的任务类型\|任务名称（="`）。

### 3.4 全仓验证（每条都要留原始输出文件，一个卡一个文件）

```bash
go build ./...
go vet ./...
go test ./...
go test -race -timeout 30m ./...
go test ./core -run TestExampleConfigMatchesLocal -v
go test ./core -run TestEveryLeafKeyIsClassed -v
cd web && npx vue-tsc --noEmit && npm run build
go build -tags dashboard ./cmd/server
```

交叉构建三条（既有收口卡口径）：`GOOS=linux`、`GOOS=darwin`、`GOARCH=386` 各一次，
以及"受平台限制没跑的项目"逐条列清而不是含糊带过。

### 3.5 端到端实测（临时目录，四类各一条）

冒烟环境按本仓库既有配方：`-tags dashboard` 内嵌形态、独立配置 + 数据 + 产物目录、
`GODELAYQ_EXECUTORS_ENABLED=true`、`GODELAYQ_EXECUTORS_ADHOC_ENABLED=true`、
`GODELAYQ_SERVER_AUTH_TOKEN` 或 users 凭据、端口挑 IPv4/IPv6 都空闲的。
PHP/Python 本机没装就按"探测不可用"记录（`runtime_ok=false`，提交 400），
并用 `exec.shell`（`bash`）与 `exec.http` 两类跑通真实执行；没跑到的类别在记录里写明"未验证 + 原因"。

必须覆盖的清单（每条记录请求体、状态码、任务终态、产物或响应片段）：

| # | 场景 | 预期 |
| --- | --- | --- |
| 1 | `{"name":"每晚对账","type":"exec.php","payload":{"script":"<存在的 .php>"}}` | 201 → success，产物里有脚本输出 |
| 2 | 同上的 `exec.shell` 与 `exec.http`（打到本机一个公开端点或本机公网地址；不能用回环，见第 6 条） | 各一条 success |
| 3 | `type` 缺失、`name` 是中文标签 | 400（类型未注册） |
| 4 | `name` 带空格或下划线、`type` 正常 | 400，`Details` 来自名称规则 |
| 5 | 只带 `name:"payment_check"` 的旧写法 | 201，`type` 为空，仍能执行 |
| 6 | `{"url":"http://127.0.0.1:<port>/"}` | 执行期按 `refusalReason` 拒（`loopback address`），**不外连**；改 `url_allow_private: true` 后放行（两条都要） |
| 7 | 路径指向 `path_prefixes` 之外、指向目录、不存在的文件、扩展名不符 | 各一条 400，理由互不相同 |
| 8 | `operator` 身份（默认 `required_role=admin`）提交 `exec.php` | 403 |
| 9 | `executors.adhoc.enabled=false` 时提交 `exec.php` | 400（类型未注册），且 `/executors` 里四条内置条目不出现 |
| 10 | 列表 `?type=payment_check` 同时命中新旧两种写法；`?name=` 只命中标签 | 断言条数 |
| 11 | 删除一个与内置键同名的用户档位后重启 | 内置那条让位还是生效，与 N04 §D15 一致（记录实测结论） |
| 12 | 崩溃恢复：adhoc 任务在跑时杀进程再启 | 按 `executors.restore_policy` 钉 paused（`cmd/server/main.go:945` 走的是注册键），不是"消失" |

界面实测（`-tags dashboard` 打开控制台）：N07 §5 的七条重做一遍，
因为那一刻的 `web/dist` 是最终构建产物。

### 3.6 后续项与限制登记

把设计文档 §10 的 12 行风险表与 README 的 S-1…S-5 逐条核对现状：
已落地写"已修 + 证据位置"，未实施写"未实施 + 实测证据"。
新发现的缺陷登记为 `D-10xx` 编号（延续本仓库既有编号习惯），每条自带处置（已修 / 登记不修 + 理由）。

## 4. 实现步骤

1. 先跑 §3.4 的全仓验证，把失败项逐条解决或登记（**环境类失败当场修，不留"既有问题"**）。
2. 改四份文档（§3.1-§3.3），每份改完 `grep` 复核关键词。
3. 搭临时目录冒烟环境，跑 §3.5 十二条清单，逐条留原始输出（一份场景一个文件）。
4. 界面实测。
5. 写 §3.6 的登记表，更新本目录 README 的状态表与各卡 §10。
6. 分批提交：文档一批、验证记录一批、每张卡的 §10 补写一批。

## 5. 测试要求

本卡不新增测试代码，要求是"证据齐"：

- §3.4 每条命令一份输出文件，路径记在收口记录里。
- §3.5 十二条每条至少两行：请求体原文 + 响应/终态原文（状态码、`Details`、产物片段）。
- 变异反向验证至少三处，逐条留原始输出：①把 N02 的门禁改回按 `job.Name` 取档位 → 相关用例红；
  ②把 N01 的名称正则里的 `\p{Han}` 去掉 → 中文名称用例红；
  ③把 N05 的执行期 `hostAllowed` 判据短路 → 私网用例红。
- 未验证项逐条列明原因（例如"本机无 PHP"、"Windows 专属路径写法未在 Linux 上实测"），
  不允许用"应该没问题"代替。

## 6. 完成标准（DoD）

1. 四份文档之间无冲突说法（`grep` 三条关键词：`已注册的任务类型`、`任务名称`、`allowed_hosts 不许空`），
   冲突处都有指向本文设计文档的指针。
2. §3.4 全部命令跑绿，交叉构建三条通过，输出留档。
3. §3.5 十二条每条都有实测记录或明确的"未验证 + 原因"；至少两类（Shell、HTTP）真实执行到终态。
4. §5 三条变异反向验证的原始输出留档。
5. 设计文档状态行改为已实施，§D1…§D15 的落地位置补齐；README 状态表全部"已完成"。
6. 缺陷登记表每行都有处置（已修 / 登记不修 + 理由），编号延续既有习惯。
7. `go test ./... -race -timeout 30m` 最终一次跑绿。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race -timeout 30m
grep -rn "已注册的任务类型" docs README.md web/src
grep -rn "job-name-type-and-adhoc-execution" docs/design
```

预期：测试全绿；第一条只在历史性的"原说法/已改"说明里出现（或零命中）；
第二条在 `executor-design.md`、`web-profile-design.md`、本目录 README 里都能命中指针。

## 8. 不在本任务范围

- 不实现 S-1…S-5 的任何一项。
- 不补 Linux/macOS 的真实执行器实测（本卡只登记"没跑"）。
- 不改样式令牌与页面布局。
- 不做观测层按类型查询（S-3）。
- 不把四份设计文档合并成一份。

## 9. 风险与回滚

| 风险 | 说明 | 退路 |
| --- | --- | --- |
| 文档指针写了但原文仍误导 | 使用者按旧文档操作 | §3.1 要求"原文加一句例外说明"而不是只改状态行 |
| 本机缺解释器导致"实测通过"名不副实 | 四类里有两类根本没跑 | §5 要求未验证项逐条列明，DoD 第 3 条不允许含糊 |
| `-race` 重复跑超时 | 偶发挂时被误判成回归 | 一律带 `-timeout 30m`（既有口径） |
| 残留 `server.exe` 锁住 `data/` | 冒烟起不来 | 冒烟前确认无残留进程（既有实测口径） |

回滚：本卡只动文档与验证记录，代码回滚按各卡 §9 的退路分别执行。

## 10. 实现记录（执行时补写）

本卡零生产代码改动，只改文档与留证据。输出文件都在 `.dev/n08/`（该目录自带 `.gitignore`，不入库）。

### 10.1 改了哪几份文档

| 文档 | 改动 |
| --- | --- |
| `docs/design/job-name-type-and-adhoc-execution.md` | 顶部状态行改"已实施"；新增 §2.1 落地位置（D1…D15 逐条给当前文件:行号）；§8 与 §10 里漂移的六处行号按现状改正（见 D-1005） |
| `docs/design/executor-design.md` | §2 的 D2 注记后面加一条 ⚠️：`executors.adhoc` 是"必须落在 workspace"与"http 档位必须有非空 `allowed_hosts`"两条的显式例外，并列出**没有一起放宽**的四条守卫，指向本文 §8 |
| `docs/design/web-profile-design.md` | D7 那一行末尾加 ⚠️：本条管的是档位定义层，内置自由执行档位是"每条任务各自给执行位置"，不是原地换执行体，指向本文 §8 |
| `docs/api.md` | §1 创建任务的两种写法（N02 已写）之外补：`GET /jobs` 的 `?type=`（已有）、`/job-types` 一句"打开整节时那四条内置键出现在这里"、§10 批量创建里"两种写法可在同一批次混用"的逐条结论（含实测那一批的 `succeeded:1/failed:3` 与四条原因）、自由执行档位一节补提交身份门槛与风险一句话 |
| `README.md` | 核心特性里加"名称是标签、类型决定跑什么"与"自由执行档位"两条；配置示例的 `executors` 段补 `adhoc.*` 七个键与默认值；"三种提交入口"那条写明目录加载器只支持旧写法 |
| `docs/deployment.md` | 热重载三档表的"重启档"一格里补 `executors.adhoc.*`（整节七项）；目录加载器一节写明"任务文件只支持旧写法"（S-2 要求的文档落地） |
| `web/src/content/job-template.md` | 修 N07 留下的编号撞车：新增那节是 §12.4，既有"跑完之后看什么"改成 §12.5（D-1001） |

### 10.2 全仓验证（§3.4，每条一个输出文件）

| 命令 | 结果 | 输出文件 |
| --- | --- | --- |
| `go build ./...` | exit 0，无输出 | `.dev/n08/01-go-build.txt` |
| `go vet ./...` | exit 0，无输出 | `.dev/n08/02-go-vet.txt` |
| `go test ./core -run TestExampleConfigMatchesLocal -v` / `-run TestEveryLeafKeyIsClassed -v` | 两条都 PASS（本机有 `configs/config.yaml`，所以是跑过而不是跳过） | `.dev/n08/03-targeted-tests.txt` |
| `go test ./... -count=1 -timeout 30m` | 五个有测试的包全 `ok`（api 22.2s / cmd/server 6.5s / core 20.5s / executor 24.1s / store/sqlite 1.7s） | `.dev/n08/04-go-test-all.txt` |
| 交叉构建 `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64`、`GOARCH=386`（各含 `go build ./...` 与只构建 `./cmd/server`） | 六条全部 exit 0 | `.dev/n08/05-cross-build.txt` |
| `go test -race -timeout 30m ./...` | exit 0；api 那一包命中构建缓存（同参数上一轮刚跑过），core 21.9s / executor 26.7s / cmd/server 8.3s / store/sqlite 5.6s | `.dev/n08/06-go-test-race-all.txt` |
| 缓存那条强制重跑：`go test -race -count=1 -timeout 30m ./api` | `ok godelayq/api 247.801s` | `.dev/n08/07-race-api-recount.txt` |
| `npx vue-tsc --noEmit`（exit 0）、`npm run build`、`go build -tags dashboard ./cmd/server` | 全部通过；`web/dist/index.html 01:28:28`、内嵌二进制 `01:28:30` | `.dev/n08/08-web-and-embedded.txt` |
| DoD 的三条 `grep`（`startsWith('exec`、`已注册的任务类型`、`任务名称（=`）在 `web/src` 下 | 三条零命中 | 同上文件 |

### 10.3 端到端十二条（§3.5，逐条一个文件，目录 `.dev/n08/e2e/`）

冒烟环境：`-tags dashboard` 内嵌形态、端口 8131、`executors.enabled: true` +
`executors.adhoc.enabled: true`、`required_role: admin`、`runtime_allow` 含 `sh`/`php`、
workspace 与数据目录在 `.dev/n08/smoke/`。凭据用 admin01 的 JWT 提交执行器任务
（**静态 machine token 的身份是 operator 档，`required_role: admin` 下提交 `exec.*` 会 403**，
这是既有设计而不是本系列的缺陷；场景 8 正好用这条差异）。

| # | 场景 | 实测结论 | 文件 |
| --- | --- | --- | --- |
| 1 | `{"name":"跑对账","type":"exec.php","payload":{"script":"…/scripts/reconcile.php"}}` | 201 → success；产物 `preview: "ran: reconcile\nargv1: none\n"`、`out_bytes:27`、`artifact:available` | `01-exec-php-success.txt` |
| 2 | `exec.shell` 同形态 + `exec.http` 打公网 | shell 201 → success（`shell-ran`）；`https://httpbin.org/post` 201 → success（`http_status:200`）；另加一条 `https://example.com/`（该地址不接受 POST）→ failed、`http_status:405`、`permanent:true`，说明非 2xx 判失败且不重试 | `02-exec-shell-and-http.txt` |
| 3 | `type` 缺失、`name` 是中文标签 | 400 `unknown job type`，`details: job type '每晚对账' not registered` | `03-legacy-name-is-chinese-label.txt` |
| 4 | `name` 带空格 / 下划线 / 点，`type` 正常 | 三条各 400 `invalid job name`，`details` 点名第一个非法字符与位置（三条位置都是 3） | `04-invalid-job-names.txt` |
| 5 | 只带 `name:"payment_check"` 的旧写法 | 201，响应里**没有** `type` 键，终态 success | `05-legacy-still-works.txt` |
| 6a | `{"url":"http://127.0.0.1:8132/probe"}` | 提交 201（名单留空=不限主机），执行期 failed：stderr `address refused by the profile's network policy: 127.0.0.1 (loopback address)`，事件 `job.failed` 同句、`permanent:true`；**本机监听端口收到 0 次请求**（没有外连） | `06a-loopback-refused-at-execution.txt` |
| 6b | 同一条 URL，配置改 `url_allow_private: true` | 201 → success、`http_status:200`，监听端口收到 1 次（真的建连了） | `06b-url-allow-private-true-passes.txt` |
| 7 | 路径的六种拒绝/放过 | 目录（取名 `adir.php` 绕开扩展名判据）400、文件不存在 400、扩展名不符 400、空值 400、含 `;` 400（`must not contain "';'"`）、不限目录时 workspace 之外的文件 **201**（对照，说明默认确实不限）、普通档位收到 `script` 键 400 且文案给出该档位实际接受的键（`allowed keys: args, env, timeout`）——七条理由互不相同 | `07-path-rejections.txt` |
| 7b | `path_prefixes: ["scripts"]` | 目录之外 400（`is outside the directories this server allows: …\ws\scripts`）、目录之内 201 | `07b-path-prefixes.txt` |
| 8 | operator 身份提交 `exec.php` | 403 `insufficient role`，`details` 点名 `executors.required_role` | `08-operator-403.txt` |
| 9 | `executors.adhoc.enabled: false` | `GET /executors` 只剩声明档位（`["exec.echo_sh"]`）、`GET /job-types` 里没有那四条键、提交 `exec.php` 400 `unknown job type` | `09-adhoc-disabled.txt` |
| 10 | `?type=payment_check` 与 `?name=每晚对账` | 类型筛命中 3 条（两条旧写法 + 一条新写法，证明比的是注册键）；名称筛只命中 1 条标签任务 | `10-filter-by-type-and-name.txt` |
| 11a | `executors.commands` 里放一条 `name: php`（与内置键撞名） | `GET /executors` 的 `exec.php` 那行是 `source:config`、`adhoc:false`、无 `location`；提交 `payload.script` 被 400（`not accepted by profile "php"`）⇒ **内置让位**，与 §D15 一致 | `11a-user-profile-wins-over-built-in.txt` |
| 11b | 删掉那条同名档位后重启 | 同一行变成 `source:adhoc`、`adhoc:true`、带完整 `location`；提交 201 → success | `11b-built-in-active-after-removing-collision.txt` |
| 12 | adhoc 任务在跑时强杀进程再启 | 杀之前 `status:running`；重启后 `status:paused`、`attempts:1`，事件 `job.paused` 带 `reason:"restore_after_crash"`、`forced:true` ⇒ 按 `restore_policy: pause` 钉住，没有消失也没有静默重跑 | `12-crash-recovery-pauses-running.txt` |
| 13 | 混合写法的一个批次 | 207，`succeeded:1 / failed:3`：新写法那条 201（`items[0]` 带 `type`）、旧写法但标签未注册 400 `unknown job type`、名称带空格 400 `invalid job name`、类型未注册 400 `unknown job type` | `13-batch-mixed-writings.txt` |

### 10.4 界面复测（跑在最终构建产物上）

`web/dist/index.html 00:39:26` 的那一份内嵌二进制（`00:39:28`）起服务，端口 8123，
N07 §5 的七条逐项重做一遍，结论与 N07 §10 的表一致：

| 项 | 复测读数 |
| --- | --- |
| ① 中文名称 + `exec.php` + 不存在的路径 | 400，表单顶部 `role=alert` 原文 `invalid executor payload：payload key "script": script file "nope.php" does not exist`，抽屉不关 |
| ② 改成存在的路径 | 201，列表出现 `收口复测 / exec.php`（表头含"类型"列） |
| ③ 切 `payment_check` | 位置栏消失，JSON 编辑器（textarea）出现，标签是"payload 合法 JSON…" |
| ④ 切普通档位 `exec.hello_sh` | 参数表单两栏（`day（必填） · 命令行参数` 文本框、`token · 命令行参数` 是 `type=password`），位置只读展示 `print_path.sh` + "位置来自档位定义，任务只给参数。" |
| ⑤ 两个筛子 | 类型 → 地址栏 `/jobs?type=exec.php`、请求 `?type=exec.php`；名称 → `/jobs?type=exec.php&name=收口复测`，表里只剩一条 |
| ⑥ 详情 | `名称 收口复测` / `任务类型 exec.php` / `脚本路径（.php） C:/Users/…/reconcile.php` |
| ⑦ 编辑模式 | 段落 `收口复测·exec.php（名称与类型不可修改，要换就新建一条）`，名称与类型没有任何可编辑控件 |
| 权限（operator） | 在 00:25 那一份产物上做的（第二、三组整体不出现，选中普通类型后给出"需要 admin 及以上"的说明）；00:25→00:39 两次构建之间**组件代码未变**，只改了 `job-template.md` 的编号，故未重做 |
| 未观测 | toast 与折叠动画（应用内标签页隐藏时被节流）；`cancelled` 行不出现在列表（取消即删记录） |

### 10.5 变异反向验证（§5，`go test -overlay`，真实工作树未改）

| # | 变异 | 结果 | 文件 |
| --- | --- | --- | --- |
| M1 | `api/handlers_executors.go:695` 的 `handlerKey := job.HandlerKey()` 打回 `job.Name` | **红**：`TestExecutorGateUsesHandlerKeyNotName` 两条子用例 + `TestAdhocGate_SubmissionEvidence` 三条子用例全红（身份不够 / 不可用 / 非法 payload 三条都变成 201） | `mutations/1-gate-by-name.txt` |
| M2 | `core/job_name.go:25` 正则去掉 `\p{Han}` | **红**：`TestValidateJobName/通过/纯中文`、`/三者混合` 等（"每晚对账"被判非法） | `mutations/2-name-rule-no-han.txt` |
| M3 | `executor/http.go` 的 `refusalReason` 开头短路成"一律放行" | **红**：`TestHTTPRunner_AdhocRefusesPrivateAddress/127.0.0.1`（期望拒绝却拿到 nil）与 `/169.254.169.254`（真的去拨号，报的是 `dial tcp …connectex…` 而不是策略拒绝） | `mutations/3-address-guard-off.txt` |
| M4（加测） | `executor/http.go:283` 的"有名单才判"短路成不判 | 卡面点名的那条 `TestHTTPRunner_AdhocHostGuardRunsAtExecution` **仍然绿**（它的拒绝来自执行期重跑的 `takeAdhocLocation`，走不到这一支）；整包跑同一条变异时 `TestHTTP_AllowedHostsMatch`（六个子用例）与 `TestHTTP_IPHostRequiresExplicitAllow` 变红 ⇒ 分支有覆盖，只是不在卡面预期的那条用例上。登记为 D-1007 | `mutations/3b-host-allowlist-off.txt`、`3b-summary.txt` |

### 10.6 设计文档 §10 十二条风险的现状核对

| # | 风险 | 现状 |
| --- | --- | --- |
| 1 | 只改 API 不改门禁 | 已落地：同一卡（N02）改了 `gateExecutorSubmission`；M1 变异证明用例有约束力 |
| 2 | 四个内置键与用户档位撞名 | 已落地：11a/11b 两条实测（内置让位、删掉同名条目后内置生效） |
| 3 | 名称规则套到旧客户端 | 已落地：场景 3/5 两条（旧写法不套规则、仍能建与执行） |
| 4 | 只带 `name` 时规则空转 | 登记不修（D4 的代价）：场景 5 就是这条的正面证据 |
| 5 | 探测对内置条目失去意义 | 已落地：`/executors` 的 `reason` 明说"这条检查说不出某个文件在不在"（11b 那一行原文），场景 1 与 7 各判一次文件 |
| 6 | SSRF | 部分缓解，按设计：6a 证明默认拒回环且不外连，6b 证明只有显式开关才放开；`url_hosts` 的收紧由 M4 那条分支覆盖（见 D-1007） |
| 7 | 任意路径执行 | 主动放宽：7b 证明 `path_prefixes` 能收窄；启动日志三条 warn 的原文在 N06 的记录里 |
| 8 | 普通档位收到新键的文案 | 已落地：场景 7 最后一条（`allowed keys: args, env, timeout`） |
| 9 | 事件表 `job_name` 是标签 | 登记不修（D13）：场景 12 的事件里 `job_name` 就是中文标签，身份靠 `job_id` |
| 10 | 编辑表单的名称框被误解 | 已落地：⑦ 复测（只读段落 + 原因） |
| 11 | 目录加载器仍按名称查键 | 未实施（S-2），文档已写明：`docs/deployment.md` 目录加载器一节与 README 的"三种提交入口" |
| 12 | 热重载分类漏登记 | 已落地：`TestEveryLeafKeyIsClassed` PASS（03 号文件），部署文档的三档表补了 `executors.adhoc.*` |

### 10.7 缺陷登记

| 编号 | 缺陷 | 处置 |
| --- | --- | --- |
| D-1001 | `web/src/content/job-template.md` 出现两个 §12.4（N07 新增那节与既有"跑完之后看什么"撞号） | 已修：后者改 §12.5；`grep "^### 12\."` 复核，页面上 12.1…12.5 各一处 |
| D-1002 | 编辑模式里旧写法任务把名称打印两遍（`payment_check·payment_check`） | 已修：类型为空时只给名称 + 旧写法说明（N07 §10 偏离 8） |
| D-1003 | 类型下拉的"普通任务"一组没有组名，与卡面 §3.2 的图示不符 | 已修：三组一律 `optgroup`，占位项留在分组外（N07 §10 偏离 1） |
| D-1004 | 位置输入框的举例对 `exec.shell` 说 `.php` | 已修：举例给两种后缀（N07 §10 偏离 3） |
| D-1005 | 设计文档 §8 与 §10 里六处 `文件:行号` 在实施后漂到别的函数上（`profile.go:993`、`:744`、`args.go:529`、`config.go:234`、`args.go:487`、`http.go:744`） | 已修：逐条改到当前行号并复核；§3 的"现状基线盘点"表**故意不改**（那是规划时的坐标，§2.1 才是落地后的） |
| D-1006 | `docs/deployment.md` 全文没有一处讲 `executors.adhoc` 的部署前提（运维最该看的文档缺章节） | 登记不修：本卡只补了热重载三档表里缺的那一项与加载器的写法说明；整节文档随 S-6 一起做 |
| D-1007 | `TestHTTPRunner_AdhocHostGuardRunsAtExecution` 对执行期 `checkTarget` 的主机名单分支没有约束力（短路它仍绿） | 登记不修：该分支由 `TestHTTP_AllowedHostsMatch` 与 `TestHTTP_IPHostRequiresExplicitAllow` 覆盖（M4 整包跑红即为证据）；adhoc 那条用例走的是执行期重跑 `takeAdhocLocation` 的路径，改它等于复制第二份判据 |

新增后续项：**S-6** 给 `docs/deployment.md` 补一节"自由执行档位的部署口径"
（与第 12/13 节同体例：开之前要有什么、`path_prefixes`/`url_hosts` 怎么收、日志看哪几条 warn）。

### 10.8 未验证项（逐条原因）

| 项 | 原因 |
| --- | --- |
| Linux / macOS 上的真实执行（脚本档位、进程树终止） | 本机是 Windows；交叉构建只证明能编译。既有系列同样未跑，见执行器系列记录 |
| `exec.python` 的真实执行 | 本机 `runtime_allow` 里有 `python`，但内置 `exec.python` 要求 `.py` 文件；本轮未造该脚本，只验证了它出现在登记表与类型下拉里（`runtime_ok:true`）。判据与 `exec.php`/`exec.shell` 同源（同一个 `checkAdhocScript`），但**没有**它的独立终态证据 |
| `url_hosts` 非空时的真实外连拒绝 | 单元层有（`TestHTTPRunner_AdhocHostGuardRunsAtExecution`），端到端本轮未跑（收紧名单要重启，而重启窗口已用于 6b/7b/11 三处） |
| `cancelled` 状态在列表里的呈现 | 取消即删记录（既有行为），列表里永远看不到那一行 |
| 观测层按类型查询 | S-3 未实施 |

### 10.9 与卡片的偏离

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
| 1 | §3.5 第 2 条要求"HTTP 打到本机一个公开端点或本机公网地址"，实测用了 `https://httpbin.org/post` | 内置 `exec.http` 的方法是 POST，`https://example.com/` 回 405（判失败，不重试）。405 那条也留在记录里，因为它正好证明"非 2xx 判失败且不重试" |
| 2 | §3.5 第 7 条要的四条路径拒绝，实测跑了七条（多加"不限目录时 workspace 之外=放行"、"空值"、"含 shell 元字符"，并把"指向目录"的取名改成 `adir.php`） | 原样写"指向 `ws/scripts` 目录"时先撞上扩展名判据（`has extension ""`），看不到"是不是普通文件"那一判；补的三条是为了让"理由互不相同"这条验收站得住 |
| 3 | §5 的第三条变异（"把执行期 `hostAllowed` 判据短路"）按字面做时卡面点名的用例仍绿 | 该用例的拒绝来自执行期重跑的 `takeAdhocLocation`，不经过 `checkTarget` 的名单分支。改成同时跑两条：地址范围守卫短路（M3，用例红）+ 名单短路整包跑（M4，另两条用例红），并把差异登记为 D-1007 |
| 4 | 端到端凭据用 admin01 的 JWT 而不是静态 machine token | 静态 token 的身份是 operator 档，`required_role: admin` 下提交 `exec.*` 一律 403（既有设计）。场景 8 反过来用这条差异取证 |
| 5 | 交叉构建的三条是 `linux/amd64`、`darwin/arm64`、`windows/386` | 卡面只写"GOOS=linux、GOOS=darwin、GOARCH=386 各一次"；`GOARCH=386` 必须配一个 GOOS，这里选 windows（本机平台 + 32 位，最能暴露平台相关文件的编译问题） |
| 6 | 冒烟环境的 `runtime_allow` 与解释器全部用本机现成的（`sh`、`php`），档位 `echo_sh` 只为"普通档位收到新键"那条对照而设 | 端到端要跑真进程，不能依赖未安装的解释器；对照条目是卡面 §3.5 第 7 条最后一条证据的最小实现 |
