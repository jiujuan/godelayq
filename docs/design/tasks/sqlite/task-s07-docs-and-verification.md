# TASK-S07　文档同步与全仓验证

- 所属阶段：M4 收口
- 依赖任务：TASK-S01 … S06 全部完成
- 涉及文件：改 `README.md`、`docs/api.md`、`docs/deployment.md`、`docs/design/sqlite-observability-design.md`、`docs/design/web-console-design.md`、`docs/design/tasks/sqlite/README.md`
- 预计规模：小（但覆盖面广，逐条核对不能跳）

## 1. 任务目标

让四份使用者文档与设计文档描述的是**实际实现的形状**，而不是设计阶段的设想；跑完全部验证并把结果逐条记进本卡第 10 节。

## 2. 背景与当前问题

前面六张卡片每完成一张，实现都可能与卡片文字有偏差（S02 的泛型选择、S04 的 `Note` 句子、S05 的 `IndexRecord` 是否走通、S06 的 `logAccessRejection` 能否拿到 `c`，都是设计时无法完全预定的点）。文档收口这一张卡的作用就是把这些偏差一次性对齐，并且把"设计时以为不重要、实测才发现"的口径写进正式文档。

另外有三处**已经存在的文档承诺**在本系列之后变成假话，必须逐条改掉，不能只加新内容：

1. `docs/design/web-console-design.md:607`：`# 写操作审计本期只输出结构化日志（§5.7.6）；/admin/audit 查询端点留二期` —— S06 已经实现。
2. 同文件 §5.7.6（`:793`）："落现有 logger，不引入新存储。持久化后可查询属二期" —— 引入了新存储。
3. 同文件 §5.6（`:646`、末尾"持久化审计不在本期范围"）与 `:33` 的缺陷清单条目"运行历史的持久化审计（重启即清空…）" —— S03/S04 已经解决。

`api/history.go:23-25` 的代码注释也在说同样的话（"需要持久化的运行历史属于二期"）。它是否要改，判断依据是"这句话对**未启用观测层的部署**是否仍然成立"——成立，因此代码注释保留、补一句"启用 `observability` 后另有持久化事件库，见 `docs/design/sqlite-observability-design.md`"。这类"注释要不要改"的逐条判断是本卡的主要工作。

## 3. 要实现的功能（逐文件清单）

### 3.1 `README.md`

| 位置 | 改动 |
| --- | --- |
| 架构设计图（`:26`） | 在持久化层右侧或下方补一块"观测层（可选，`observability.enabled`）：SQLite 三张表"，标注它是加速器、不影响任务快照 |
| 数据流图（`:93`） | EventBus 的下游补一条"→ 事件库（可选）" |
| 核心文件详解表（`:108`） | 新增一行 `store/sqlite/`，说明它承担什么、为什么驱动只在这里 |
| 目录结构（`:124`） | 加 `store/sqlite/` 与 `docs/design/sqlite-observability-design.md`、`docs/design/tasks/sqlite/` |
| 核心特性（`:232`） | 在"实时可观测性"（`:260`）一节里加一条：事件时间线可跨重启留存、输出文件有索引、写操作有台账 |
| 配置项（`:379`） | 新增 `observability` 一节的取值表（照 `executors` 一节的详略程度，不必逐项抄注释，但默认值与"关掉即惰性"要写） |
| 其它文档（`:453`） | 加设计文档链接 |
| 适用场景（`:14`） | 若有"运维自动化"一条已提到执行器，考虑补一句"执行记录可查询"，不要重复展开 |

### 3.2 `docs/api.md`

| 位置 | 改动 |
| --- | --- |
| `## 运行事件 API`（`:991`） | 两个端点各补：`note` 的两个取值及其分岔依据（是否装配事件库）、装配库时 `limit` 上界为 1000、`timestamp` 精度到微秒（纳秒在持久化路径上丢失）、最新一条可能有 `flush_interval` 的写入延迟、读库失败返回 500 而不静默退回内存 |
| `## 运维 API（ops）`（`:1023`） | 新增 `GET /api/v1/admin/audit`：全部查询参数、响应字段、`verdict`/`exec_verdict` 的封闭取值、未装配时 503 |
| `## 执行器 API`（`:509`） | 新增 `GET /api/v1/jobs/:id/artifacts`：字段、排序、四种状态码（401/200 空/200/503）；并在结果端点一节补一句"索引自本版本起，更早的产物不在列表里但仍可读取" |
| `## 鉴权与跨域`（`:14`） | 权限矩阵补两个新端点的档位（`artifacts`=viewer、`admin/audit`=ops） |
| 全文 | 事件端点原来写的"内存缓冲，重启即清空"这类说明，凡涉及响应形状的都要改成"未启用观测层时…"的条件句 |

### 3.3 `docs/deployment.md`

新增一节 `## 启用观测层（可选）`，放在"开启执行器"（`:167`）之后，内容至少包含：

