# TASK-E19　文档同步与全量验证

- 所属阶段：M5 收口
- 依赖任务：TASK-E01 … E18（全部完成）
- 涉及文件：`README.md`、`docs/api.md`、`docs/deployment.md`、`docs/example.md`、`docs/design/executor-design.md`、本目录 `README.md` 与各卡的完成状态、`examples/demo3`（可选）
- 预计规模：小（内容多但都是文字与验证）

## 1. 任务目标

把执行器写进面向使用者的三份文档，清掉前面各卡登记的"待补文档"事项，并在 Linux 与 Windows 上各跑一次全量验证，产出一份可交付的实现记录。

## 2. 背景与当前问题

`README.md` 现在描述的是"四个示例处理函数"级别的执行能力（核心特性 §3 只写"三种提交入口"与"上下文传递"），`docs/api.md` 完全没提执行器，`docs/deployment.md` 的安全章节也没有"开启执行器之后该怎么部署"。前面每张卡都在代码注释里登记了文档待办（例如 `metadata.permanent`、`stats.running` 的含义变化、优雅关闭时长建议、`loader_allow` 的真实作用范围），这些散落的待办必须在一处收口，否则实现完成后文档仍然是旧的，下一个读者会按旧文档理解系统。

## 3. 要实现的功能

### 3.1 `docs/api.md`

1. 新增"执行器"章节，内容：
   - 执行器键的命名规则（`exec.<档位名>`）与 `GET /api/v1/job-types` 的关系。
   - `GET /api/v1/executors` 的请求与响应示例（字段逐个说明，含 `runtime_ok`、`reason`、`args[].pattern`、`has_secret_args`、`required_role`）。
   - `GET /api/v1/jobs/:id/result` 的全部查询参数、默认值、四种失败返回（400/403/404/503）与响应示例。
   - `POST /jobs` 提交执行器任务的 payload 结构（三类各一份完整示例，含 Unix 与 Windows 差异说明）。
   - 事件 `job.completed` / `job.failed` 的 `data.result` 与 `metadata.permanent` 字段说明。
2. 修订既有条目：
   - `GET /api/v1/stats` 的 `running` 含义（两池之和）与新增的 `paused` 说明保持现状。
   - `GET /api/v1/admin/runtime` 响应示例补四个 exec 字段。
   - 权限矩阵表补一行"提交执行器任务：`executors.required_role`，默认 admin"，并引用 E16 第 5.1 条那张 15 组矩阵的结论。
   - `PUT /jobs/:id` 的约束（执行器任务禁止改名、改 payload 会重校验）。
   - `job.paused` 事件的 `metadata` 补两项来源说明（E14）：`reason: "restore_after_crash"` 表示
     崩溃恢复时把"上次正在执行"的档位任务停在 paused 上等人确认，此时 `forced: true`、并带 `attempts`；
     没有 `reason` 的暂停事件才是用户在控制台点的。同一节要写明一条现场事实：
     事件历史是进程内存里的，重启之后崩溃前那几轮 `job.started` 已经不在，
     时间线上只剩这条 `job.paused`——所以来源标记必须写在事件里，不能靠上下文推断。
3. 全文检查现有措辞：凡是"任务类型需提前注册 Handler"的地方（例如参数表里 `name` 的说明），补一句"执行器档位由 `executors.commands` 声明"。

   E15 落地后 `docs/api.md` 这一节要补的三条（写文档时按这些结论，别照卡片原文抄）：

   - `POST /jobs` 的 `http` 档位 payload 示例只接受四个键：`params`、`headers`、`body`、`timeout`，
     写成 `url`/`method`/`cmd` 之类一律按"提交内容非法"拒掉（E08 的那条未知键检查）。
     `headers` 只能覆盖档位 `header_allow` 里声明过的键，`Host`、`Content-Length`、`Transfer-Encoding`
     三个头由执行器决定，payload 里出现就是错误。
   - `exec` 摘要里 `http_status` 与 `exit_code` 互斥：http 档位只填前者，
     状态码永远不会写进 `exit_code`。`out_bytes` 记的是**落盘字节数**而不是响应全长，
     响应体超过 `max_body_bytes` 时 `truncated: true`；`capture_response: false` 的档位
     `out_bytes` 为 0、`preview` 为空，产物 `.err` 仍然写两侧头。
   - `/result` 的 `stream` 对 http 档位含义换了：`out` 是响应体，`err` 是请求与响应的头（不是 stderr）。
     默认读取方向对 http 应该取 `head`（响应体开头最有用），取值函数 `executor.PreferredResultDirection`
     已在 E15 落地，是否透出成 `/executors` 的字段由 E18 定；本卡按 E18 的最终形状写文档。

