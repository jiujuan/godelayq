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

   TASK-E18 落地后，`GET /api/v1/executors` 与 `JobResponse` 又多了几个键，控制台按它们生成表单和读输出，
   文档必须写全（结论取自 `api/handlers_executors.go` 与 `api/dto.go`，不要照本卡推测）：

   - 响应顶层：`max_timeout`（字符串 Go duration，只在 `enabled: true` 时出现，关闭时这个键整个省略）。
   - 每个档位：`preferred_result_direction`（`head` 或 `tail`，来自 `executor.PreferredResultDirection`，
     `http` 为 `head`、其余为 `tail`；它同时是 `GET /jobs/:id/result` 里 `from` 参数的取值写法）、
     `positional`（对象 `{max, pattern}`，档位没声明 `positional` 时这个键不出现）、
     `method`、`header_allow`、`body_mode`（后三项只出现在 `http` 档位上；配置里没写 `body` 的档位
     在响应里归一成 `body_mode: "none"`，不是空串）。
   - `JobResponse` 新增 `attempts`：与 `GET /jobs/:id/result` 允许的 `attempt` 范围同一口径
     （`1..attempts`，`attempts` 为 0 时只允许 1）。要同时写明现状：重试副本不带这个计数，
     重试过的任务仍然是 `attempts: 1`，也就是第 10 节遗留事项里那条 `CloneForRetry` 缺陷。

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

   E16 落地之后同一节要照这些现场写（别照卡片原文抄）：

   - 15 组档位矩阵（`executors.required_role` 取 `operator|admin|ops` × 身份取
     `viewer|operator|admin|ops|machine`）的结论在 `TestCreateJob_ExecutorRequiredRole` 与
     E16 卡第 10 节的冒烟 A 组。权限矩阵表照这三条写：viewer 连建任务都进不去（`POST /jobs`
     的路由本身要 operator）；`machine`（静态 token）与 operator 同档，所以默认配置
     （`admin`）下脚本凭据不能提交执行器任务；`ops` 高于 `admin`，始终可提交。
   - 执行器任务在提交期被拒的四条文案各不相同，每条给一个真实响应例子：
     `insufficient role`（403）、`executor profile is not available on this server`（400，
     details 是探测原因）、`invalid timeout`（400，请求顶层的 `timeout` 超档位上限）、
     `invalid executor payload`（400，payload 校验）。批量接口逐条独立：档位不够的那一条在
     `errors[].code` 里给 403，整批仍是 207。
   - `PUT /jobs/:id` 现在能出现 `name` 字段，但只允许传回任务原本的名字：不同值 400
     `job name cannot be changed`（这一条对所有任务生效，普通任务改成 `exec.` 前缀同样被拒）；
     改 payload 会重跑提交期那一整套判定，因此四种拒绝都可能在这里出现。
     判定顺序是"任务不存在 404 → 改了名字 400 → 档位不够 403 → 不是 pending 409"，
     前两条在状态检查之前判，所以档位不够的身份不会从 409 里读出这条任务跑完了没有。
   - `GET /executors` 的 `required_role` 在执行器关闭时是 `null`；`has_secret_args` 只看档位
     有没有声明 secret 参数，不看某一次提交有没有真的带值。
   - 含 secret 参数的档位被掩码的位置要一次写清，别只写"payload 会掩码"：任务对象的六处出口
     （创建、列表、详情、更新、重试、批量）里的 `payload` 与 `exec.preview`，以及 `/result`
     响应里的 `meta.preview`。`payload` 掩的是 `args`/`params`/`headers`/`env` 四个字段里
     命中 secret 参数名的取值，`body` 不掩（档位没有声明"body 里哪个键是凭据"的能力，
     要传凭据就走请求头：`secret` 参数 + `header_allow` 里的同名键）；`exec.preview` 是按值替换
     （`orders/***`），不是整段抹掉。磁盘上的 `jobs.json`、产物文件与 `meta.json` 都仍是原文。
   - `/result` 被档位挡住时 details 说的是档位的声明，不是任务内容；放行时响应多一个
     `redaction_note` 字段，取值是固定一句英文，文档里给出原文并说明它是提醒而不是"已防护"。