1. 打开方式与三个子开关（`events` / `artifacts` / `audit`）各自的含义；默认关闭。
2. 依赖：`modernc.org/sqlite` 纯 Go，无需 C 工具链；二进制体积约 +8~10MB（写出本卡实测的数值）。
3. 文件与备份：`observability.path` 指向哪、WAL 模式会产生 `-wal`/`-shm` 旁文件；**运行中备份用 `VACUUM INTO`，不要直接拷单个 `.sqlite` 文件**；停服后备份整目录即可。
4. 权限：库文件 `0640`、目录 `0750`；与 `data/jobs.json` 同级时同一套备份与属主策略。
5. 持久化强度：`synchronous: normal` 断电最多丢最后若干次已提交事务（与 `store.flush_interval` 同量级）；需要更强保证改 `full` 并接受写入变慢。
6. 容量与保留：`write_audit` 随写请求量线性增长、`job_events` 随任务执行次数增长；CI 高频部署应先调 `retention_count`；给出"每天约 10 万次执行"的量级估算（用实测行数，不要抄本卡的默认值当结论）。
7. 观测：`/api/v1/admin/runtime` 里的 `dropped` 计数含义；**丢了记录不会有任何告警**，需要主动看这个数。
8. 关掉即回到原状：把 `enabled` 改回 `false`，表与既有行留着不影响任何东西。
9. 与 `store.history_limit` 的关系：三者保留的是不同东西（快照留痕 / 事件留存 / 台账留存），不要指望它们同步；引用设计文档 D9。
10. 若 Systemd 一节（`:344`）用了 `ReadWritePaths` 或 `ProtectSystem`，把 `observability.path` 加进可写路径清单。

### 3.4 `docs/design/sqlite-observability-design.md`

按实际实现改，并把偏离处标注出来：状态行改为"已实现"；§6 的 DDL 与代码里的 `schema.go` 逐字对齐；§9.5 的 `action` 映射表与实际映射表对齐；§5 的架构图若装配点形状变了要跟着改（例如 `newServer` 的签名最终传了哪几个依赖）。**每条与设计不同处加一行说明"实现如此、设计原本如何、为什么这样选"**，不要静默改写。

### 3.5 `docs/design/web-console-design.md`

按 §2 列的三处既有承诺逐条改为"已实现（TASK-S03/S04/S06）"并给出指向。`:33` 的缺陷清单条目同理——它列的是"重启即清空"，现在是有条件的。**这一份文档只改与观测层相关的句子，不动其它内容**（它同时是控制台前端的现行规格）。

### 3.6 本系列 `README.md`

状态表逐条改为已完成 + 完成日期；补一段"阶段进度"总结，说明哪些实测项受平台限制没跑（例如 Unix 侧权限断言，参照 TASK-E06 第 10 节的既有处理方式）。

## 4. 实现步骤

1. 先跑全套验证（§5），把结果与失败项抄进本卡第 10 节——**先验证再改文档**，否则文档写的是设想。
2. 改 `docs/api.md`（使用者最需要准确的一份）。
3. 改 `docs/deployment.md` 新增一节。
4. 改设计文档（对齐实现 + 标偏离）。
5. 改 `web-console-design.md` 的三处承诺。
6. 改 `README.md` 的七处。
7. 最后改本系列 README 的状态表。

## 5. 测试要求（本卡是"验证"，不是"新增测试"）

全部命令在仓库根目录跑，逐条记录实际输出（通过/失败、耗时、以及和上一卡相关的行）：

```bash
go build ./...
go vet ./...
go test ./... -race -count=1
go test -run TestExampleConfigMatchesLocal ./core
GOOS=linux GOARCH=amd64 go build -o /tmp/gdq-linux .   # 或临时目录
GOOS=darwin GOARCH=arm64 go build ./...
go build -tags dashboard ./...
cd web && npx vue-tsc --noEmit && npm run build
# 依赖边界断言（三条都必须为空）
go list -deps ./core | grep -i sqlite
go list -deps ./api  | grep -i sqlite
go list -deps ./executor | grep -i sqlite
# 格式检查（只看新增文件，不用 gofmt -l 的全量输出）
```

端到端场景（临时目录里的独立二进制 + 配置 + 数据，跑完删除）：

1. **默认关闭**：`observability` 一节整个删掉 → 启动、跑一批任务、`data/` 下没有 `.sqlite*`、两个事件端点响应与 S03 之前逐字段一致（存一份改动前的 JSON 做对照）。
   > 执行时更正：`note` 字段不是 S04 加的，S01 之前就有了（基线 `c971f81` 的响应里已带同一句话），所以两个版本的 `note` **恒等**而不是"从无到有"；
   > 真正的分岔值是 `persisted event store; newest entry may lag by the write flush interval`，只在装配了事件库时出现（见 §10.4 场景 3）。对照按"整份响应归一化后逐字段相等"做。
2. **开关矩阵**：总开关 × 三个子开关，逐个确认"建表但不写"与"完全不注入"两种状态都不报错、端点分别给出 503 或空列表。
3. **重启留存**：跑一条任务 → 停服 → 重启 → 时间线仍有重启前事件；`GET /jobs/:id/artifacts` 若这次执行是执行器任务则有两行。
4. **不回压**：`queue_capacity` 配成 2，提交 500 条批量任务 → 进程不崩、有 warn、`/admin/runtime` 报出 `dropped > 0`、普通任务的准时性与关闭耗时都没有变化（这条是 D4 的最终证据，也是整个设计里最需要现场证明的一条）。
5. **审计不含凭据**：全表搜索任何含 secret 参数取值的字符串，命中数必须为 0；同时确认 `route` 列不含 `?token=`。
6. **关闭顺序**：`SIGINT` 停服，日志无"向已关闭连接写入"、无 panic；`SIGKILL` 造崩溃现场，重启后库文件可打开、事件表只丢最后未落库的批次（把实际丢了几条记下来，与 `flush_interval` 对照）。
7. **磁盘异常**：把 `observability.path` 指向不可写目录 → 启动失败并给出原因（不是静默降级），确认这条错误信息可读。

## 6. 完成标准（DoD）