### 3.2 `docs/deployment.md`

新增"开启执行器"一节，逐条写清（每条都要有"为什么"）：

1. 前置条件：必须配置 `server.auth.token` 或 `server.auth.users`。程序在 `enabled=true` 且未启用鉴权时会记 error 级日志（E04），但那只是告警，不会阻止启动。
2. 运行账号：用最小权限的系统用户跑服务；`executors.workspace` 目录属主是该用户，权限 `0750`，**脚本内容等同于该用户的执行权限**，因此任何能改这个目录的人等于能配置命令。
3. 文件权限：`jobs.json` 与 `data/exec/` 里的产物都会包含任务参数与输出，可能含敏感值；按凭据文件的等级设权限（`0640` 起），并说明"参数掩码只作用于 HTTP 响应，不作用于磁盘"（E16 的结论）。
4. 时长关系：`scheduler.shutdown_timeout` 建议 ≥ 10 秒，因为取消执行器任务需要"终止 + 宽限 + 等待输出收尾"三段（E10 的结论），5 秒默认值下关闭会被拖到强制退出。
5. Windows 限制：`taskkill /T /F` 不是原子操作，极端情况下可能有派生进程残留（E11 第 3.3 条），彻底方案是 Job Object，尚未实现。
6. Windows 的输出编码：控制台程序写出的文本是系统本地代码页（中文 Windows 是 GBK），产物文件按原样字节保存，因此接口与 `meta.json` 里的尾部预览会把非 UTF-8 字节显示成替换字符（E11 第 10 节第 11 条）。要么在文档里给出"用 `chcp 65001` 或在脚本里重定向编码"的口径，要么按档位声明代码页转码——本卡只做文档，转码需求若要实现就开新任务卡。
7. Windows 的 `script` 档位限制：`cmd`、`pwsh` 这类解释器必须带 `/c`、`-File` 之类的开关才会执行文件，而 `script` 档位生成的命令行是 `[解释器, 脚本路径]`（E11 第 10 节第 9 条）。当前可用写法是 `kind: binary` + `fixed_args: [/c, 脚本名]`，文档要写明这条差异，并说明参数形状的改造归后续任务卡。
8. `deny_private_ranges: false` 只允许在开发机使用；关闭后 HTTP 档位可以访问回环与内网。
   E15 落地之后这一节可以照现场写：
   - 这种取值在启动阶段是合法的（E02 只拒绝它配通配主机名的写法），因此注册完成之后会有一条
     `level=WARN msg="executor http profiles accept private and loopback addresses" profiles=...`
     把关掉防线的档位名列出来。生产部署看到这一行就是要去改配置。
   - 被拒时的现场线索是一条同主题的 warn（含档位名、主机与被拒 IP，不含完整 URL），
     产物 `.err` 末尾一行 `! request failed: address refused by the profile's network policy: ...`，
     并且这类失败算永久失败、不消耗重试名额。
   - HTTP 档位的产物文件里 `a<attempt>.out` 是**对端返回的响应体**，`a<attempt>.err` 是两侧头
     （`Authorization`、`Cookie`、`Set-Cookie`、`X-Api-Key`、`Proxy-Authorization` 的值写成
     `<redacted>`）。因此上面第 3 条的文件权限建议对 HTTP 同样成立，而且响应体里可能带着
     回显出来的请求参数——`secret` 参数只在请求行与响应里打码，管不住对端把值写回正文。