### 3.2 `docs/deployment.md`

新增"开启执行器"一节，逐条写清（每条都要有"为什么"）：

1. 前置条件：必须配置 `server.auth.token` 或 `server.auth.users`。程序在 `enabled=true` 且未启用鉴权时会记 error 级日志（E04），但那只是告警，不会阻止启动。
2. 运行账号：用最小权限的系统用户跑服务；`executors.workspace` 目录属主是该用户，权限 `0750`，**脚本内容等同于该用户的执行权限**，因此任何能改这个目录的人等于能配置命令。
3. 文件权限：`jobs.json` 与 `data/exec/` 里的产物都会包含任务参数与输出，可能含敏感值；按凭据文件的等级设权限（`0640` 起），并说明"参数掩码只作用于 HTTP 响应，不作用于磁盘"（E16 的结论）。
   E16 落地之后这一条可以照现场写：`jobs.json` 里的 `payload` 是原文，`exec.preview` 在新执行的任务上是
   按值掩码后的文本（见 E16 卡第 10 节"输出预览"一节），而改动之前落盘的旧文件里预览仍是原文——
   响应层会掩，磁盘上要换数据文件或重跑任务才会变；产物 `.out` 与 `meta.json` 按设计保持原文。
   同一节的权限说明还要带一条联动：`executors.required_role` 既是提交执行器任务所需的档位，
   也是读取含 secret 参数档位的产物正文所需的档位（E16 §3.3 第 2 条没有另设一个键）。
   把它降到 `operator` 就等于把"输出里可能回显出来的凭据"一起交给 operator——
   E16 冒烟 G 组的现场正是如此。文档要按这个联动写，让运维明白降档的代价不只是放开提交。
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
12. 目录任务加载器与执行器任务的边界（E17）。服务端二进制不启用目录加载器，所以这一条只影响
    自行把 `core.Scheduler` 和 `core.DirectoryLoader` 接起来的程序，文档要把作用范围先说清，
    别让运维以为改配置就能让服务端读目录：
    - 默认 `LoaderOptions.AllowExecJobs: false`：任务文件里 `name` 以 `exec.` 开头的会被拒绝。
      判断在 JSON 解析之后、绑定 Handler 与入队之前，因此被拒绝的文件既不会进调度器，
      也不会调用任何 Handler。
    - 被拒绝文件的去向：一条 warn 日志（含文件路径、任务名和固定原因原文
      `executor jobs are not accepted from the loader`）；配了 `LoaderOptions.ErrorDir` 时复制一份
      并在末尾追加一行原因；源文件仍按 `PostLoadAction` 处理。`KeepAfterLoad` 会把该文件记入
      已处理集合，所以不会每轮扫描重复拒绝同一个文件。
    - 开关的接法：`executors.loader_allow` 只由 `Registry.LoaderAllowed()` 读取，调用方把结果
      填给 `LoaderOptions.AllowExecJobs`；加载器本身不回读配置，服务端也没有这条链。
      未接线时该配置项没有任何行为差别，这一点要写明白。
    - 安全口径：能写监控目录不等于能执行命令。文档给出打开开关的前提（监控目录的写权限
      收窄到服务账号独占），并说明打开后放弃的是哪一层防护。

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
   `api` 包的 `TestAdminRuntime_ExposesExecPool`（E13 的用例）在 TASK-E17 收尾时三次失败，
   已修：断言跑得太早，没等第二条档位任务进执行器队列，改成轮询到位再断言；
   原因、修法与修后的跑动记录见 TASK-E17 卡第 10 节"未验证与遗留"最后一条。
   E19 全仓验证加一项 `go test ./api -race -count=50 -run TestAdminRuntime`，确认这条不再偶发
   （TASK-E17 收尾时该命令已跑过一轮并全绿，见该卡第 10 节"补充"第 2 条）。