- [x] §5 的全部命令跑完并记录，全仓测试通过；任何一项在 Windows 之外没跑的（Linux 侧真实执行、权限位断言）都要在 `docs/deployment.md` 与本系列 README 里写明"未验证"。（命令清单与实际输出见 §10.3；Linux 侧未跑的项目在 `docs/deployment.md`"启用观测层"第 4 条与本系列 README 的"阶段进度"里各写了一句）
- [x] 依赖边界三条断言全为空。（`go list -deps ./core|./api|./executor | grep -i sqlite` 三条命中数均为 0；另加一条 `./store/sqlite → godelayq/api` 存在，方向是实现方引消费方，与 S05 同）
- [x] 三份使用者文档（`README.md`、`docs/api.md`、`docs/deployment.md`）里没有任何句子与本系列实现矛盾；`web-console-design.md` 的三处"留二期"改为指向已实现。（§10.1 逐文件列出改动的 12 处；§10.2 第 1 条记着 api.md 的四条中有三条是 S04/S05/S06 当卡已写、本卡核对确认；§10.7 是本条的 grep 命中清单与逐条处置）
- [x] 设计文档与实现有差异的地方**逐条标注**，不是静默改写。（新增 §15 十三行"设计原本如何 / 实现如此 / 为什么"，正文相应位置各留一行 `⚠️ 实现如此` 指回；§3 的现状盘点显式声明为不回改的基线）
- [x] 默认配置路径（不含 `observability` 一节）经实测与改动前一致，有对照记录（§5 第 1 条）。（场景 1：用 `c971f81` 另编一份改动前的二进制，与当前构建并排跑同一份 33 行配置，两份响应归一化后逐字段相等、`data/` 下无 `.sqlite*`；见 §10.4）
- [x] `api/history.go:23-25` 与 `executor/artifact.go` 相关注释按 §2 的判断处理完毕（该保留的保留并补指向，该改的改）。（history 的"重启即清空，不是审计日志"保留、把"属于二期"换成指向观测层与 S03/S04；artifact 的类型注释补了索引那条可选职责；另顺手把 `core/scheduler.go` 里"只留在内存缓冲"那句加了一个条件句）
- [x] 本系列 README 的状态表与"阶段进度"更新，遗留项单独列一节。（状态表 S07 行 + 新增"阶段进度与未验证项"一节，遗留项按卡片/缺陷编号列出）

## 7. 验收方式

```bash
# 复核 §5 的全部命令；文档侧用关键词扫残留的假话
grep -rn "重启即清空\|不引入新存储\|留二期\|不在本期范围" README.md docs/
```

`grep` 命中的每一处都要么已改为条件句，要么在 §3.5 的判断里被明确保留（代码注释里对未启用部署仍成立的那些属后者）。把命中清单与逐条处置写进第 10 节。

> 实际扫的时候把 `只有结构化日志`、`没有可查询的审计`、`不入库` 三个词并进来一起看（§2 那三处承诺的原话里就有它们），
> 命中清单与逐条处置在 §10.7。

## 8. 不在本任务范围

- 不改任何业务代码。若验证过程中发现缺陷：能一行改掉的顺手改并在第 10 节记录；改动超过一处的登记为缺陷，另立卡片，不在本卡夹带。
- 不做控制台页面（S08）。
- 不做性能基准报告：只记录"开关前后有无可见差异"，不产出 QPS 曲线。
- 不改 `docs/example.md`，除非 `examples/` 的行为受影响（本系列不影响：两个示例都不装配观测层）。
- 不做 SQLite 版本兼容矩阵（modernc 自带 amalgamation，无外部版本可言）。

## 9. 风险与回滚

- 风险：本卡最容易做成"只加新内容、不改旧句子"，于是文档同时存在两种说法。应对：§7 的 `grep` 清单是硬性的，每一条命中都要有处置记录。
- 风险：`-race` 下新增的订阅 goroutine 与关闭路径可能出现只在特定时序成立的竞争，跑一次通过不代表没有。应对：§5 第 4、6 两条各跑 5 次（`-count=5`），把结果记下来。
- 风险：端到端场景 2 的开关矩阵有 8 种组合，容易只测其中两三种。应对：列成表逐格打勾，缺格要在第 10 节说明为什么可以缺。
- 回滚：本卡纯文档，可单独 revert；但要求 revert 时同时把状态表回退，避免"文档说已完成、实现记录没写"。

## 10. 实现记录（执行时补写）

### 10.1 落点

**使用者文档**

| 文件 | 改了什么 |
| --- | --- |
| `README.md` | 8 处：适用场景那条补"执行记录可查"；架构图在持久化层下面加一块观测层（三张表 + "加速器不是账本"）；数据流图给事件总线补第三个订阅者；核心文件表加 `store/sqlite/` 一行；目录结构加 `store/sqlite/` 七个文件、`api/audit.go`、`api/handlers_artifacts.go`、`executor/artifact_index.go` 与 `docs/design/` 三项；核心特性第 4 条把"事件时间线"改成条件句并新增"观测层（默认关闭）"一条；配置项全表补 `observability` 一节（12 个键带默认值）；"其它文档"加设计文档与卡片目录链接；接入层安全"边界"那条把"无审计落盘"改成两条出口的条件句 |
| `docs/api.md` | 2 处（另 3 处经核对已由前序卡完成）：角色表给 `viewer` 补 `artifacts`、给 `ops` 与 `machine` 补台账档位；"事件里的执行结论"那段把"事件历史在进程内存里"改成条件句。**核对已完成**的：运行事件一节的 `note` 两个取值与分岔依据、库路径 `limit` 上界 1000、微秒精度与时区口径、`flush_interval` 延迟、读库失败 500 不静默退回（S04）；`GET /jobs/:id/artifacts` 全小节 + "索引从启用它的那个版本开始记"（S05）；`GET /admin/audit` 全小节（S06） |
| `docs/deployment.md` | 新增 `## 启用观测层（可选）`（在"开启执行器"之后、Systemd 之前），10 个小节按 §3.3 的十条逐项写：三个子开关与"关掉即回到原状"、依赖与体积实测、文件/旁文件/`VACUUM INTO` 备份、权限与"为什么单列"、`synchronous` 强度与崩溃实测、容量与保留（每行字节实测 + 两张吞吐估算表）、**丢弃计数当前只有关停日志可看**、503 与结构化日志兜底、与 `store.history_limit`/产物 TTL 的三处保留对照、Systemd 写权限清单；另把"已知边界"里"没有可查询的审计存储"改成条件句，把 Systemd 那段关停顺序补上观测层那一步 |