9. `restore_policy: pause` 的运维含义：崩溃后会出现一批 `paused` 的执行器任务，需要人工确认后恢复（E14），并给出确认与批量恢复的操作步骤（`POST /jobs/batch-ops`）。
   E14 已在本机实测过一遍，文档照这几个结论写：
   - 判据是存储里的 `running` 快照。`kill -9`/`taskkill /F` 这类强杀会留下 `running`，
     正常停服（SIGTERM 走优雅关闭）不会——那条路径把被打断的任务落成 `pending`，重启后照常重跑。
     这一条差异是运维最容易误解的地方，要写在同一段里。
   - 重启后的现场：启动日志一条 `paused executor jobs after crash count=N`，
     `/api/v1/stats` 的 `paused` 计数包含它们，逐条有 `job.paused` 事件带 `reason=restore_after_crash`。
   - 确认动作：单条 `POST /api/v1/jobs/{id}/resume`（200，返回 pending），
     批量 `POST /api/v1/jobs/batch-ops` + `{"action":"resume","ids":[...]}`（207，逐条结果在 `items`/`errors`）。
     恢复后按既有的"触发时间已过就立即补跑"走，即确认一次就跑一次。
   - `restore_policy: replay` 的对照现象：同一条 `running` 快照重启后直接重新执行，
     日志只有 `restored jobs from store count=1`，不会出现上面那条 paused 汇总。
10. 保留策略：`executors.output.ttl` 与 `store.history_ttl`、`history_limit` 的配对建议（E06 第 9 条：`history_limit: -1` 时产物会在下次启动的孤儿清理中被删）。
11. 反向代理注意事项沿用现有章节，补一句：`/api/v1/jobs/:id/result` 的响应不缓存（`no-store`），不要把 `/api` 整体配成可缓存。

### 3.3 `README.md`

1. 核心特性 §3"灵活的任务定义"里增加一条：支持由配置声明的执行器（脚本 / 二进制 / HTTP），不需要改代码即可新增可执行任务；同时明确"默认关闭"。
2. 配置项一节补 `executors:` 的骨架（与 `configs/config.example.yaml` 一致，不重复全部注释）。
3. 目录结构一节补 `executor/`（列出该包内的文件与职责，格式照现有 `core/`、`api/` 的写法）。
4. 架构设计图：在 Scheduler Core 与 Persistence 之间加一个"执行层"框（`executor`：档位注册表 / 进程执行器 / HTTP 执行器 / 结果与产物），保持图内既有对齐风格；图下的"* 暂时没有实现"注记不要动（执行器实现后不应再出现在未实现清单里，但要确认 Redis/MySQL 那条注记仍准确）。
5. 适用场景表补一行"运维自动化：定时清理、报表脚本、部署钩子"。
6. "其它文档"里把执行器条目的"设计定稿，尚未实现"改成实现状态（与本卡同步修改）。

### 3.4 设计文档与本目录状态

1. `docs/design/executor-design.md` 顶部状态改为"已实现（M0–M5）"，并把与实现有偏差的段落就地标注（沿用 `web-console-design.md` 的 ⚠️ 体例）。至少要复查这几处：§6.4 产物清理策略（E06 采用"启动时孤儿清理 + 周期 TTL"）、§6.8 端点参数（E07 定稿）、§6.9 加载器（E17 的真实作用范围）、§2 D5/D6/D7 的落地位置。
2. 本目录 `README.md` 的状态表逐条改成"已完成 / 部分完成（说明缺什么）"。
3. 前面各卡在代码注释里登记的文档待办，逐条确认已在本卡完成，并删除代码里的"待补文档"注释（不留过期 TODO）。

### 3.5 示例（可选，若时间允许）

新增 `examples/demo3/main.go`：配置里声明一个 `script` 档位 + 一个 `http` 档位，用编程方式提交并打印结果摘要；或者把 `examples/demo2` 扩成"档位 + 加载器 `AllowExecJobs`"的演示。二选一即可，要求 `go vet ./...` 覆盖到。

## 4. 实现步骤

1. 先做全量验证（第 5 节），把失败项记下来——文档要写的是实际行为，不是计划行为。
2. 按 3.1 → 3.2 → 3.3 → 3.4 顺序改，每份文档一次提交。
3. 最后统一清理代码里的"待补文档"注释。

## 5. 测试与验证要求

本卡没有新增 Go 代码（除可选示例），但承担全项目的最终验证责任，要求逐项记录实际输出：