2. 交叉编译：`GOOS=linux`、`GOOS=darwin`、`GOOS=windows` 各 `go build ./...` 一次（平台文件多的包最容易在这里漏编译分支）。
3. 配置守卫：`go test -run TestExampleConfigMatchesLocal ./core`；并确认本机 `configs/config.yaml`（不入库）与模板键集合一致。
4. 前端：`cd web && npx vue-tsc --noEmit && npm run build`；随后 `go build -tags dashboard -o godelayq-console ./cmd/server` 能成功（`web/dist` 已存在）。
   TASK-E18 那张手工走查清单（本目录 `task-e18-console-frontend.md` 第 5.2 节九条）只在 Windows 内嵌形态跑过一轮，
   结果与未观测项记在该卡第 10 节。E19 在 Linux 上复走其中三条就够：档位可用与不可用的显示差异、
   详情页输出的头尾读取与"加载更多"、`secret` 参数在详情页与输出预览里的掩码。
   Windows 那轮没覆盖到的两项（`cancelled` 状态在列表里不出现、macOS 整树终止）不在前端范围内，按第 5 条与第 10 节处理。
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

- [x] `docs/api.md` 里两个新端点、事件新字段、权限矩阵新行齐备，示例可复制执行。
- [x] `docs/deployment.md` 的"开启执行器"一节 9 条全部落地，每条含原因，不含"注意安全"这类空话。（实际写了 12 条，见第 10 节"偏离"第 2 条）
- [x] `README.md` 四处更新完成（特性、配置、目录结构、架构图 + 适用场景），且"执行器"不再出现在未实现清单里。
- [x] 设计文档状态与偏差标注更新；本目录状态表逐条更新。
- [x] 代码注释里没有遗留的"待补文档"TODO（`grep -rn "待补文档\|TODO(docs)" --include=*.go .` 为空）。
- [~] 第 5 节六类验证在 Linux 与 Windows 上各跑过一次，结果写进本卡的"实现记录"小节（新增小节，逐条记 通过 / 未观测 / 失败已修）。
  **Windows 六类全跑；Linux 侧只有交叉编译**（本机 WSL2 没有 gcc，跑不了 `-race`；也没有可用的 Linux 图形浏览器），见第 10 节"未验证与遗留"。
- [x] 默认配置（`enabled=false`）下的用户体验与改动前一致，有第 5.6 条的回归确认。


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

## 10. 实现记录（2026-09-30）

落地文件（全部是文档与代码注释，没有新增 Go 代码）：

- `docs/api.md`：新增 `## 执行器 API` 一节（八个子节：与 `/job-types` 的关系、档位清单、提交执行器任务、
  提交期被拒的四种响应、读取执行输出、任务对象里的 `exec` 与 `attempts`、事件里的执行结论、
  敏感参数出现在哪些地方），并修订权限矩阵表、`POST /jobs` 参数表、`PUT /jobs/:id`、
  `GET /admin/runtime`、`GET /stats`、事件端点与 `/job-types` 的措辞。全文 922 → 1365 行。
- `docs/deployment.md`：新增 `## 开启执行器` 一节，12 个编号小节，每条都写"为什么"。
  另在 `scheduler.shutdown_timeout` 的行内注释与加载器小节各加一处指向本节的链接。全文 257 → 435 行。
- `README.md`：适用场景、核心特性 §3、配置项骨架、目录结构（`executor/` 15 个文件与 `api/handlers_executors.go`）、
  架构图新增执行层框、"其它文档"里执行器条目的状态。
- `docs/example.md`：目录加载器示例补 `AllowExecJobs: false` 一行与一段拒绝现场。
- `docs/design/executor-design.md`：状态改"已实现（M0–M5）"，⚠️ 标注 §2 的 D5/D6/D7/D9 落地位置、
  §5.1 第 3/4 条、§6.2 的平台文件名、§6.4 的四条（搬运、字段、产物布局、清扫）、§6.7 的取值规则、
  §6.8 的端点参数与事件形状、§6.9 的被拒文件去向、§9 的 M1/M2/M5、§10 逐条勾选、§11 新增第 7/8/9 条
  ——全文 26 个 ⚠️ 标记，覆盖卡片点名的四处之外的七处。
