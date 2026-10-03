# 配置热重载任务卡（TASK-R01 … R07）

设计说明在 `../../config-reload-design.md`，本目录把它拆成可逐个执行的卡片。
卡片只写"做什么、怎么算做完、怎么验"，设计理由不重复。

一句话概括这个系列：让进程自己发现 `configs/config.yaml` 变了，把其中"取值型"的配置项
在运行期换上，其余项明确告诉人"这次没生效、要重启"，改到凭据或执行许可就整次作废。

三条系列级口径，任何一张卡都不许松动（对应设计文档 §4 的 I1/I2/I3）：

1. **一份权威**：`applied`（当前生效的那份配置）是"现在到底跑哪套值"的唯一答案，只有重载成功才换。
   各子系统继续持自己的取值快照，本系列不把任何子系统改成"实时读配置"。
2. **要么全变要么没变**：一次重载先整份读通（`LoadConfig` 的精确解码 + `Validate`），
   再过拒绝档，然后逐项应用；中途失败就用记下的旧值逐项回滚，`applied` 保持旧值。
   回滚本身失败必须进入 `degraded` 并让人看得见，不许只留一行日志。
3. **没生效必须说出口**：重启档进 `ignored_keys`、拒绝与失败进 `error`、监听器故障进 `watcher_error`。
   静默不生效是本系列要消灭的东西，不是一种可接受的默认行为。

## 执行顺序

| 阶段 | 任务 | 内容 | 该阶段结束时可演示的能力 |
| --- | --- | --- | --- |
| M0 判据 | R01 | `reload` 配置节、三档分类表、`Diff` 与两条守卫测试 | 能对两份配置说清"哪些键可热更、哪些要重启、哪些要作废"，仍无任何行为变化 |
| M1 落点 | R02 | 六个轻量 setter（日志级别、重试上限、存储留痕、观测层两处的保留策略） | 每个热更键都有一个可调的入口，单包内验证完毕 |
| | R03 | `Scheduler.ResizeWorkers` 运行期扩缩 worker | 不重启把并发从 8 调到 32 再调回 4，在途任务不被打断 |
| | R04 | `Registry.ApplyConfig` + `Applier.ApplyConfig` | 不重启增删 `executors.commands` 档位，处理函数随之整批换掉 |
| | R05 | `core.ConfigWatcher`（目录监听 + 防抖 + 内容去重 + 状态） | 改文件能触发一次回调，坏文件与删文件都有明确结论 |
| M2 接线 | R06 | `cmd/server/reload.go` 的 `reloader`（分派表 + 逆序回滚）+ `/admin/runtime` 的 `reload` | 改文件后日志级别、并发数、留痕、档位真的变了；失败时旧值原样 |
| M3 收口 | R07 | 端到端场景实测、全量验证、既有文档同步 | 十四条场景逐条实测，文档与新事实一致 |

依赖关系（与各卡片头部的"依赖任务"一致，冲突时以卡片为准）：
R01 先行；R02 需要 R01；R03、R04、R05 都只依赖 R01/R02，可并行；
R06 需要 R02–R05 全部；R07 需要 R06。

## 每张卡片的结构