**设计文档**

| 文件 | 改了什么 |
| --- | --- |
| `docs/design/sqlite-observability-design.md` | 状态行改"已实现"并声明"差异逐条在 §15、正文不回改"；§3 现状盘点加"基线凭证、不回改"的说明；§5 架构图两处 `⚠️ 实现如此`（审计是读写两个接口、两个写入器合并成一个参数）；§6 开头加"DDL 与 `schema.go` 逐字对齐"的核对结论与两处必然差异；§6.3 末尾标注 `verdict` 九个取值与 `exec_reason_code` 混词；§7.1 标注 `dropped` 没有在线出口；§9.5 补完整的 21 项 `action` 映射表 + `logAccessRejection` 的实际落点；§12 验收清单改成带实测出处的表；§13 第 3 条标注为半实现；**新增 §15 十三行差异清单** |
| `docs/design/web-console-design.md` | §1.6 非目标里"运行历史的持久化审计"改为"已由 S03/S04/S06 落地 + 默认仍不启用"；决策 D4 那句"不入库"加 `⚠️ 实现如此`（默认不入库仍然成立）；§5.6 末尾"写操作审计仍在其后的 TASK-S06"改为"两处二期都已落地"。（`:607` 端点表那一行与 §5.7.6 全段在 S06 已改，本卡核对无误） |
| `docs/design/executor-design.md` | 两处顺手改（各一句）：§2 非目标末尾给"审计落盘"指出归属；§8"明确不做"里删掉"长期留痕请接外部日志"与"只有结构化日志"这两句假话，改为指向观测层设计。（该文件 §3 现状盘点表里"重启即清空"那一行**判断为基线记录、保留原文**——见 §10.7） |
| `docs/design/tasks/sqlite/README.md` | 状态表 S07 行；新增"阶段进度与未验证项"一节 |

**代码注释（三处，全是注释、无行为改动）**

`api/history.go:21-27`（保留"重启即清空，不是审计日志"，把"属于二期"换成指向 S03/S04 与 `api/handlers_events.go` 的兜底说明）、
`executor/artifact.go:17-30`（类型注释补可选索引职责与"文件仍是权威"）、
`core/scheduler.go:85-88`（"只留在内存缓冲"加条件句）。

### 10.2 偏离了卡片的地方

1. **§3.2 那张表里 api.md 的四条，有三条前序卡已经写完**（运行事件一节、artifacts 一节、`/admin/audit` 一节），
   本卡逐条核对后只补了权限矩阵与"restore_after_crash"那一句。清单没按现状裁剪，照做会变成重复劳动。
2. **§3.1 的 README 行号整体偏移**（那份文档被执行器系列改过）：`适用场景` 在 `:21` 不是 `:14`、
   配置项全表在 `:384-432` 不是 `:379`、核心文件表在 `:110` 不是 `:108`。按内容定位改的，没按行号。
3. **§3.3 第 7 条的前提不成立**：`/api/v1/admin/runtime` 里没有观测层段落，`DB.Stats()` 也没有 `dropped` 字段。
   部署文档因此按现状写"唯一的现场是关停时一条汇总 WARN"，并把缺的在线出口登记为 D-0701（改动要动 `RuntimeResponse`、
   装配与窄接口，超过 §8 的"一行顺手改"门槛）。同源的假话还在 `configs/config.example.yaml` 的注释里，见 D-0702。
4. **§3.3 第 2 条要求"写出本卡实测的数值"**：重新测了一次当前构建（`windows/amd64`、不带 tag）
   `38,813,696 B`，比 S02 记录的 `38,583,808 B` 又多 `229,888 B`（S03…S06 三个写入器的代码）。
   部署文档按"接入前 → 当前"两个数写，避免两个来源的数字混用（D-0707）。
5. **§3.3 第 6 条要求用实测行数估算**：本卡新加了一个容量探针（500 次写请求 → 500 行台账 + 500 行事件，
   `VACUUM INTO` 之后 311,296 B ⇒ **约 311 B/行**），再用场景 1/3/5 实测到的"一次完整执行 = 3 行事件 + 1 行台账
   （执行器任务再 +1 行产物索引）"推两张估算表。设计文档里"每天 10 万次执行"那句话现在有了数。
6. **§3.3 第 10 条是条件句**：Systemd 那份单元里没有 `ProtectSystem`/`ReadWritePaths`/`ProtectHome`，
   所以按条件写法保留（"只有你把 `observability.path` 指到别处时才需要动这一项"），没有改单元本体。