- `docs/design/tasks/executor/README.md`：状态表 E19 行与 M5 段落。
- 四条代码注释收口（`core/scheduler.go` 的失败事件 metadata、`api/handlers.go` 的 `stats.Running`、
  `executor/proc.go` 的 `processWaitDelay`、`api/executors_submission_test.go` 的矩阵用例说明）。

### 与卡片的偏离与补充

1. **§3.2 要求 9 条，实际写了 12 条**：多出的三条是 E15/E06/E17 在各自卡片里追加给本卡的口径
   （`deny_private_ranges: false` 的现场线索、TTL 与留痕的配对、目录加载器的作用范围）。
   §3.2 正文第 8/9/10/12 条本身就要求这些内容，所以是"9 条编号"过时而不是内容超范围。
2. **§3.3 说"四处更新"，实际六处**：卡片列的四条（特性/配置/目录结构/架构图+适用场景）加上"其它文档"状态行，
   加上架构图下面那条"Redis/MySQL 尚未实现"的注记复查（确认它仍然准确，未改）。
3. **§3.4.3 的"删除代码里的'待补文档'注释"这条，DoD 的那条 grep 从一开始就是空的**：
   前面各卡登记待办时用的措辞是"归 TASK-E19 统一补文档""登记在 E19 的文档收口清单里"，
   不含 `待补文档` 这个连续四字，也不含 `TODO(docs)`。本卡把四条 E19 署名的注释全部改掉，
   并加了一条更严的检查：`grep -rn "E19" --include=*.go .` 现在也为空。
4. **§3.4.1 点名四处，实际标注了十处**：复查时新发现的偏差比点名多
   （§5.1 的超时夹取与位置参数写法、§6.7 的三条取值规则、
   §9 里程碑表的 M1/M2/M5、§10 验收清单的逐条状态、§11 的三条后续项，加上点名之外的
   §6.4 拆成四条、§6.8 拆成端点形状与事件形状两段）。§6.2 标题里的平台文件名写错（`proc_win.go`），
   这一处直接改成实际的 `proc_windows.go`，没有加标注。
   点名之外的部分照同样体例标了，没有另开一节。
5. **§3.5 的 `examples/demo3` 未做**。理由：DoD 不要求它；`examples/demo2` 已经在 TASK-E17 的冒烟里
   承担了"加载器 + `AllowExecJobs`"的演示（真实跑过、拒绝现场写在该卡第 10 节）；
   再加一份示例要么重复 `demo1` 的编程式提交，要么把档位脚本放进需要独立 workspace 目录的示例里
   （示例目录与 `executors.workspace` 是两套路径，读者容易把两者当成一个）。
   如果要做，建议开一张新卡并先定示例要落在哪条路径。
6. **§5.5 的"HTTP 档位指向 `http://127.0.0.1:8080/api/v1/health`"改成两个本机对端**：
   `exec.local_health` 打自己的 `/api/v1/health`（同端口 18090）、`exec.local_target` 打一个自行
   起在 `127.0.0.1:18091` 的裸 TCP 监听器。第二条是"服务端没有收到请求"这条判据的正面证据：
   监听器日志只有 `listening on 127.0.0.1:18091` 一行，没有 accept——
   之前想用应用日志证明"没收到"是站不住的，那轮构建没开请求日志。
7. **§5.4 的"在 Linux 复走三条"没做**：本机没有可用的 Linux 图形浏览器，WSL2 里连 gcc 都没有
   （`-race` 跑不起来，见本目录共同的验证口径）。这条与 §5.1/§5.5 的 Linux 侧一起落到"未验证与遗留"。
8. **§7 验收里的 `git diff --stat HEAD~6..HEAD` 换成按文件核对**：本卡的提交数是 8（四份使用者文档拆了 5 次，
   代码注释 1 次，设计文档 1 次，卡片与状态表 1 次），用固定回看步数会漏掉前面的文档。

### 验证（§5 六类逐条）

