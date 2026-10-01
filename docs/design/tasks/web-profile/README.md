# 档位在线管理任务卡（TASK-W01 … W09）

设计说明在 `../../web-profile-design.md`，本目录把它拆成可逐个执行的卡片。
卡片只写"做什么、怎么算做完、怎么验"，设计理由不重复。

一句话概括这个系列：把 `executors.commands` 那张列表从"只能写在 yaml 里、改完重启"
变成"页面上可增删改、改完立即生效、重启后仍在"。

三条系列级口径，任何一张卡都不许松动：

1. **一份规则**（设计文档 D3/I1）：页面写入用的校验与探测必须是 `executor` 包那一份，
   不允许出现"页面上宽松一点"的第二套规则。分叉的表现是"保存成功、重启后启动失败"。
2. **落盘先于生效**（I2）：先写文件成功，再动注册表。写盘失败时注册表与调度器都不动。
3. **默认关闭**（D2）：`executors.web_enabled: false` 时不建文件、不注册端点行为、
   不注入，全仓行为与本系列之前逐字节一致。每张卡的 DoD 都要有一条用例守住这条。

## 执行顺序

| 阶段 | 任务 | 内容 | 该阶段结束时可演示的能力 |
| --- | --- | --- | --- |
| M0 基础 | W01 | `executors.web_enabled` / `profiles_path` 两个配置键与 `core` 档位存储 | 能读写 `exec-profiles.json`，损坏文件会报错，行为零变化 |
| | W02 | `executor` 构造入口的严格/宽松两模式 | 能对内存里一条档位定义做与启动期同一套校验 + 探测 |
| M1 可变注册表 | W03 | `executor.Registry` 改为可变（整表原子替换）+ `source`/`degraded` | 运行期能换掉整张档位表，读侧看不到半成品 |
| | W04 | `core.Scheduler` 的 `UnregisterHandler` 与 `PauseByHandlerKey` | 能摘掉一个任务类型并把它的未终态任务置 `paused` |
| M2 接线与端点 | W05 | 启动合并 yaml ∪ 文件、冲突降级、装配注入 | 页面建的档位活过重启 |
| | W06 | 三个写端点 + 鉴权 + 审计动作 | 不重启就能建/改/删档位，任务立刻能提交 |
| | W07 | `GET /executors` 扩展来源与可编辑标记 | 接口说清每条档位从哪来、能不能改、当前能不能跑 |
| M3 界面 | W08 | 控制台档位管理页 + 能力表 + 文案 | 界面上建/改/删各跑一遍，低档位看不到入口 |
| M4 收口 | W09 | 文档同步、全量验证、待做项登记 | 四份使用者文档与设计一致 |

依赖关系（与各卡片头部的"依赖任务"一致，冲突时以卡片为准）：
W01→W02→W03→W05 顺序做；W04 独立于 W01-W03，可以并行；
W06 需要 W03、W04、W05；W07 需要 W03（可与 W06 并行）；W08 需要 W06、W07；W09 需要全部。

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
- **本系列会新增两个配置键**（`executors.web_enabled`、`executors.profiles_path`），
  必须同时改 `configs/config.example.yaml` 与本机 `configs/config.yaml`（后者被 `.gitignore` 排除），
  否则 `go test -run TestExampleConfigMatchesLocal ./core` 失败（本机没有 `config.yaml` 时该测试是
  跳过而不是通过，记录时要分清）。另注意两个易混机制：YAML 未知键被拒来自 `UnmarshalExact`；
  `core/config.go` 的 `BindEnv` 字符串列表漏项**不会报错**，只会让该项无法用 `GODELAYQ_*` 覆盖。
- 依赖方向红线：`core` 不许 import `executor`（档位记录结构放 `core` 正是为了这条）；
  `api` 可以 import `executor`（现状已如此）。
- 构造函数命名一律 `New*`（`Open` 属既有例外名单）。
- 涉及端点的卡片：`api/audit.go` 的 `auditActions` 映射必须同步加行，
  并用一条用例证明"漏配时落进 `other` 而不是静默丢行"（`api/audit.go:260-270` 的既有口径）。
- 涉及前端的卡片：`cd web && npx vue-tsc --noEmit` 与 `npm run build` 必须通过。图标只用 lucide。
  `web/src/composables/usePermission.ts` 的能力表与后端路由档位必须一起改。