1. 全仓：`go build ./...`、`go vet ./...`、`go test ./... -race`。
2. 交叉编译：`GOOS=linux`、`GOOS=darwin`、`GOOS=windows` 各 `go build ./...` 一次（平台文件多的包最容易在这里漏编译分支）。
3. 配置守卫：`go test -run TestExampleConfigMatchesLocal ./core`；并确认本机 `configs/config.yaml`（不入库）与模板键集合一致。
4. 前端：`cd web && npx vue-tsc --noEmit && npm run build`；随后 `go build -tags dashboard -o godelayq-console ./cmd/server` 能成功（`web/dist` 已存在）。
5. 端到端手工场景（在 Linux 与 Windows 各跑一次，逐条记录结果）：
   - 打开 `enabled`，配一个输出到 stderr 并以 3 退出的脚本档位；提交 → 观察 400/403（参数越界与低权限）→ 正常提交 → 事件里读到 `exit_code:3` → `/result` 读到输出。
   - 60 秒脚本中途 `POST /jobs/:id/cancel` → 任务转 cancelled、无残留进程（`ps` / `tasklist` 断言）。
   - 超时档位（`timeout: 2s` + `sleep 30`）→ 约 2 秒失败、事件 `metadata.timeout==true`。
   - HTTP 档位指向 `http://127.0.0.1:8080/api/v1/health` → 被拒，服务端没有收到请求。
   - `kill -9` 服务端进程 → 重启后正在跑的执行器任务显示 `paused`，`GET /jobs/:id/events` 里有 `reason:"restore_after_crash"`。
   - 20 条执行器任务 + 20 条普通任务同时到期 → 普通任务并发仍受 `scheduler.workers` 控制且不被阻塞。
   - 产物 TTL 与孤儿清理：手工造过期目录 → 启动后按策略处理，日志可读。
6. 回归确认：`executors.enabled` 保持 false 时，用改动前的 README 快速上手流程跑一遍（编译、起服务、提交 `payment_check`、看 `/job-types`），确认默认部署路径没有任何变化。

## 6. 完成标准（DoD）

- [ ] `docs/api.md` 里两个新端点、事件新字段、权限矩阵新行齐备，示例可复制执行。
- [ ] `docs/deployment.md` 的"开启执行器"一节 9 条全部落地，每条含原因，不含"注意安全"这类空话。
- [ ] `README.md` 四处更新完成（特性、配置、目录结构、架构图 + 适用场景），且"执行器"不再出现在未实现清单里。
- [ ] 设计文档状态与偏差标注更新；本目录状态表逐条更新。
- [ ] 代码注释里没有遗留的"待补文档"TODO（`grep -rn "待补文档\|TODO(docs)" --include=*.go .` 为空）。
- [ ] 第 5 节六类验证在 Linux 与 Windows 上各跑过一次，结果写进本卡的"实现记录"小节（新增小节，逐条记 通过 / 未观测 / 失败已修）。
- [ ] 默认配置（`enabled=false`）下的用户体验与改动前一致，有第 5.6 条的回归确认。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
GOOS=linux go build ./... && GOOS=darwin go build ./... && GOOS=windows go build ./...
go test -run TestExampleConfigMatchesLocal ./core
grep -rn "待补文档\|TODO(docs)" --include=*.go . ; echo "exit=$?"   # 期望无输出
git diff --stat HEAD~6..HEAD                                        # 确认四份文档都在
```

文档自查：把 `docs/deployment.md` 新章节里的每条命令原样执行一遍，能跑通才算写完。

## 8. 不在本任务范围

- 不新增功能来弥补文档里发现的缺陷：发现问题就登记到设计文档 §11 或开新任务卡，本卡只写文档与验证。
- 不做英文版文档。
- 不生成 CHANGELOG（仓库没有这个惯例）。
- 不改 `docs/core-scheduler-heap-event-load-analysis.md`（它分析的是既有核心模块，与新执行层的耦合很低；如果实现改动了调度主循环，只在 E13 的实现记录里注明该文档需要跟进）。

## 9. 风险与回滚

- 风险：文档描述的是"计划的行为"，与实现有出入。缓解：第 4 节要求先验证后写文档，第 6 节要求每条命令实际执行过。
- 风险：架构图与目录结构容易只改一处（README 改了、设计文档没改）。要求两份文档在同一提交里改完，`git diff` 一次看全。
- 风险：`docs/core-scheduler-heap-event-load-analysis.md` 里关于调度循环的描述在 E13 之后会过期。本卡不重写它，但要显式记录这条已知过期项，别让下一个人误以为文档全量准确。
- 回滚：纯文档，可逐份 revert。