7. **§5 的命令清单加了三条**：`windows/386` 交叉编译与两条 `go list -deps` 的补充断言（沿用 S05 §10.3 的口径）；
   格式检查从"只看新增文件"改为"对被改动的三个 `.go` 注释做剥 CRLF 副本检查"（本卡没有新增 Go 文件）。
8. **§9 第二条要求"§5 第 4、6 两条各跑 5 次（`-count=5`）"——那两条是端到端场景，没有 `-count=5` 这种跑法**。
   按双重口径执行：端到端场景 4 与 6 各跑 5 轮，Go 侧对相关包补一次
   `go test ./store/sqlite ./api ./cmd/server -race -count=5 -run 'EventLog|Audit|Artifact|Batch|Shutdown|Close|Runtime'`。
9. **§5 场景 1 的"存一份改动前的 JSON 做对照"做法升级**：不是手工留一份旧响应，而是用 `git archive c971f81`
   另编一份改动前的二进制，与当前构建并排跑同一份配置、同一批动作，再逐字段比对归一化后的响应。
   顺带纠正了卡片的一处前提（D-0703）。
10. **§3.4 的"§6 DDL 与 `schema.go` 逐字对齐"没有靠改代码达成**：`schema.go` 里加的 `IF NOT EXISTS`
    与第四张迁移表是 S02 的既有实现（迁移要能整批重来），改它们会让 S02 的迁移用例重跑且没有收益。
    处理方式是在设计文档里写明"形状一致 + 两处必然差异"，代码不动。
11. **Windows 的优雅停服从"做不到"变成可测**：本系列前四张卡都记了"外部无法向控制台进程发 SIGTERM，
    关停结论只有 Go 用例覆盖"。本卡找到可做的方式（子进程用独立进程组起，再送 `CTRL_BREAK_EVENT`），
    场景 4/6 因此有了真实现场。旧记录不回改，口径更新写在设计文档 §15 第 12 条与 D-0705。
12. **§3.5 点的三处既有承诺，实际只剩两处要改**：`:607` 与 §5.7.6 在 S06 已改完（本卡核对），
    §5.6 那句"持久化审计不在本期范围"在 S04 已改，剩下的是非目标清单那一行与决策 D4 的"不入库"。

### 10.3 验证证据（§5 的命令清单）

```
go build ./...                       → 通过（2s）
go vet ./...                         → 通过（1s）
go test ./... -race -count=1         → 全绿（186s：api 182.2s / cmd/server 5.6s / core 13.0s / executor 24.9s / store/sqlite 3.7s）
go test -run TestExampleConfigMatchesLocal ./core -v → PASS（0.169s，是"通过"不是"跳过"：本机有 configs/config.yaml）
go build -tags dashboard ./...       → 通过
go vet  -tags dashboard ./...        → 通过（无输出）
go list -deps ./core|./api|./executor | grep -i sqlite → 三条命中数均为 0
go list -deps ./store/sqlite | grep godelayq/api       → 1 条（实现方引消费方，与 S05 同方向）
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build ./cmd/server → 通过
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build ./cmd/server → 通过
CGO_ENABLED=0 GOOS=windows GOARCH=386   go build ./cmd/server → 通过（本卡补的一条）
gofmt -l -s（对被改动的 3 个 .go 做剥 CRLF 副本）        → 无输出
cd web && npx vue-tsc --noEmit       → 通过（无输出）
cd web && npm run build              → 通过（vite 3.06s，产出 web/dist，未入库）
```

按 §9 第二条的重复验证（时序敏感的那两条）：

```
端到端场景 4（不回压）× 5 轮 × 两种队列容量      → §10.4
端到端场景 6（崩溃与关停）× 5 轮                  → §10.4
go test ./store/sqlite ./api ./cmd/server -race -count=5
    -run 'EventLog|Audit|Artifact|Batch|Shutdown|Close|Runtime'
  → 全绿（store/sqlite 20.6s / api 402.2s / cmd/server 1.7s；每包各 5 遍）
```