- 界面文案改动要同步 `web/src/content/job-template.md`（那份是任务模板文档，
  现在写着"改 `executors.commands` 需要重启进程，页面上不能新增或编辑档位"，W08/W09 要按新事实改）。
- 每张卡片一个提交（改动跨面大时可按"新增文件 / 接线 / 测试"拆 2-3 个），
  提交信息用 Conventional Commits，前缀按内容选 `feat` / `fix` / `test` / `docs` / `refactor`。
- 在共享 worktree 里不要直接 `git stash`；需要暂存时用带名字的 tag。
- 冒烟一律在系统临时目录里造独立的配置 + 数据目录 + 产物目录，跑完删除；
  不得写入仓库的 `configs/config.yaml` 与 `data/`。执行器相关冒烟需要凭据，
  用 `GODELAYQ_SERVER_PORT` 挑一个 IPv4/IPv6 都空闲的端口。
- 收口卡（W09）要跑：`go build -tags dashboard ./cmd/server`、三条交叉构建、
  `go test ./... -race -count=1`，并把"受平台限制没跑的项目"逐条列清而不是含糊带过。

## 系列级待做项（本期只登记，不在任何一张卡里做）

设计文档 §7.2 的五条：S-1 路径白名单（生产部署前必须补）、S-2 细粒度授权与档位改动专列、
S-3 `GET /executors` 的绝对路径对 viewer 遮蔽、S-4 脚本文件管理、S-5 页面上编辑 `env` 取值。
W09 负责把它们逐条落成文档里的登记项，并核对 §7.1 那张偏离表与代码现状一致。

## 待拍板（写卡时按推荐值落的，执行前请复核）

| # | 问题 | 卡片采用的答案 |
| --- | --- | --- |
| P1 | store 文件里某条与 yaml 同名（人后来改了 yaml）：启动失败还是条目降级？ | 降级，config 赢（设计文档 §5.2、W05 §3） |
| P2 | `exec-profiles.json` 损坏：拒启动还是忽略继续起？ | 拒启动，照分组先例（设计文档 §6.5、W05 §4） |
| P3 | 界面落点：独立视图还是把设置页那段只读区块改成可编辑？ | 独立视图 `ProfilesView.vue`，设置页留只读概览与入口（设计文档 §12、W08 §3.1） |

## 状态