| # | 命令 | 结果 |
| --- | --- | --- |
| 5.1 | `go build ./...`、`go vet ./...` | 通过，无输出 |
| 5.1 | `go test ./... -race -count=1` | 全绿：`api` 103.09s、`cmd/server` 5.52s、`core` 12.34s、`executor` 23.26s，其余包 no test files |
| 5.1 | `go test ./api -race -count=50 -run TestAdminRuntime` | 通过（1.71s，50 轮无失败），TASK-E17 登记的偶发已消失 |
| 5.2 | `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64`、`GOOS=windows GOARCH=amd64` 各 `go build ./...` | 三个都通过 |
| 5.3 | `go test -run TestExampleConfigMatchesLocal ./core` | 通过；本机 `configs/config.yaml`（不入库）与模板键集合一致 |
| 5.4 | `cd web && npx vue-tsc --noEmit` | 通过，无输出 |
| 5.4 | `npm run build` | 通过，`dist` 产物最大块 `index-*.js` 156.25 kB（gzip 57.22 kB）、模板页 118.19 kB |
| 5.4 | `go build -tags dashboard ./cmd/server` 与 `go vet -tags dashboard ./...` | 都通过（`web/dist` 存在，嵌入生效） |
| 5.4 | Linux 侧复走 TASK-E18 三条走查 | **未做**（见"偏离"第 7 条） |
| 5.5 | 七个端到端场景 | Windows 全部跑过，见下表；Linux 侧**未做** |
| 5.6 | 默认配置回归 | 通过，见下表最后一行 |
| DoD | `grep -rn "待补文档\|TODO(docs)" --include=*.go .` | 无输出（`grep -rn "E19" --include=*.go .` 同样无输出） |
| DoD | `gofmt -l`（对照用） | 仍因 CRLF 全量误报，按本目录口径不采信；本轮只改注释与文档，未做格式化 |

### 端到端场景（§5.5，Windows 真实进程）

冒烟环境：`%TEMP%/gd-e19smoke/`（配置、store、`data/exec`、workspace 全在临时目录，仓库的 `data/` 没被写过），
服务监听 `:18090`，`executors` 取 `runtime_allow: [bash]`、`concurrency: 2`、`scheduler.workers: 2`、
`required_role: admin`、`restore_policy: pause`、`output.ttl: 1m`，档位 7 条
（`fail3`/`long60`/`nap`/`show_token`/`not_deployed` + 两条 http）。账号四种身份齐（admin/operator/viewer/ops）。