改动前后对照：三处 `.go` 只动注释，改后复跑
`go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿
（api 178.2s / cmd/server 5.6s / core 12.6s / executor 23.5s / store/sqlite 3.8s），
`gofmt -l -s`（剥 CRLF 副本）对这三个文件无输出。

### 10.4 端到端七个场景（§5）

环境：`%LOCALAPPDATA%\Temp\s07e2e`，独立二进制 + 各自 `data/` + `ws/` 里的两个脚本档位，端口 8200-8312 逐段分配，
每个场景跑之前先确认端口空闲并清掉上一轮的残留进程；跑完删除临时目录。仓库的 `configs/config.yaml` 与 `data/` 全程未写。
配置由 `mkcfg.py` 按场景生成（只写必要项，观测层段落按参数出现或缺省），场景驱动 `drive.py`（场景 1/3/7）
与 `drive2.py`（场景 2/4/5/6）。容量数字另用一个独立探针 `s07probe`（见 §10.3 末与 §10.4 场景 4/6 之外的说明）。

> 记录说明：临时目录在本卡收尾之前被系统回收过一次（`s07e2e` 只剩最后写入的一个文件），
> 上面引用的每一个数字都取自回收前各次运行的终端输出，脚本本身没有留档。
> 要复现需要重建 `mkcfg.py`/`drive*.py` 与两个档位脚本——本轮不重跑（结论未变，且冒烟本就要求跑完删除）。

**场景 1（默认关闭，逐字段对照）**：`observability` 一节整段不写。改动前的二进制（`c971f81`）与当前构建各跑一遍
"登录 → 建一条 `data_sync` → 等 2.5s → 读两个事件端点"，把 UUID、时间戳、耗时、状态数换成类型标记后比对：

```
baseline c971f81：per-job count=3 types=[scheduled, started, completed]  global count=3  旁文件=无
HEAD           ：per-job count=3 types=[scheduled, started, completed]  global count=3  旁文件=无
逐字段对照（归一化后）：完全一致
```

`note` 两边的值都是 `in-memory buffer, cleared on restart`（见 D-0703：这字段不是 S04 新加的）。

**场景 2（开关矩阵 8 格）**：总开关 × `events` × `artifacts` × `audit`，每格都是新目录新库。
"装配"列读自启动日志的三个字段，端点列是实测 HTTP 码，note 列区分"库/内存"：

| # | 组合 | 装配了的 | artifacts | audit | events | note | 表行数（事件/索引/台账） |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 总开关 off | 无（不建库） | 503 | 503 | 200 | 内存 | 无库文件 |
| 2 | 全 on | events+artifacts+audit | 200 | 200 | 200 | 库 | 6 / 1 / 3 |
| 3 | 只关 events | artifacts+audit | 200 | 200 | 200 | 内存 | **0** / 1 / 3 |
| 4 | 只关 artifacts | events+audit | **503** | 200 | 200 | 库 | 6 / **0** / 4 |
| 5 | 只关 audit | events+artifacts | 200 | **503** | 200 | 库 | 6 / 1 / **0** |
| 6 | 关 events+artifacts | audit | 503 | 200 | 200 | 内存 | 0 / 0 / 3 |
| 7 | 关 events+audit | artifacts | 200 | 503 | 200 | 内存 | 0 / 1 / 0 |
| 8 | 只留 events | events | 503 | 503 | 200 | 库 | 6 / 0 / 0 |

八格全测到（§9 第三条要求不缺格）。三格"关掉的表 0 行而库文件存在"正是"建表但不写"；
第 1 格是"完全不注入"（连文件都不建）。第 2 格台账 3 行而第 4 格 4 行的差不是丢失：
`ops` 账号那次登录发生在读台账之前几十毫秒、还没到一个 `flush_interval`，
就是 `note` 第二句说的这件事（顺带取到一次现场）。

**场景 3（重启留存）**：一条普通任务 + 一条 `exec.hello`，硬停后重启再查：

```
重启前 01a0f4e2-…acd5：note='persisted event store; …' 条数=3
重启后 HTTP 200：note 同上，条数=3 类型=[job.scheduled, job.started, job.completed]
重启后产物列表：{"count":1,"items":[{"attempt":1,"kind":"script","profile":"hello","out_bytes":19,
                  "err_bytes":0,"truncated":false,"state":"available","created_at":"2026-10-01T08:35:02+08:00"}]}
重启后台账 total=5；普通任务时间线 3 条；旁文件 observe.sqlite / -wal / -shm 三个都在
```

**场景 4（不回压，D4 的最终证据；5 轮 × 两种容量）**：`queue_capacity: 2` 对照默认 4096，
每轮 5 个 `POST /jobs/batch`（单请求上限 100，见 D-0702）共 500 条排期在 30 分钟后的任务，
外加一条 `delay=2s` 的普通任务计时；停服用独立进程组的 `CTRL_BREAK`：

```
queue_capacity=2      ：批量 5 个请求全部 207、总耗时 0.05-0.08s；普通任务 2.04-2.14s 完成；
                        进程存活、以码 0 优雅退出；pending=500
                        关停 WARN：event writer dropped=495/497/497/499/497，audit writer dropped=2/3/3/4/3
queue_capacity=默认    ：同样 207 / 0.00-0.07s / 2.08-2.14s / 退出码 0 / pending=500，没有 dropped 行
```

结论：队列打满时**丢的是观测记录，请求路径与调度时序没有可见差异**（普通任务 2.04 vs 2.08s 同量级、
批量耗时同量级、500 条全部在册）。丢弃数只出现在关停 WARN 里——这就是 D-0701。

**场景 5（审计不含凭据）**：三条 canary 分别放进 secret 参数（`exec.nosync` 的 `token`）、
只打印到 stdout 的普通参数（`exec.hello` 的 `who`）、以及一个写请求的查询串 `?token=…`；
停服后按列扫三张表（`CAST(col AS TEXT) LIKE`）：

```
secret 参数取值   's07-secret-arg-4d7a'   → write_audit 0 / job_events 0 / artifact_index 0
查询串取值        's07-query-value-9c22'  → write_audit 0 / job_events 0 / artifact_index 0
stdout 打印的入参 's07-stdout-arg-1e55'   → write_audit 0 / job_events 1 / artifact_index 0
write_audit.route 含 '?' 的行数 = 0
台账三行：auth.login / job.create / job.create，route 全是模板
```

`write_audit` 三列全 0 命中、`route` 无查询串，本卡 §6 第三条守住。第三行那 1 处命中是 D-0605 的复现：
事件 `job.completed` 的输出预览（`result.preview`）把脚本打印的值带了回来——处置见 D-0704。
第一行 0 命中而不是 1，是因为预览取的是**一路**流的尾部（这条档位的 stderr 覆盖了 stdout），所以换个写法才会命中。

**场景 6（关停顺序与崩溃；5 轮）**：`flush_interval: 500ms`，400 条批量落库途中 `taskkill /F` 造崩溃，重启后清点：

```
第 1 轮 job_events=400（丢 0）  write_audit=5（丢 0）  重启后端点可读 200  重启后优雅关停=退出码 0  可疑日志 0
第 2 轮 job_events=0  （丢 400） write_audit=0（丢 5）  同上
第 3 轮 400 / 5，丢 0
第 4 轮 400 / 5，丢 0
第 5 轮 job_events=0（丢 400） write_audit=1（丢 4）
```

规律与 `synchronous: normal` 的承诺一致：**丢的就是"最后一个还没提交的合并周期"里攒下的行**，
要么整批在、要么整批不在，没有出现半行或坏行；重启后端点照常可读、库照常能优雅关停，
五轮日志里都没有 panic、没有"向已关闭连接写入"。5 轮里 2 轮全丢是因为那两次从启动到硬杀没跨过一个 500ms 的 tick。

**场景 7（磁盘异常）**：`observability.path` 指向三个不可用的位置，都启动失败且原因可读、端口没被占用：

```
C:/observe.sqlite                    → ERROR "sqlite: create database file: open C:/observe.sqlite: Access is denied."
C:/:/bad/observe.sqlite              → ERROR "sqlite: create dir for C:/:/bad/observe.sqlite: mkdir C:\:: The filename,
                                        directory name, or volume label syntax is incorrect."