| 任务 | 状态 |
| --- | --- |
| W01 | 已完成（2026-10-01，实现记录见该卡第 10 节；六处偏离含"存储类型名带 `Executor` 前缀"、"**新增一条配套校验：`web_enabled: true` 必须同时 `enabled: true`，设计文档 D2 的方向因此改了**（W09 原场景 1 拆成 1a 拒启动 / 1b 双关 503）"、"`Command()` 返回 error 且顺带拒负数 timeout"、"List 只拷结构不拷嵌套切片，登记为 D-0101"；守卫 `TestExampleConfigMatchesLocal` 实测是 `--- PASS` 而不是跳过；冒烟证实 `web_enabled: true` 时进程连档位文件的父目录都不碰） |
| W02 | 已完成（2026-10-01，实现记录见该卡第 10 节；六处偏离含"`resolveAny` 落地叫 `resolveAnywhere`"、"相对写法在两种模式下都以 workspace 为基准"、"错误前缀按来源分（配置侧保留 `executors.commands[i]`，单条侧改给 `profile "name"`）"、"`BuildProfile` 自行调 `Config.Normalized`"；**抓到卡面前提错误一条并就地改掉**：`relativeTo` 只在 Windows 跨盘符时兜底成文件名，同盘越界会给 `..\..\` 上跳形式，所以 W07 的 `path_display` 判"在不在 workspace 内"只能用 `withinDirectory`（D-0201）；真实进程冒烟证实 yaml 侧越界拒绝一字未松，宽松模式没有调用方、端到端留给 W05/W06；Linux/macOS 未实跑） |
| W03 | 已完成（2026-10-01，实现记录见该卡第 10 节；六处偏离含"`ApplyStore` 收 `[]StoreEntry` 而不是两个平行参数"、"**降级条目不进 `entries`，改成 `snapshot.degraded` + `Degraded()` 访问器**（注册键是 map 的键，同名两条无法共存；设计文档 §5.2/§6.3 与 W07 §3 已同步，W07 输出时降级那批的 `key` 与生效那条相同）"、"source/degraded 不放 `Profile`，靠 `SourceOf()` 反查"、"执行器关闭时 `ApplyStore` 直接报错而不是收下空表"、"`Degraded` 标志只在真撞名时有意义（W05/W06 别拿它表达'我不想注册它'）"、"并发用例的轮次标记改用 `Timeout` 并加断言 store 键数只能是 0 或 2"；**反向验证做过**：临时改成原地先删后加，`-race` 报 8 条 DATA RACE（读 `Profiles()` registry.go:282 vs 写 `ApplyStore` registry.go:225），撤销后绿；`api/` 包零改动，既有登记表用例未改预期即通过） |
| W04 | 已完成（2026-10-01，实现记录见该卡第 10 节；偏离含"测试文件名按仓库下划线体例落 `scheduler_hot_registry_test.go`"、"**逐条处置复用 `Pause` 而不是直接调 `pausePendingJob`**（多带 `ErrJobNotPending`/`ErrJobNotFound` 两个哨兵判定，代价是"已 paused"必须由状态过滤先拦，否则重复调用会重复计条数）"、"空键判定用 `strings.TrimSpace`，比 `RetagGroup` 只拒空串更严"、"卡 §5.2 的重启断言改用真实 `core.JSONFileStore` + `Restore`（mock 落不了盘）"、"`core/scheduler_test.go` 零改动，helper 够用"、"卡正文行号是动笔时的位置，落地后整体下移，现行位置记在 §10.1"；**反向验证做了三处变异**：只删一张表 → 成对删用例变红；状态过滤去掉 `paused` → 幂等与混合两条变红；逐条处置换成 `ForcePause` → "运行中跳过"那条变红（正是 §9 风险表点名的哨兵）；`go test ./core -race -count=3` 无 flake，全仓 `-race` 全绿；真实进程冒烟（`%TEMP%\w04smoke`，跑完已删）**只证明回归**——本卡两个方法都还没有调用入口，现场走查留到 W06 §7；冒烟里 `exec.hello_demo` 跑通、HTTP 暂停的任务硬杀重启后 `paused:1 / heap_size:0`、`resume`→`cancel` 干净；未覆盖：纯内存部署不暂停（与 `RetagGroup` 同口径）、摘除 handler 后 HTTP 提交口的表现归 W06） |
| W05 | 已完成（2026-10-01，实现记录见该卡第 10 节；七处偏离含"**合并函数落地叫 `MergeStoreProfiles(cfg, stored) ([]StoreEntry, []ProfileWarning)`，只管 store 侧**"（`MergedProfile` 与 W03 的 `StoreEntry` 同形；再跑一遍 `LoadProfiles` 等于把 config 的连坐规则搬进 store 路径；store 侧不连坐所以没有 error 返回值）、"`NewRegistry` 签名没动，合并结果经 `ApplyStore` 进表（与 W06 的运行期写入共用同一个入口和同一套拒绝规则）"、"**卡 §3.6 的 api 注入本卡没做**：没有读取方就不先注入，W06 要自己把活的存储实例建到 `run()` 里（本卡闭包只交回记录）"、"依赖形状是读记录的闭包不是构造 store"、"顺序断言改用替身的调用流水（`register:<键>` < `restore_guard` < `start`）"、"`web_enabled=false` 那条断言升级为'目录都没被创建'"、"三个新文件按仓库体例命名"；**反向验证三处变异**：守卫挪到注册之前 / `Degraded` 恒 false（四条用例红，cmd/server 那条表现为启动失败）/ store 侧改成连坐，各自变红后恢复；真实进程冒烟跑通"`smoke_py` 由手写文件注册并跑成 python 输出、同名的 `smoke_cfg` 跑出来是 node 输出（config 赢，端到端证据）、缺失脚本那条 `runtime_ok=false` 且提交期 400、`total=3 registered=3 unavailable=1 degraded=1`；坏文件挡住启动且 error 日志含路径与 `delete it` 指引；`GODELAYQ_EXECUTORS_WEB_ENABLED=false` 时坏文件与进程无关且不建目录"；新缺陷一条 **D-0501（store 侧探测失败没有逐条 warn，只有汇总计数）登记不修**，理由与归属见卡 §10.6 |
| W06 | 已完成（2026-10-01，实现记录见该卡第 10 节；八处偏离含"**卡 §3.2 的三个窄接口落地成两个**：`applier` 与 `registrar` 合并成新类型 `*executor.Applier`（`Validate` + `Apply`），五步顺序与 I2 回滚只存在一处，`api` 不直接调 `RegisterHandler`/`UnregisterHandler`，只有 `PauseByHandlerKey` 由删除端点直接调"、"**DELETE 三步顺序改成'钉任务 → 删文件 → 生效摘 handler'**（卡面顺序里'摘成功、删失败'会留下'表里有、进程里跑不了'，正是 I2 要防的方向）"、"POST 成功是 201 不是 200"、"`?jobs=block` 自己按存储数 pending/running/paused 三档，响应把 `already_paused_jobs` 单列（`PauseByHandlerKey` 的返回值只含新钉住的 pending）"、"**W05 那条依赖换了形状**：`newExecutorProfiles`（交回记录）改成 `newExecutorProfileStore`（交回活的实例），两份内存视图合成一份；两个依赖经 `profileStoreAPI` 打包成 `newServer` 第 7 参数（照 `observabilityAPI` 先例）"、"请求体解码不用 `ShouldBindJSON`（gin 不拒未知键），改 `json.Decoder` + `DisallowUnknownFields`"、"PUT 时 `name` 以路径写法为准"、"卡 §5.2 的 python 档位改用本机一定有的外壳，python 那条留给 §7 冒烟"；**变异反向验证三处**：回滚调用短路 → 生效失败回滚用例红；删除跳过 `PauseByHandlerKey` → 删除语义用例红；POST 摘掉 `RequireRole(RoleOps)` → 门禁用例三条 403 变 409；真实进程冒烟（`%TEMP%\w06smoke`，跑完已删）跑通"operator 403 → ops 建 `smoke_py` 201 且响应无 env 取值 → **不重启提交并跑成 success 读到 python 输出** → PUT 换 script 400 / 换 timeout 200 → block 409 与 `jobs=whatever` 400 与默认删除 200 `{paused_jobs:1}` → `resume` + 提前 `trigger_at` → 判 failed 且日志 `no handler registered for job` → 三个动作各查到台账行含被拒的那几条"；新缺陷三条 **D-0601（设计文档 §5.3 表格与同节第 2 条自相矛盾）已修**、D-0602（页面写入与手改文件之间没有乐观并发）与 D-0603（每次写全表重建）登记不修，理由与归属见卡 §10.5） |
| W07 | 已完成（2026-10-01，实现记录见该卡第 10 节；八处偏离含"**`toExecutorProfile` 换成收一个 `executor.ListedProfile`**，因为按卡面"多收两个入参"的写法每行要连调三次访问器、每次各读一张表，而 W06 之后运行期真会整表替换 → 新增一次读表的 `Registry.List()`（生效 + 降级合并、按键排序、同键时生效在前）"、"**`editable` 落在 executor 侧算**（DoD"只在后端一处计算"由位置保证，并得到"写响应与列表行是同一个结构体"这条更强判据）"、"降级那条的 `reason` 用冲突说明、`runtime_ok` 恒 false，即便它指向的脚本其实存在"、"卡 §5.1 的"`LookupHandler` 查不到"换成 `SourceOf`/`Lookup` 两条等价判据（api 层看不到调度器的处理函数表）"、"**卡 §5.4 的往返用例前提不成立并改写成三段**：响应体从来不是请求体的子集（`key`/`runtime_ok`/`reason` 等只展示键早就存在，而 `runtime`/`script`/`args_render`/`env` 这些定义字段一个都不在响应里），加上 W06 拒一切未知键，那种发回必定 400；改成"新键不撞请求体键名 + 写响应与列表行逐字段相等 + 列表行当请求体必须 400""、"既有断言动了一处（`TestListExecutors_DefaultStateIsNotAnError` 的整对象 `JSONEq`，两个新顶层键关闭时也要给出，期望值只能跟着长）"、"设计文档 §6.8 的 `editable` 公式补上 `!degraded`"、"`path_display` 多一种省略情形（program 写成 PATH 程序名）"；**变异反向验证四处**：`editable` 不看 `web_enabled` / `PathDisplay` 信 `ScriptRel` 形状（越界那条真给出 `../elsewhere/...`）/ `List()` 不合降级条目 / 降级改用探测结论，各自变红；`List()` 的一次读表有 20 轮 `List()` 与 `ApplyStore` 交错的 `-race` 用例；真实进程冒烟（`%TEMP%\w07smoke`，跑完已删）核对到五种状态（config 只读且没有 `path_display` 键、store 相对写法、store 越界给绝对路径且 `runtime_ok:true`、同一个 `exec.py_hello` 两行并存、顶层 `web_enabled` 与 `runtime_allow`），`viewer` 与 `ops` 看到完全相同的行（S-3 现状），`POST` 的 201 响应与列表行逐字段相等，`-tags dashboard` 的设置页渲染 6 行且控制台无消息；新缺陷三条 **D-0701（设计文档 §6.8 与卡 §3.1 的 `editable` 公式不一致）已修**、D-0702（`GET /executors` 说不出档位定义字段，响应不能当编辑表单的回填来源，归 W08 判）、D-0703（生产构建剥掉 Vue 警告，"无 console 消息"不能当 `:key` 的证据，记为验证口径限制）；`npx vue-tsc --noEmit` 与 `npm run build` 绿，Linux/macOS 未实跑） |
| W08 | 已完成（2026-10-02，实现记录见该卡第 10 节；**前置：先收掉 W07 登记的 D-0702**——新增 ops 档的 `GET /executors/profiles/:name` 把档位定义给回来（`env` 只给键名 `env_keys`，取值仍然不外露），配套 `PUT` 把"请求体没带 `env` 这个键"解释成"不改"、显式 `"env": {}` 才是清空；九处偏离含"**卡 §3.3 的字段表没有 `positional`，而 PUT 是整条覆盖**，表单改为只读回显 + 原样带回（代价：页面不能声明位置参数，登记 D-0801）"、"`env` 新增是整组替换而不是合并，取值不回显就无从合并（D-0802）"、"载荷按 `kind` 组装，不属于当前档位的键整个不发（依据 `checkFieldsMatchKind`）"、"列表行做成'整行按钮 + 行下方独立按钮'，因为 `<button>` 不能嵌 `<button>`"、"`editable=false` 的行也能点选，右侧退化成两种只读说明（配置来源 / 与配置同名）"、"http 那一组按 DTO 全量给 11 项，`headers` 用 `Name: v1, v2` 行文本，`deny_private_ranges` 做三态下拉对应 `*bool`"、"台账页动作常量 `AUDIT_ACTIONS` 补三项（设计文档 §6.7 归本卡，卡 §3.5 没列）"、"写响应直接当选中行，不再回列表找那一条"、"契约用例用三份样本而不是一份，因为一份载荷同时出现 `script` 与 `url_template` 不合法"；**变异反向验证三处**：样本键名改 `allowDash` / 样本删掉 `cwd` / 读口不置空 `record.Env`，各自变红；全仓 `-race` 绿（api 135.8s）、`vue-tsc` 与 `npm run build` 与 `-tags dashboard` 构建全过；REST 16 步 + 浏览器 10 步逐条实录（不重启建 `exec.ui_py` 并跑成 success、改 timeout 后 env 仍在文件里、换 kind 400、探测失败仍 201 且列表摊开原因、与 yaml 同名 409 且表单不关、删除弹窗三条说明 + 先 `block` 吃 409 再 `pause` 钉住那条成 `paused`、operator 无入口且 curl 四方法 403、设置页徽章与入口、台账选 `executor.profile_delete` 查到 5 行含被拒那几条），另用手写撞名文件重启验到 `exec.cfg_py` 两行并存与降级说明态；新缺陷三条 D-0801/D-0802/D-0803 均登记不修（理由见 §10.5）；⚠️ 冒烟目录被本机清理器回收一次，照原配置重建后全部步骤重跑；Linux/macOS 未实跑，binary 与 http 两种档位的表单只过了类型检查与契约用例） |
| W09 | 待执行 |