| 场景 | 结果与证据 |
| --- | --- |
| 打开 `enabled` + stderr 退 3 的脚本档位：参数越界与低权限 → 正常提交 → 事件 `exit_code:3` → `/result` 读到输出 | **通过**。四条拒绝文案各一条实拍（403 `insufficient role`、400 `invalid executor payload` 两条不同 details、400 `executor profile is not available on this server`、400 `invalid timeout`），响应原文全部写进 `docs/api.md`；`exec.fail3` 跑完 `exit_code:3`、`out_bytes`/`err_bytes` 与两条 stderr 行在 `/result?stream=err` 里读到 |
| 60 秒脚本中途 `POST /jobs/:id/cancel` → cancelled、无残留进程 | **部分通过**。取消返回 204、任务转 cancelled；取消前 `/admin/runtime` 读到的占用是 `exec_running:1`（取消之后的那个采样点本轮没再取一次，池空是靠"任务不在列表里 + 没有第三条档位任务在跑"推出来的，不是实测数字）。残留断言用 `tasklist` 的镜像名计数（msys 的 `$$` 不是 Windows PID，这条口径记在下面"补充观测"）：`bash.exe` 立刻回到基线，**一条 `sleep.exe` 活过了它自己的 60 秒** → 记为缺陷，见设计文档 §11 第 2 条与 `docs/deployment.md` 第 5 小节 |
| 超时档位（payload 写 `timeout: "2s"`、脚本 `sleep 3`）→ 事件 `metadata.timeout==true` | **通过**。日志 `msg="job timed out" ... timeout=2s error="profile \"nap\": timed out: exit status 1"`，`duration_ms=3051`（2 秒判定 + 终止与管道收尾，与 `killGrace`/`processWaitDelay` 的口径一致）；`metadata.timeout` 走的是同一条 `timedOut` 分支，因此为 true（这一条由代码路径与上面那条日志共同支撑，事件正文本轮没再抓一次） |
| HTTP 档位指向回环地址 → 被拒，且服务端没收到请求 | **通过**。warn `executor http target address refused profile=local_target host=127.0.0.1 address=127.0.0.1 reason="loopback address"` + 同主题的 `job failed`，18091 的监听器没有 accept（见"偏离"第 6 条）；这类失败 `permanent` 为真、不消耗重试 |
| `kill -9`（Windows 用 `taskkill /F`）→ 重启后正在跑的任务 `paused`，事件带 `reason:"restore_after_crash"` | **通过**。重启日志 `paused executor jobs after crash count=1 reason=restore_after_crash`，`GET /jobs/:id/events` 里那条 paused 带 `reason` 与 `forced:true`、`attempts`；`/api/v1/stats` 的 `paused` 计数含它。对照的 `replay` 现象沿用 TASK-E14 那一轮 |
| 20 条执行器 + 20 条普通任务同时到期 → 普通任务不被阻塞 | **通过**。轮询采样 12 次都是两池同时忙（`exec_running=2`、普通池 2），`stats.running` 全程是 4（两池之和），40 条最终全部 success；`concurrency: 2` 没有挤掉 `workers: 2` |
| 产物 TTL 与孤儿清理 | **通过**。手工造 3 个"ID 已不存在"的目录与 24 个过期目录，重启日志两行：`artifact orphan directories purged count=3`、`artifact expired directories purged count=24 ttl=1m0s`；再启一轮（第三实例）时孤儿/过期计数各 2，说明上一轮的产物按策略继续收敛 |
| 批量接口的混合权限 | **通过**。一条普通 + 一条 `exec.*` 用 operator 提交：207、`succeeded:1`、`errors[0].code=403`，响应原文进 `docs/api.md` |
| `PUT /jobs/:id` 的四条约束 | **通过**。改 `name` 为不同值 → 400 `job name cannot be changed`（普通任务同样生效）；档位不够的身份改执行器任务 → 403 而不是 409（判定顺序 404→400→403→409）；payload 改坏 → 400 且详情不落库 |
| `GET /executors` 的形状 | **通过**。启用时给 `enabled:true`、`required_role:"admin"`、`max_timeout:"5m0s"` 与七条档位（含 `positional`、`preferred_result_direction`、http 的 `method`/`header_allow`/`body_mode`），逐字段与 `docs/api.md` 的示例对得上；未启用那一轮（5.6）这个端点返回 200、`enabled:false`（实测到 HTTP 状态，响应体的 `required_role:null` 与"没有 `max_timeout` 键"由代码分支与 `TestListExecutors_DefaultStateIsNotAnError` 保证，本轮没再逐字段回看） |
| 含 secret 参数的档位掩码六处出口 + `/result` 的 `redaction_note` | **通过**。`exec.show_token` 提交后，创建/列表/详情/更新/重试/批量六处的 `payload` 与 `exec.preview` 都看不到本机那个取值，`args.token` 是 `***`；operator 读该任务产物正文 403（warn 原文 `access denied who=oper01 have=operator need="execution output of a profile with secret arguments" required=admin`）、admin 读到正文并带 `redaction_note`（原文在 `docs/api.md`）；磁盘上的 `jobs.json` 与产物文件按设计仍是原文（文档写明） |
| `/result` 的参数与不存在路径 | **通过**。同一任务的 `?stream=err&from=tail` 与 `?stream=out` 均 200；`?attempt=9`、`?max_bytes=0`、`?stream=nope` 三条各自 400；一个不存在的 ID 是 404。四条 400/404 的响应原文都按实测写进 `docs/api.md`；未装配产物存储时的 503 只有后端用例覆盖（`api/handlers_executors_test.go`），本轮没法用真实进程造出来 |
| 5.6 默认配置回归（`executors` 不打开） | **通过**。用 `configs/config.example.yaml` 原样起服务（`:8080`，无鉴权）：`/health`、`/job-types`、`/executors`、`POST /jobs` 提交 `payment_check`、任务按 delay 执行、`/jobs/:id`、`/stats` 逐条 200/201，行为与改动前的 README 快速上手一致 |