指向一个已存在的目录（ws/）           → ERROR "sqlite: create database file: open …/ws: is a directory"
```

三种都是退出码 1、`server exited with error`，没有任何"降级继续跑"的路径（设计文档 §11 的口径）。

### 10.5 缺陷处置

| 编号 | 严重度 | 事实 | 处置 |
| --- | --- | --- | --- |
| D-0701 | 中 | **`dropped` 没有在线出口**：设计文档 §7.1 写"累计值挂到 `/admin/runtime` 的观测输出里"、§13 第 3 条同样承诺、卡片 §3.3 第 7 条也按它有；实测 `RuntimeResponse` 里没有观测层段落，`DB.Stats()` 只有三张表行数与 `schema_version`（S02 当时明确不预留该字段），唯一的现场是关停时一条汇总 WARN。长期不重启的进程可以一直缺页而读不到数 | **登记不修**（改动要同时动 `api/handlers_admin.go` 的响应结构、`cmd/server` 的窄接口与装配，超过 §8 的"一行"门槛）：本卡把 `docs/deployment.md`"启用观测层"第 7 条按现状写清"怎么看、看不到什么"，设计文档 §7.1/§13/§15 第 10 条同步标注，处置建议（给 `/admin/runtime` 加一段 `{rows, dropped}`）留给后续卡 |
| D-0702 | 低 | `POST /jobs/batch` 单请求上限是 100（`api/handlers.go:23` 的 `maxBatchCreateSize`），卡片 §5 场景 4/6 的"提交 500 条 / 400 条批量任务"一次发不完，首轮拿到的是 `400 batch too large`（于是那两轮的观测库里什么都没写，测试结论一度失真） | **已按现状执行**：驱动改成 5×100 与 4×100 分批发送，并在 §10.4 写明"批量单请求上限 100"这一前提；卡片正文不回改（它是当时的认知），`docs/api.md` 早已写明 100 条上限 |
| D-0703 | 低 | 卡片 §5 场景 1 的预期"两个事件端点响应与 S03 之前逐字段一致（存一份改动前的 JSON 做对照）"隐含"`note` 是新增字段"；实测基线二进制（`c971f81`，S03 之前）的响应里 `note` 已存在且句子相同（`in-memory buffer, cleared on restart`，`c260956` 引入） | **已在卡片更正**：§5 场景 1 加了一条执行时更正，说明真正的分岔值是持久化那句，对照按"整份响应归一化后逐字段相等"执行；实测两份响应确实逐字段相等 |
| D-0704 | 中 | S06 登记给本卡判断的 D-0605 复现了：脚本把入参打印到 stdout 时，`job.completed` 事件的 `result.preview` 会把参数取值写进 `job_events.data`（场景 5 实测 1 处命中）。设计文档 §10.1"不进表的内容：……参数取值"字面覆盖三张表 | **判断为"文档说清了、行为不改"**：`write_audit` 侧 0 命中，本卡范围内没有需要改的代码；删预览会改变 `GET /jobs/:id/result`、控制台时间线与既有文档的表现，超出收口卡。落地的处置是三处文字——设计文档 §15 第 13 条、`docs/deployment.md` 第 4 条（把"事件库含输出预览"写进为什么要设权限的理由里）、S06 卡 §10.5 保持原登记。若要真正收口，方向是给 `executors.output.inline_preview` 加一个"不落库"的档，另立卡片 |
| D-0705 | 低 | 本系列前四张卡都记了"Windows 上外部发不出优雅 SIGTERM，关停结论只有 Go 用例覆盖"。本卡实测该口径偏保守：子进程以 `CREATE_NEW_PROCESS_GROUP` 起、驱动侧 `os.kill(pid, CTRL_BREAK_EVENT)`，Go 的 `signal.Notify(SIGINT)` 收到同一次中断，进程走完整关停后以码 0 退出 | **已按新口径执行并记录**：场景 4/6 用了这条路径；设计文档 §12 第 2 条与 §15 第 12 条写明。旧卡的记录不回改（当时的结论是真的），但**后续卡不要再把优雅停服列为"Windows 测不了"** |
| D-0706 | 低 | 卡片 §3.1/§3.2/§3.3 引用的行号在本系列推进过程中已经整体偏移（README 被执行器系列改过、api.md 被 S04/S05/S06 加过节），照行号改会改错位置 | **已按内容定位**：本卡所有文档改动按标题与语义找位置，§10.2 第 2 条记下偏移实例。后续卡的清单类章节建议写锚文本而不是行号 |
| D-0707 | 低 | 二进制体积有两个来源的数：S02 记录的 `38,583,808 B` 与本卡重测的 `38,813,696 B`（差 `229,888 B` 来自 S03…S06 三个写入器），设计文档 §13 第 1 条只写了前者 | **已修文档**：`docs/deployment.md` 第 2 条同时给"接入前 / S02 时 / 当前"三个值并说明差的来源；设计文档不改（那是 S02 当时的实测凭证） |
| D-0708 | 低 | 场景 2 第 2 格的台账行数比第 4 格少一行，看着像丢数据；实际是 `ops` 账号那次登录距查询不足一个 `flush_interval`，还没落库 | **无缺陷，已记录**：这正是 `note` 第二句承诺的语义，§10.4 的表下面写了一句，免得下次有人按"应该有 N 行"判故障 |

### 10.6 未覆盖与已知边界

- **Linux/macOS 一切未实跑**：权限位（`0640/0750`）、`-race` 真机、`SIGTERM` 关停、Systemd 单元、
  `VACUUM INTO` 的 shell 例子（本卡用 Python 驱动的同一条语句量的大小，命令行形态未跑）都没有跑过。
  `docs/deployment.md` 第 4 条与本系列 README 的"阶段进度"各写了一句"未验证"。
- 场景 4 的"普通任务准时性"是单次计时（`delay=2s` 的一条），没有做吞吐曲线或 P95；
  卡片 §8 明确不做性能基准，这里只留"两池同量级"的对照。
- 场景 6 的崩溃只测了 `taskkill /F`（等价 `SIGKILL`），没有测掉电/磁盘满；
  `synchronous: full` 的行为差异只有单元测试与文档描述，没跑对照轮。
- 容量估算用一次探针（500 写 + 500 事件，`VACUUM INTO` 后 311,296 B）外推，未测：
  含长 `user_agent` 与中文账号名的行、WAL 长期不 checkpoint 时的旁文件峰值、
  表接近 `retention_count` 上界时的淘汰耗时。每行字节数因此是**量级**而不是承诺值。
- 前端只在场景 3 间接涉及（`note` 由后端给、界面直接引用），**本卡未做浏览器实测**；
  `npx vue-tsc --noEmit` 与 `npm run build` 通过不代表页面上那两句文案的观感（S04 的 D-0403 三处文案仍未处理）。
- `/admin/audit` 与 `/jobs/:id/artifacts` 的响应在真实浏览器里的翻页/筛选行为留给 S08（控制台审计页）。
- 设计文档 §15 的十三行差异只覆盖"设计写了、实现不同"的方向；
  反过来"实现有、设计没写"的东西（比如 `observability.artifacts.enabled` 需要 `executors.enabled` 才建索引）
  记在各自卡片的第 10 节里，本卡不重复搬运。

### 10.7 §7 的 grep 命中清单与逐条处置

命令（比卡片多并了三个词，理由见 §7 下那条说明）：

```bash
grep -rn "重启即清空\|不引入新存储\|留二期\|不在本期范围\|只有结构化日志\|没有可查询的审计\|不入库" README.md docs/
```

| 命中 | 判定 | 处置 |
| --- | --- | --- |
| `README.md:188`（目录结构里 `history.go` 那行） | 说的是内存缓冲本身，仍成立 | **已改**为条件句："启用事件库后它是兜底" |
| `README.md:308`（核心特性"事件时间线"） | 原句无条件断言"重启即清空" | **已改**：默认路径 + 启用后的路径两句并列，并给出 `note` 的说明职责 |
| `README.md:339`（"边界"条目） | "无审计落盘"现在是假话 | **已改**：两条出口写清，并保留"都不记请求体与参数取值" |
| `README.md:368`（"含凭据的那份不入库"） | 讲的是 gitignore，与观测层无关 | **保留**（同词不同义） |
| `docs/deployment.md:159` | "没有可查询的审计存储"是假话 | **已改**为条件句，并指向新增的"启用观测层"一节 |
| `docs/design/executor-design.md:80` | 位于 §3"现状盘点（规划时基线，已核实）" | **保留原文**：那是基线凭证，回改会把"当时看到什么"抹掉。中途我误改过一次，已回退 |
| `docs/design/executor-design.md:462`（§8 明确不做） | "长期留痕请接外部日志""只有结构化日志"两句成假 | **已改**：整段拆成"仍然不做的清单"+"两条已由观测层落地"的指向 |
| `docs/design/sqlite-observability-design.md:17/19` | 同属基线盘点 | **保留**，并在 §3 开头加了一句"不回改、现在以 §5–§10 为准" |
| `docs/design/tasks/executor/*.md` 五处、`docs/design/tasks/sqlite/*.md` 六处 | 前序卡片的正文与第 10 节记录（历史凭证） | **全部保留**：卡片是执行记录，回改等于伪造当时的判断 |
| `web-console-design.md` 的三处 | — | 已无命中：`:607` 与 §5.7.6 由 S06 改、§5.6 末尾由 S04 改，本卡又补了非目标条目、决策 D4 与 §5.6 末句 |
| 卡片自身（本文件 §2/§3/§5/§7 的引用） | 引用别处原话作为待办 | **保留**：那是任务描述，不是产品声明 |

扫完之后的结论：三份使用者文档与设计文档里**没有残留的无条件假话**；仍写着旧事实的地方要么被显式标为基线凭证，
要么就是卡片本身的历史记录。