1. 任务目标：一句话说明这张卡交付什么。
2. 背景与当前问题：为什么现在没有，缺了会怎样。
3. 要实现的功能：逐条列出，可测试。
4. 实现步骤：按顺序的操作。
5. 测试要求：单元测试、集成测试、手工测试分别做什么。
6. 完成标准（DoD）：全部满足才算完成。
7. 验收方式：可直接复制的命令和预期输出。
8. 不在本任务范围：容易被额外多做、但明确不做的事。
9. 风险与回滚：可能遇到的问题，以及出问题时怎么退回。
10. 实现记录：**执行时补写**，逐条记录与卡片的偏离及原因。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./... -race` 必须通过。
- `gofmt -l` 在本仓库会因为行尾 CRLF 报出大量文件，结果不可用。要做格式检查时只对新增文件
  显式 `gofmt -w <file>`，不要按 `gofmt -l` 的输出判断是否合格。
- 需要重复跑的用例一律带 `-timeout`：`go test ./... -race -count=5 -timeout 30m`
  （档位在线管理系列的 D-0901 就是这么踩的：`api` 包不带 `-timeout` 必然败在 600s 默认超时上）。
- **本系列会新增配置键**（`reload.enabled`、`reload.debounce`），必须同时改
  `configs/config.example.yaml` 与本机 `configs/config.yaml`（后者被 `.gitignore` 排除），
  否则 `go test -run TestExampleConfigMatchesLocal ./core` 失败（本机没有 `config.yaml` 时
  该测试是跳过而不是通过，记录时要分清）。
- 两个易混机制：YAML 未知键被拒来自 `core/config.go:806` 的 `UnmarshalExact`；
  `core/config.go:734-794` 的 `BindEnv` 字符串列表漏项**不会报错**，只会让该项无法用
  `GODELAYQ_*` 覆盖。R01 的 DoD 里专门有一条守这个。
- 依赖方向红线：`core` 不许 import `executor`、`store/sqlite`、`api`；`executor` 与 `api`
  可以 import `core`（现状即如此）。本系列把三档分类表、`Diff`、`ReloadState`、`ConfigWatcher`
  全放 `core`，把"新值 → 具体 setter"的分排放 `cmd/server`，就是为了不破这条线。
  由此得出一条贯穿全系列的口径：**拒绝档的判据只用 `core` 自己能看到的信息**
  （配置结构体里的字段），不借助 `executor` 侧的归一化结果——理由见设计文档 §6.4 与本目录的待拍板 P2。
- 构造函数命名一律 `New*`（`Open` 属既有例外名单）。
- 本系列**不改前端**：`/admin/runtime` 多出的 `reload` 对象只在 REST 与日志里可见，
  控制台页面本期不读它（设计文档 §10 的 N5）。因此各卡都不涉及 `web/`，
  只有 R07 要改使用者文档里的文字表述。
- 涉及端点的卡片（只有 R06）：`/admin/runtime` 是既有 GET 端点，不新增写端点，
  因此 `api/audit.go` 的 `auditActions` 映射**不需要**加行；R06 要用一条用例证明这一点
  （重载状态不写台账）。
- 每张卡片一个提交（改动跨面大时可按"新增文件 / 接线 / 测试"拆 2-3 个），
  提交信息用 Conventional Commits，前缀按内容选 `feat` / `fix` / `test` / `docs` / `refactor`。
- 在共享 worktree 里不要直接 `git stash`；需要暂存时用带名字的 tag。
- 冒烟一律在系统临时目录（`%TEMP%`，Windows 本机；注意该目录里的东西可能被清理器回收，
  重建后要重跑全部步骤）造独立配置 + 数据目录 + 产物目录，跑完删除；
  不得写入仓库的 `configs/config.yaml` 与 `data/`。端口用 `GODELAYQ_SERVER_PORT` 挑一个
  IPv4/IPv6 都空闲的。
- ⚠️ 本系列的冒烟有一条比前两个系列更麻烦的地方：**被测对象就是配置文件本身**。
  每个场景都得改那个真的被 `-config` 指着的文件，改完要还原；一次场景里既要等 watcher 触发，
  又要能区分"是没触发"还是"触发了但被判不生效"，所以判据一律取自日志与 `/admin/runtime`
  的 `reload` 对象，不要靠"看起来任务跑得快了"来推断并发数变了。
- 收口卡（R07）要跑：`go build -tags dashboard ./cmd/server`、三条交叉构建、
  `go test ./... -race -count=1`，并把"受平台限制没跑的项目"逐条列清而不是含糊带过。

## 待拍板（写卡时按推荐值落的，执行前请复核）

| # | 问题 | 卡片采用的答案 |
| --- | --- | --- |
| P1 | 是否给 `core.Store` 接口加 `SetHistoryRetention`（所有测试替身要一起实现） | 加，与 `schedulerAPI` 上那组 `SetExec*` 同法（设计文档 §13 P1、R02 §3.3） |
| P2 | 拒绝档的判据是否借用 `executor.LoadProfiles` 的归一化结果 | 不借，只用 `core.ExecutorCommand` 的字段清单（依赖方向红线；设计文档 §6.4 的"投影"落地为 §5 的字段清单，见 R01 §3.4） |
| P3 | `reload.debounce` 默认值 | 500ms（比目录加载器的 100ms 宽：改错的代价高于多等 400ms） |
| P4 | 被 `GODELAYQ_*` 压住的键是否在重载结果里单列 | 不单列，统一走"本次无变化"文案（设计文档 §13 P4） |
| P5 | `web_enabled=false` 但 `reload.enabled=true` 时是否仍打开档位文件并构造 `Applier` | 构造 `Applier`，不打开档位文件（给它一个不依赖 store 的构造入口）。设计文档 §13 的 P2 已同步成这条，理由是该组合今天连 `profiles_path` 的父目录都不碰 |

## 状态

| 任务 | 状态 |
| --- | --- |
| R01 | 已完成（2026-10-02，实现记录见该卡 §10；**11 处偏离**，其中四条是抓到卡面前提不成立并就地改写卡面：§3.1 第 4 条与 §5.2/§9 自相矛盾（"`enabled: false` + 10ms 也拒"才是落地方向，`debounce == 0` 唯一合法）、§9 风险表第四条写的 `server.auth.users[0].password_bcrypt` 子路径现在根本摊不出来（只有 `executors.commands` 按元素摊，前缀命中因此改成"等值或子路径"两形态都拒）、**§3.4 少写了一条判据：纯路径 `classify` 会把新增档位的 `runtime`/`script` 判进拒绝档，与 §5.3 表里"增一条 script → `Hot` + `Commands`"直接冲突，落地在 `Diff` 内加 `classifyChange`（条目名只在一侧出现 → 算增删 → 热更）并把 §3.4 改成四条行为要求**、卡正文行号是动笔基线位置落地后整体下移（现行位置记在 §10.1）；另三条实现口径卡里没写：指针（`*bool`/`*ExecutorPositional`）当一个 `leafScalar` 叶子不递归进背后、`classify` 未命中时返回 `ClassRestart` 而不是零值热更档、`isCommandList` 按元素类型判而不是按路径字面量写死。**三档表实际规模**：`configClasses` 50 条（热更 11 / 重启 39）+ `rejectPrefixes` 3 条 + `permissionCommandFields` 14 条，`flattenLeaves(DefaultConfig())` 摊出 53 个叶子、§5.1 的双档位样本摊出 104 个。守卫用例 `TestEveryLeafKeyIsClassed` 双向绿并**做了三处变异反向验证**：删 `"store.groups_path"` 一条 → `leaf key "store.groups_path" has no reload class`；表里留 `"logging.formatt"` + 把 `script` 写成 `scrript` → 反向两条各自报红；恢复后绿。TDD 先红证据留档（`undefined: flattenLeaves/classify/configClasses/...` 的构建失败，以及只加类型不加四处配套时 §5.2 三条用例分别红在 `DefaultConfig`/`BindEnv`/`Validate` 上）。验收三条命令全绿：卡 §7 命令 1 共 30 条 PASS 含 `TestDiff` 的 21 个子用例、**`TestExampleConfigMatchesLocal` 是 `--- PASS` 不是 SKIP**；`go build`/`go vet` 无输出；全量 `go test ./... -race -count=1` 退出 0（api 225.298s / core 12.733s / executor 24.679s / cmd/server 5.826s / store/sqlite 3.635s），core 包 346 条 `--- PASS` 零失败、既有用例一条没改预期也没删。**手工（§5.4）**：`%TEMP%` 冒烟配置写 `debounce: 10ms` 后 `go run ./cmd/server -config=...` 启动失败并给出 `reload.debounce 10ms is too small, a merge window below 50ms means re-reading the config file several times per save (omit the key to use 500ms)`，目录里除 config.yaml 外没建任何文件；合法 `500ms` 那份能起且日志零 reload 痕迹（跑完 taskkill + 删目录）。**行为零变化**得到证明：全仓无监听、`cmd/server`/`api`/`executor`/`store/sqlite` 零改动，`Diff`/`classify`/`ReloadState` 除测试外零引用。新缺陷三条 **D-R0101（`ReloadState` 两个 `time.Time` 带 `omitempty` 无效，会给出 `0001-01-01` 假时间）登记不修归 R06**、D-R0102（"入参必须已 `Normalized`"无法在 core 内自检，只有注释与用例约定）登记不修归 R06、D-R0103（无名/重名档位靠 `#<i>` 兜底与覆盖，配对语义不成立）登记不修归 R04；两份 YAML 的 `reload:` 注释里"接线在 R05/R06，R07 收口前打开本节不会有行为变化"那条是暂时口径，**R07 必须改写**；Linux/macOS 未实跑（本卡纯内存计算，交叉构建归 R07）。**质量复核轮**（同一卡二次返工，规格审查已先行通过）：修掉 3 条 Important + 10 条 Minor，其中两条是新的静默失效路径并已登记为缺陷——**D-R0107**（`flattenStruct` 对"摊不出任何导出字段的结构体"产出 0 个叶子，那个配置键既不进分类表也不进 `Diff`）与 **D-R0106**（含点档位名被 `splitCommandLeaf` 切错、归因署错名；卡 §9 风险表把"名字不含点"的前提挂在 `ValidateProfileName` 上，而**配置加载链根本不调它**，卡面已就地改正）。用例规模由 30 条变为 **16 个顶层用例 + 22 个子用例**（新增容器叶子、含点名归属、`ConfigClass.String` 三条），§10.1 的"位置"列改成按符号名定位（不写行号）。六条变异反验证（M1 容器叶子兜底、M2 遍历排序、M3 含点名清洗、M4a 两份清单交叠、M4b `Commands` 漏容器路径、M5 整节漏摊）全部判红、恢复后逐字节相同；终态 `go build`/`go vet` 无输出，`go test ./core -race -count=5 -timeout 30m` 55.939s，全量 `-race -count=1` 五包全 `ok` |
| R02 | 已完成（2026-10-02，实现记录见该卡 §10；**12 处偏离**，其中三处是抓到卡面前提不成立并就地改写卡面：**§5.2/§6 把"既有断言因字段类型要换读法"写成一处、实为四处**（`core/scheduler_test.go` 两条 + `core/store_history_test.go` + `core/config_test.go` + `store/sqlite/events_test.go`，一律只换 `field.Load()`、期望取值与语义不动）、**§5.4 点名的 `auditEntryFixture(i)` 与"method、path、result"字段全仓不存在**（既有的叫 `auditEntry(seq)`，`api.AuditEntry` 没有 `Path`/`Result`）、**§5.2 把 `mockRetryPolicy` 的定义位置写错且它记不下"被问的那一刻"**，故新增 `recordingRetryPolicy`）。另两处卡面理由被落地时证伪并改正：`quietLogger` 消音与 ±1ms 精度无关（两步都在 warn 之前算完）、`trimTerminalLocked` 的局部快照不是为了防撕裂而是为了让同一次淘汰的条数与时长来自同一代策略。**§3.5 那张"热更键 → 入口 → 生效时机"对照表落在卡 §10.1**（卡面允许包注释或实现记录，选后者以免把内部接线口径写进 `core` 的包文档）。补齐 `Store` 接口实现者两名（`mockStore`、`stubStore`，判据取 `go vet ./...` 而不是 `go build`，因为实现者全在 `_test.go`）。新用例 12 条（core 7 + store/sqlite 5）全绿，`-race -count=3` 干净；**六条变异反验证里 N5 是等价变异**（负时长折成 0 与 0 在 `prune` 的 `<= 0` 判据下同义，没有任何用例能区分，如实记在 §10.3/§10.6 而不是造一条只断私有字段的用例冒充覆盖）。新缺陷三条 **D-R0201（两个保留值是两次独立 `Store`，存在"新条数配旧时长"的瞬时窗口）登记不修归 R06**、D-R0202（`RetryPolicyMaxDelay` 只认 `*ExponentialBackoffRetry`，自定义策略读 0）登记不修归 R06 的读数文案、**D-R0203（设计文档 §6.1 把 `store.flush_interval` 误列为热更键，与 `configClasses` 的 `ClassRestart` 及 `core/store.go` 的 ticker 三行注释冲突）归 R07 文档同步**。行为零变化：六个入口除测试外零调用方，`cmd/server` 只多了测试替身的一个方法。Linux/macOS 未实跑。**质量复核轮**（同一卡二次返工，规格审查先行通过）：修掉 3 条 Important + 9 条 Minor，其中三条是"判据空转"——`TestSetHistoryRetention_ConcurrentWithTrim` 的上界 31 没有出处且把判据建在协程调度顺序上（改成并发结束后自己再设一次已知值收口）、三条"回落到默认"的用例在"回默认"与"当不限量"两种实现下都能过（改成断存进原子位的取值）、`TestSetRetryPolicy_ConcurrentWithFailureHandling` 的尾段其实证不了跨协程可见性（改成"每次读数都必须是写进去过的那一类值"+如实声明判据是 `-race`）；另外为"一次失败只 `Load()` 一次"这条注释主张补了 `wantCalls` 断言，`TestNewLogger_KeepsOldContract` 因三条断全是既有用例子集而换成只断"转调后级别仍生效"的 `TestNewLogger_DelegationWritesThrough`，卡 §5.2 的"±1ms 是本仓既有口径"经核实是错的（既有用例断的是退避抖动区间）并就地改正。变异反验证由 6 条增至 **8 条且全部判红**：首版 N1/N6 是"编译失败"型（停在构建、跑不到断言）已换成可编译形态，N5 曾被判为等价变异、在补上取值断言后成为真判据。终态 `go build`/`go vet` 无输出，`go test ./core ./store/sqlite -race -count=3` 34.865s/12.951s，两条并发用例另压 `-count=50`/`-count=30` 全绿，全量 `-race -count=1` 五包全 `ok`。**应用户要求补做冒烟**：新增 `cmd/server/config_reload_smoke_test.go`（8 条用例，把 R01 的 `Diff` 与 R02 §10.1 那张表串成一条 miniature 重载链：读配置 → 分类 → 有拒绝项整次作废 → 先整批查表确认每条热更键都有入口 → 逐键调入口 → 成功后才推 `applied`；半途失败**刻意不回滚**，那是 R06 的交付物，本 harness 只报"哪条失败、已应用哪几条、`applied` 没动"；判据全部取行为——debug 写出、`RetryPolicyMaxDelay` 变 5s、留痕剪到 2、两张观测表各剪到 2 行，主用例并断 `applied_keys` **恰好等于表上八条热更键路径**），主用例还把 `hotEntries()` 的键名集合与那份列表**互检**（R03/R04 往表上加键却没有用例走到时先红）；八条针对冒烟具本身的变异（S1 级别入口空操作 / S2 不拦拒绝项 / S3 被拒也推 `applied` / S4 重启档当热更 / S5 缺入口被静默剔除 / S6 读配置失败当成无事发生 / S7 入口失败后接着应用后面的键 / S8 表上加一条没人走到的键）全判红、恢复后与备份逐字节相同，`go test ./cmd/server -count=3 -race -run TestSmoke -timeout 30m` 干净（3.226s）；另有进程冒烟：`%TEMP%` 隔离目录起 `server.exe`（存储与观测层路径用 `GODELAYQ_*` 指走，仓库 `data/` 一次没写），health 200、建单/列表 200、`jobs.json` 与 `observe.sqlite` 落地，**整份启动日志里 `reload` 出现 0 次**，即未接线状态下行为零变化。**冒烟具自身另过两轮 fresh-context 复核**（2026-10-03，判据全记在卡 §10.4 的"复核轮"与"复核轮的复核"）：第一轮 2 条 Important + 4 条 Minor——注释与实现相反（原写"失败键之前的键已应用"，而表上只有 `logging.level` 的入口会返回错误且它排最前，本表根本造不出半应用态；已改成如实交接给 R06，并补一条行为判据"失败之后的键确实没执行"+ S7 变异）、应用顺序是本 harness 的取法（I2 要 R06 按依赖顺序应用）、`reloadOutcome` 声称对齐 `ReloadState` 但后者**没有 `failed_keys`**（读失败那个 `<配置文件读失败>` 占位是会漏进 `/admin/runtime` 的假路径，R06 走 `error` 字段）、`wantApplied` 其实是第二份字面清单（补键名互检 + S8）、日志收集器换成带锁的 `logSink`（`store.flushLoop` 与两个观测写入器的 `onError` 跟主协程共用同一条 logger，裸 `bytes.Buffer` 是竞争点；此项属加固、无判据型用例）、"坏 YAML"措辞不准（那是合法 YAML 被 `UnmarshalExact` 按未知键拒掉）。第二轮 1 条 Important（README 的变异计数没跟着本轮走）+ 3 条 Minor（顺序推理只删了一半、"换依赖顺序不必改断言"说过头、新判据证明的是"那一步没跑"而非"跑了但用旧值"），全部处理完。终态复跑：`go build ./... && go vet ./...` 无输出、`go test ./... -race -count=1 -timeout 30m` 五包全 `ok`（api 132.682s / cmd/server 6.359s / core 12.168s / executor 22.797s / store/sqlite 4.146s）。R06 接线后要把 `applyCandidate` 换成生产重载链，场景清单留下 |
| R03 | 已完成（2026-10-03，实现记录见该卡 §10；已过两轮 fresh-context 复核，第二轮判"可以提交"、只留两条 Minor 措辞已收口）：**生产改动只有 `core/scheduler.go`**，新增 `Scheduler.ResizeWorkers(n) error` + 两个原子字段（`targetWorkers` 期望并发、`retireRequests` 退场名额）+ 私有 `shouldRetire()`（CAS 消费名额，`Add(-1)` 会被两个同时退场的协程减成负数）+ `worker` 循环开头的 `!exec && shouldRetire()` 判定 + `Start` 两处（`retireRequests.Store(0)` 与既有 `stopCh` 复位并排、`targetWorkers.Store(workers)`）+ `RuntimeStats.Workers` 改读 `targetWorkers`。**温和缩容按拍板落地**：不强杀、不取消在途任务，多出来的 worker 跑完手上那条、回到循环开头时自己退；代价是闲着停在 `select` 上的多余协程既不检查名额也不退出，读数（期望并发）与实跑协程要等任务重新流动才收敛（卡 §10.2 第 13 条把注释口径改正过）。**七处偏离卡面**，四条是抓到卡面前提不成立并就地记下：§5.5 第四条让用 `runtime.NumGoroutine()` 差值 ±2 并引用 `core/scheduler_exec_class_test.go:430` 作先例，**那条引用是反的**（该注释说的正是"卡片允许两种写法，这里选了另一种"，因为同包在途协程会让进程级计数在本机 `-race -count=5` 下 ±1 抖动、实测翻红过），换成调度器自己的判据（闸门法 + 终局并发峰值）；卡 §3.1 说 `targetWorkers` 是"执行协程在循环开头读它"，实际执行协程读的是 `retireRequests`；卡 §5.3 示例在四条任务都返回之后等"正好 1"，那时在跑数量停在 0，等待必然不到判据，改成让第二阶段的任务也卡在另一道闸门上；**卡 §3.2 只列了 `Start` 一处写 `targetWorkers`，实际还要 `NewScheduler` 与 `SetConcurrency` 各补一处**，否则未启动的调度器 `Workers` 报 0，而 `core/runtime_stats_test.go` 与 `api/handlers_admin_test.go` 两条既有用例断的就是未启动状态（本卡不许改既有用例）。另三条是口径选择：运行期扩缩**不回写** `s.concurrency`（那一份是启动期快照，`queueCapacity<=0` 时通道容量回退取的就是它，改了会报出通道并不具备的容量），未启动分支才回写；两条卡面没有的用例（`StopWaitsForResizedWorkers` 用"闸门还关着时 `Stop` 不得返回"直接判 `wg` 记账，`ExecutorPoolNeverRetires` 判 `!exec` 那一半）；§5.4 多加"前提凑数"与"通道没被换"两条断言。**复核轮**（fresh-context 复核判"仍需返工"，1 条 Important + 3 条 Minor，全部处理）：初版把变异 M8（退场判定挪到 `executeJob` 之后）记成**等价变异**，这条记录是错的——复核给出"余额悬着时有新协程进来"这一形状，据此补第十二条用例 `…_RetireCheckPrecedesNextPickup`，M8 变成只被这一条判红、其余 11 条全绿；补用例的首版又把 `defer unblock()` 注册在 `defer scheduler.Stop()` 之前，失败路径上后进先出会先 `Stop`、等一个卡在被关闭闸门里的 worker，整包撞到 5 分钟超时才失败——**这个次序错误只有跑变异才暴露得出来**；另三条 Minor 是三处注释口径（把用例构造写成通用性质、"在途任务跑完才收敛"、"立刻看到已关闭的 stopCh"）。**验证**：12 条 `TestResizeWorkers_*` 全 PASS、`go test ./core -race -count=5 -timeout 30m` 61.689s 无 flake、`go build ./... && go vet ./...` 无输出、全仓 `go test ./... -race -count=1 -timeout 30m` 五包全 `ok`（api 108.962s / cmd/server 6.326s / core 13.117s / executor 22.410s / store/sqlite 3.752s）；**八条变异 M1~M8 全部判红**，还原后终态 `core/scheduler.go` 76721 字节（sha256 前缀 `235a09ed`）、`core/scheduler_resize_test.go` 30958 字节（前缀 `dcd9dedd`）。`SetConcurrency`/`SetQueueCapacity` 的 warn-and-ignore 口径一字未改，既有测试文件断言零修改即通过。行为零变化：`ResizeWorkers` 至今**零生产调用方**（接线在 R06），`grep -rn ResizeWorkers --include=*.go` 只剩定义与用例。新缺陷四条全部**登记不修**并写明归属：**D1** 热更出来的并发数不跨进程内重启（`Start` 会按启动期快照重新覆盖 `targetWorkers`）归 R06 重放；**D2** 放锁之后 `go` 语句执行之前若另一条协程做完 `Stop→Start`，新协程会领到上一代的 `workCh`、等新一代的 `stopCh`，把那次 `Stop` 的 `wg.Wait()` 拖住——今天触发不了（**生产进程 `cmd/server` 只 `Start` 一次**；`examples/demo1:29`/`examples/demo2:41` 各是独立 main、也只 `Start` 一次且都不接 `ResizeWorkers`，不要把这条口径写成"全仓唯一 Start 调用点"），真要支持关停与重启并发得把停止信号成对传进 `worker`（那会改卡 §3.2 定死的签名）；**D3** 只有下限没有上限，热更 `scheduler.workers: 100000` 会立刻起十万条协程，归 R06 在重载链补上界校验；**D4** 温和缩容期间读数与实跑不一致，按设计保留，归 R07 使用者文档。未覆盖项：执行器池的运行期扩缩（设计 §10 N1）、D1/D2 的交错、`retireRequests` 恒非负只有 CAS 保证而无专门用例、`/pools` 与 `/admin/runtime` 的 `workers` 端点级用例（本卡不改 api） |
| R04 | 已完成（2026-10-03，实现记录见该卡 §10；**分两半落地**并过了一轮 fresh-context 复核 + 修复轮，复核判"仍需返工"的 1 条 Important + 3 条 Minor 全部处理）：`executors.commands` 现在可以运行期整表换。**登记表侧**：`Registry.ApplyConfig(profiles []*Profile) error` 与既有 `ApplyStore` 镜像对称（换掉 config 那一批、store 那一批原样保留，同样一次 `newSnapshot` + 一次 `Store`，被拒的那批一字不动现表）；卡 §3.1 预留的 `commands []core.ExecutorCommand` 参数确认**没有读取点**、按 YAGNI 删掉，探测改在方法内部现调 `Probe(profile)`（与 `NewRegistry` 同一条口径，`Applier` 那侧只为写 warn 日志再探一次，两处同一个函数不会分叉）。**一条卡面没写的决定**：降级展示面跟着进来的那一批重算——被 config 顶掉的 store 条目只在"顶它的那条还在这批里"时继续降级，那条件消失时它既不回生效表也不留在降级面，**复活需要一次 store 路径的 `Apply()` 重读文件**；这是设计 §12 那条风险的既定落点（若不做这条，同一批重复应用会让降级计数在 1/0 之间来回跳，"可重入"就破了）。**同步器侧**：`Applier` 新增 `configCommands` 记账（受既有 `writeMu`、只在锁内读写、`NewApplier` 从 `registry.executors.Commands` 起步）、`executorsNow()`（非 `Commands` 恒取启动期冻结值、`Commands` 换成当前列表）、`ApplyConfig(candidate core.Config) (ApplyResult, error)` 四步固定顺序（enabled 判 → 用**冻结值 + 新列表**合成配置走 `LoadProfiles` 严格模式 → 逐条探测只为 warn → 锁内整表替换 + 两批全部重登记 + 摘掉不再存在的 `exec.` 键 + **最后**才推进记账），共享片段 `syncHandlers(reRegister, live)` 把 `Apply` 原有的 `keepConfig` 折进入参而不是复制一份差集逻辑（`Apply` 没有改成调用 `ApplyConfig`：两者范围不同，合并会让 store 路径意外摘掉 config 键位）；`Validate` 仍在锁外读冻结值，理由写进方法注释（它只看许可字段、那些永不热更，且 `writeMu` 是写链的锁）。**装配侧一处改动**：`Applier` 的构造条件放宽成 `web_enabled || (reload.enabled && executors.enabled)`，`web_enabled=false` 那一支用新的 **`NewConfigApplier`**（不接 store ⇒ 档位文件一次都不碰，父目录也不建；那种部署里 `Apply()` 返回明确错误而不是 nil panic），`profileStore` 的打开条件一字未改 —— 这就是拍板 P2"web 关闭时不打开档位文件"的落地形状。**两处卡面前提不成立**：**D-R0401** 卡 §5.2 建议用"放宽顶层 `workspace` 后新档位越出冻结 workspace"当兜底判据，核实 `resolveInside` 对绝对路径/`..` 的拒绝发生在拼接 workspace **之前**，换 `workspace` 取值换不出任何一条相对写法的合法性变化，真正能区分"用的是冻结值"的维度只有 `runtime_allow`，用例据此落在 `runtime`（并先自证放宽值本身过得了 `LoadProfiles`，否则判据空转）；**D-R0404** 设计文档 §9 与 §12 都写"用冻结的 `workspace`/`runtime_allow`/`env_allow` 做 `LoadProfiles` 越界拒绝"，实情是**全局 `executors.env_allow` 从不进那次校验**（唯一读取点是执行期的 `executor/env.go` 的 `BuildEnv`；进校验的是每条档位自己的 `cmd.EnvAllow` 名单），兜住它的实际机制是"冻结值随 `executorsNow()` 钉进处理函数闭包"，代码注释已按实际机制改写、设计文档同步留给 R07。**D-R0103 的答复**（R01 派给本卡）：`LoadProfiles` 会拒空名与重名，所以 `ApplyConfig` 收到的档位永远有名字且互不相同，`#<i>` 那种形状只是 `core.Diff` 侧的展示塌缩、不会作为 `*Profile` 进到这一层 —— **登记不修**归 R06（接线时 `Diff` 的 `#<i>` 只作日志与拒绝档判据，生效判定以本卡的 `LoadProfiles` 为准）。另两条缺陷：**D-R0403** nil-store 部署里 `Apply()` 恒返回错误（P2 拍板的结果，要重放 store 侧得先打开 `web_enabled`）登记不修归 R06。新增 18 条用例（`executor/registry_apply_config_test.go` 7 条 + `executor/applier_config_test.go` 11 条，含两批共存的三步用例、与重启等价用例、nil-store 用例、并发 20 轮的终态自洽用例），`cmd/server` 侧修复轮补了 `TestRun_ReloadWithoutWebBuildsConfigOnlyApplier` 证明放宽那一支的真实形状（档位存储闭包一次都没被调用、服务拿到的 `profileStoreAPI` 是 `store=nil` 且 `applier≠nil`、config 侧档位照旧注册）；既有 `TestApplier_*` 与 `cmd/server` 既有用例**断言一字未改**即通过，`applier_test.go` 的 diff 纯增量。**七条变异 M1~M7 全部判红**（M1 用整份 candidate 校验 / M2 不重建 config 闭包 / M3 摘除去 `core.ExecPrefix` 判据 / M4 失败批次也提前推进记账 / M5 nil-store 走 panic 而非报错 / M6 `Apply` 仍读启动期列表 / M7 构造条件退回只看 `web_enabled`），M1 在修复轮的新基线上又重跑过一次、仍在 `executor/applier_config_test.go:179` 判红，全程二进制 I/O 还原、终态 `executor/applier.go` sha256 前缀 `0d1de289`、`cmd/server/main.go` 37961 字节回到 `f09ef2b7`。终态验证：`gofmt -l` 对本卡六个改动文件无输出、`go build ./... && go vet ./...` 无输出、`go test ./cmd/server -run 'WebEnabled|Profile|Reload' -v` 14 条全过（**卡 §7 那条 `WebEnabled|Profile` 过滤器选不中新用例，要带 `Reload`**）、`go test ./executor -race -count=5 -timeout 30m` 三次复跑 105.4s/108.1s/109.9s 全干净、全仓 `go test ./... -race -count=1 -timeout 30m` 五包全 `ok`。行为零变化：`ApplyConfig` 与 `NewConfigApplier` 至今**零生产调用方**（触发链在 R05/R06），且 `reload.enabled` 默认关 ⇒ 构造条件与改动前等价；`api` 一字未改，写端点在 nil-store 部署仍回 503（`requireExecutorProfiles` 先闸 `WebEnabled()`、再成对判 store 与 applier）。未覆盖：真起进程跑一次"文件变了自动走到 `ApplyConfig`"（R05/R06）、`ApplyResult.Warnings` 在 config 侧恒空因而设计上没有用例、探测"不可用"分支只走日志（`selfExecutable` 恒可用，硬造会引入本机依赖） |
| R05 | 已完成（2026-10-03，实现记录见该卡 §10；**过了两轮 fresh-context 复核**，第二轮判"仍需返工"的 2 条 Important + 8 条 Minor 全部处理并留档）：**纯新增两个文件**，`core/watch.go` 482 行 + `core/watch_test.go` 959 行，全仓没有任何调用方（`grep -rn ConfigWatcher --include=*.go` 只命中定义、用例与 `core/config_reload.go` 的一句注释）。**落地形状**：`ConfigWatcher` 盯**目录**、按文件名**磁盘拼写**精确匹配、不筛事件类型、防抖形状照 `core/load.go` 的 `pendingLoads`；状态由 `atomic.Pointer[ReloadState]` 一份换指针，`WatcherError`/`WatchedPath` 只由 watcher 写（要求 10），`ReloadFunc` 交回的其余字段逐字段原样存（要求 9）；`Run` 有 ctx 与 `stopCh` 两条停止源，两条退出路径的收口共用同一个 `stopTimers` + 关 fsnotify 句柄（defer）；关停用 `loopDone`/`started` 两个位点做结构化证明而非 `sync.WaitGroup`（草图那形状 `wg.Add` 在 `go Run` 的协程里会与 `Close` 的 `Wait` 竞态），`Close` 用 `waitForLoop` 有界等 5s、**不强杀也不等待在途重载**。**三处卡面前提被核实不成立并就地记下**：§3 的 `ReloadFunc` 契约注释写"watcher 的事件循环串行调用它"**是错的**（调用点是 `time.AfterFunc` 的计时器协程，两个窗口到期可以重叠），实际由 `callReload` 持 `reloadMu` 串行；§9 风险表把 ReloadFunc panic 的站点写成"事件循环里 `defer recover()`"**也是错的**（recover 挂在循环上捕获不到，未捕获的 panic 崩的是整个进程——变异 5 的栈就是 `created by time.goFunc → schedule.func1 → trigger → callReload`），已改为 `callReload` 内 recover；§3 自身的 `Run` 注释"等事件循环与在途重载收尾"与 `Close` 注释"不强杀在途重载"矛盾，取后者并写进契约（调用可能在 `Run`/`Close` 返回之后才结束）。§6 第三条的 `runtime.NumGoroutine()` 差值判据按 R03 同一条教训换成结构化判据（同包在途协程会让进程级计数在本机 `-race -count=5` 下 ±1 抖动）。**两处本卡独有的静默失效被补掉**：`basename` 若取入参拼写，Windows/macOS 默认卷上 `-config ...\Config.yaml` 会 `os.Stat` 成功、watcher 建得出来、`State()` 一切正常而监听器永不触发（改为构造期 `resolveDiskFileName` 解析目录真实拼写，解析不出来当部署级问题报构建错误）；同一 watcher 第二次 `Run` 会对已关闭的 `loopDone` 再关一次而 panic（改为 `started.CompareAndSwap` 只认第一个进入者、后进者静默返回）。**变异反验证 14 条全部判红**（1–6 是首轮那六条在返工后的**新字节**上重跑，判红用例与结论不变；7 去 `reloadMu`／8 去 `Run` 的停表 defer／9 去 `Run` 的关句柄 defer／10 `storeState` 抹空 `Error` 与三份键清单与 `LastAppliedAt`／11 去第二次 `Run` 守卫／12 `basename` 不走磁盘拼写解析／13 去 `trigger` 的 `closed` 短路／14 `handleEvent` 认出目标却不排期，最后这条专门接住"监听器整体失灵"在 `IgnoresOtherFiles` 正向对照上判红）；**变异 3 的判红点本轮搬家**：`Close` 与 `Run` 共用 `stopTimers` 之后，"Close 忘掉停表"在 `CloseIdempotent` 上**变成绿灯**（被别人的 defer 兜住），因此新增 `CloseStopsTimersWithoutLoop`（整场不启动 `Run`）接管判红——这条记为 D-R0505，教训是"两处共用一份清理"会让既有用例的判据失效，改实现要连着复判旧变异。全程二进制 I/O 变异与还原，每个变异后断言指纹、14 次全部一致，终态 `core/watch.go` sha256 `342463c8…`、`core/watch_test.go` `4a28a629…`。**验证**：`go test ./core -run TestConfigWatcher -count=1 -v` 20 个 Test 函数 = 19 PASS + 1 SKIP（要求 7 的 Chmod，Windows 无 POSIX chmod；另有两条大小写用例在敏感卷上会 `t.Skipf`/`t.Logf` 早退，本机 Windows 实跑通过），`go test ./core -race -count=5 -timeout 30m` 102.682s 无 flake、`go build ./... && go vet ./...` 无输出、全仓 `go test ./... -race -count=1 -timeout 30m` 五包全 `ok`（api 219.930s / cmd/server 6.479s / core 21.709s / executor 25.119s / store/sqlite 3.744s）、终态 `sha256` 与变异备份一致；`api` 的 wall 四次读数 124.196s→232.994s→128.744s→219.930s，随负载浮动 2 倍，卡 §10.3 只把它当"本次真实输出"而不当基线。**这组数字是重跑过的**：一度把逐个改写 `core/watch.go` 的变异反验证与后台全量验证并行跑，无法证明那次编译读的是终态字节，于是在干净字节上把 `gofmt`/单包/`build+vet`/`core -race -count=5`/全量重跑一遍并改记读数，流程缺陷登记为 D-R0515（已修，变异与全量验证此后串行）。本卡不起任何进程（没有装配点就没有构造入口），"改一次存盘看进程是否按防抖重载一次"的端到端实测归 R06 接线 + R07。**新缺陷登记**：实现侧四条 D-R0504（重载未串行 + 契约注释与实现相反）、D-R0506（ctx 退出路径不停表不漏句柄）、D-R0507（大小写拼写静默失效）、D-R0508（第二次 `Run` panic）全部**已修**；测试判据侧六条 D-R0501/D-R0505/D-R0509/D-R0510/D-R0511/D-R0513（要求 3 首版只写成口头论证、现补 `TriggersOnIdenticalContent`）、文档侧一条 D-R0512、流程侧一条 D-R0515，同样**已修**；携带项四条全部**写明 R06 的具体动作**——D-R0502（`/admin/runtime` 不要直接 marshal `core.ReloadState`，两个时间字段自己格式化、判"从未尝试过"一律用 `Result==""`、要有一条用例覆盖"起来就一次都没重载过"的读数形状）、D-R0503（`reload.enabled` 读数取进程侧配置而不是 `State().Enabled`）、D-R0514（`State()` 是浅拷贝，三份切片与交回方共享底层数组，读侧不许就地排序或改写）、D-R0102（两份入参须已 `Normalized()`，本卡在 core 内无法自检）。未覆盖项如实列在卡 §10.6：真实 watcher 死亡路径、error 通道 `!ok` 不退出那一支、"两个防抖窗口自然到期重叠"的真实形状、跨 watcher 实例的调用顺序、"精确相等优先"那一支需要敏感卷、句柄关闭只验到 API 层、Linux/macOS 未实跑 |
| R06 | 未开始 |
| R07 | 未开始 |