补充观测（不在 §5.5 清单里，但影响读文档的人）：

1. msys 的 `$$` 不是 Windows 进程号，`subprocess.run(['tasklist', ...])` 从 python 里拿不到输出，
   所以残留断言只能按镜像名计数。这条口径已经写进 TASK-E19 的脚本注释，后续任何 Windows 进程断言都要照做。
2. `POST /jobs/:id/cancel` 成功时是 **204 无响应体**，第一版验证脚本按"总是 JSON"解析直接崩了。
   `docs/api.md` 的取消一节早已写明 204，是脚本没照文档写。
3. 被取消的任务在列表与详情里都读不到（`GET /jobs/:id` 返回 404，`total` 也不含它），
   与 §10 验收清单无关，但和"取消之后去读输出"这条使用路径直接冲突，已写进下面"未验证与遗留"。
4. `stats` 的 `completed` 在这一轮是 41（40 条本轮任务 + 上一轮的一条终态留痕），
   说明留痕计数与本轮跑动是同一份存储，冒烟目录复用时要按这个理解读数字。

### 未验证与遗留

| 项 | 状态与处置 |
| --- | --- |
| Linux 与 macOS 的真实运行、那一侧的 `-race`、§5.4 的三条走查复走 | **未做**（WSL2 无 gcc、无可用 Linux 图形浏览器）。登记不修，等有真实环境时按本卡 §5.4/§5.5 复跑；这是执行器系列收口时唯一成块的外部依赖 |
| Windows 整树终止的残留（实测一条 `sleep.exe` 存活） | 登记不修（本卡不改代码）。已写进设计文档 §11 第 2 条与 `docs/deployment.md` 第 5 小节；彻底方案 Job Object 仍是 E11a |
| `CloneForRetry` 不搬 `Attempts`，重试产物覆盖、`?attempt=2` 读不到 | 登记不修（要动 `core`）。已写进设计文档 §6.4/§10/§11 第 7 条与 `docs/api.md` 的 `attempts` 说明；E15 末尾与 E18 第 10 节各有一条现场 |
| `cmd`/`pwsh` 在 `script` 档位下拿不到执行开关 | 登记不修。绕法（`kind: binary` + `fixed_args`）已写进 `docs/deployment.md` 第 7 小节与模板页；改造归后续任务卡（设计文档 §11 第 8 条） |
| Windows 控制台输出是 GBK，预览里出现替换字符 | 登记不修（本卡只写文档）。口径在 `docs/deployment.md` 第 6 小节，设计文档 §11 第 9 条留了转码这条后续 |
| 被取消的执行器任务读不到任务与输出（`GET /jobs/:id` 404） | 登记不修，属既有语义（取消即从存储删除，见 `core/scheduler.go` 的 `Cancel`）。作为已知使用冲突记录在本卡"补充观测"第 3 条，若要改需另立任务 |
| 档位超时在事件里记成 `timeout=0s`（E15 末尾登记的那条既有现象） | 本轮没有复现：这一轮 `job.timed out` 日志与事件里的 `metadata.timeout` 都是生效值。原因是 E16 把生效超时写进了 `job.Timeout`。登记不修，等 core 侧统一收口时再判 |
| `docs/core-scheduler-heap-event-load-analysis.md` 的调度循环描述在 E13 之后过期 | 本卡按 §8 不改它，在这里显式记录这条已知过期项（§9 要求的那条）：`dispatch` 现在是双队列两池、`executeJob` 多了类别与 `Exec` 摘要两处 |
| 冒烟临时目录 `%TEMP%/gd-e19smoke`（含本机 dev 凭据） | 本卡收尾时删除，不入库；仓库的 `configs/config.yaml` 与 `data/` 全程未被写入 |

