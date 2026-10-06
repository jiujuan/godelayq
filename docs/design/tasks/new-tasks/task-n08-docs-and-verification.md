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

| # | 与卡片的偏离 | 原因 |
| --- | --- | --- |
