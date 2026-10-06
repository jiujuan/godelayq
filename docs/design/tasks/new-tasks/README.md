# 任务名称/类型解耦与自由执行位置任务卡（TASK-N01 … N08）

设计说明在 `../../job-name-type-and-adhoc-execution.md`（下称"设计文档"，引用记作 `§D1`、`§5.4`），
本目录把它拆成可逐个执行的卡片。卡片只写"做什么、怎么算做完、怎么验"，设计理由不重复。

一句话概括这个系列：新建任务时**名字给人看、类型决定跑什么**，并新增四条内置的
"自由执行档位"（`exec.php` / `exec.python` / `exec.shell` / `exec.http`），
让脚本路径与请求 URL 在提交任务时给出。

四条系列级口径，任何一张卡都不许松动：

1. **不新造执行通路**（§D5）：adhoc 是档位，不是另一种任务。并发许可、超时合成与上限、
   产物落盘、失败分类、重试、崩溃恢复、输出掩码全部复用现成那一条
   （`executor/proc.go:65`、`executor/http.go:62`、`executor/register.go:87`）。
   出现"新建一个 handler 直接 exec.Command"就是跑偏。
2. **不复制校验规则**（§D3、§D7）：名称规则、路径规则、URL 规则都只有一份实现，
   放在 `core` 或 `executor`；前端那份只是提前提示一次，拒绝权在服务端。
   两份规则迟早分叉，分叉的表现是"页面上填得进去、提交后 400"或反过来。
3. **兼容双写法**（§D4）：请求体带 `type` 才套新语义；不带 `type` 时行为与本系列之前逐字一致。
   每张涉及创建流程的卡都要有一条用例守住"旧写法仍然可用"。
4. **默认关闭**（§D11）：`executors.adhoc.enabled: false` 时四条内置档位一条都不注册，
   接口与页面看不出任何变化。每个新增面的 DoD 都要有一条用例守住这条。

## 执行顺序

| 阶段 | 任务 | 内容 | 该阶段结束时可演示的能力 |
| --- | --- | --- | --- |
| M0 语义 | N01 | `core.ValidateJobName` 名称规则 | 有一个能判"中文/字母/数字、1..64 字符"的函数与用例，行为零变化 |
| | N02 | api 名称/类型解耦、`?type=` 筛选、门禁改按注册键 | 同一份 yaml 起服务，用 `{name:"每晚对账", type:"payment_check"}` 能建任务；旧写法仍能建 |
| M1 配置与档位 | N03 | `executors.adhoc.*` 配置节 + 两份 yaml + 热重载归类 | 配置能写这一节、非法取值被 `Validate` 拒、关闭时行为零变化 |
| | N04 | `executor` 构造与注册四条内置 adhoc 档位 | `executors.adhoc.enabled=true` 时 `/job-types` 里出现四个键，撞名时内置让位 |
| | N05 | 提交期路径/URL 校验 + 执行侧渲染 | 提交 `payload.script` / `payload.url`，非法值 400，合法值真的跑到那个文件/地址 |
| M2 接口面 | N06 | `GET /executors` 的 `adhoc`/`location` 元数据 + 启动告警 | 接口能说清"哪几条是自由执行、位置输入框该怎么填" |
| M3 界面 | N07 | 新建任务表单三处改动 + 列表/详情/筛选 | 界面上名称自由填、类型分三组可选、adhoc 类型给位置输入框，四类各跑通一条 |
| M4 收口 | N08 | 使用者文档同步、全仓验证、端到端实测、后续项登记 | 四份文档与设计一致；PHP/Python/Shell/HTTP 各一条真实任务到终态 |

依赖关系（与各卡片头部的"依赖任务"一致，冲突时以卡片为准）：
N01→N02 顺序做；N03→N04→N05 顺序做，与 N01-N02 无依赖可并行；
N06 需要 N02 与 N05；N07 需要 N02 与 N06；N08 需要全部。

**跨阶段的两个硬性同步点**（分开做会留下不一致状态）：

- N02 必须同一次改完 `gateExecutorSubmission`（`api/handlers_executors.go:599`）。
  只改创建流程不改门禁，会出现"按标签查不到档位、于是 adhoc 任务的 payload 一条都没校验
  就进了堆"，而那一刻任务已经会被真的执行一次。
- N05 必须同一次改完 `submissionFieldNames`（`executor/args.go:71-73`）与
  `submissionKeys` 的错误文案（`:197`）。只加键不改错误文案，普通档位收到 `script` 键时
  报出来的允许键列表与实际不一致。

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

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过；收口前跑
  `go test -race -timeout 30m ./...`。**`-count=5` 一类重复跑必须带 `-timeout 30m`**，
  否则偶发挂时（既有实测口径）。
- `gofmt -l` 在本仓库会因行尾 CRLF 报出大量文件，结果不可用。要检查格式时只对新增文件
  显式 `gofmt -w <file>`，不要按 `gofmt -l` 的输出判断是否合格。
- **本系列新增一整节配置**（`executors.adhoc.*`），必须同时改
  `configs/config.example.yaml`（入库）与本机 `configs/config.yaml`（含凭据、被 `.gitignore` 排除），
  否则 `go test -run TestExampleConfigMatchesLocal ./core` 失败（`core/config_test.go:423`；
  本机没有 `config.yaml` 时该测试是跳过而不是通过，记录时要分清）。
  注释体例照同节既有写法（`configs/config.example.yaml:74-114`）。
- **新配置字段必须进热重载分类表**：`core/config_reload.go` 的表（`:60-96`）里逐条写
  `executors.adhoc.*` 的归类。守卫有两条，都会红：
  `TestEveryLeafKeyIsClassed`（`core/config_reload_test.go:24`）与
  导出字段叶子覆盖（`core/config_reload_test.go:101` `assertEveryExportedFieldCovered`）。
- `BindEnv` 的字符串列表漏项**不会报错**，只会让该项无法用 `GODELAYQ_*` 覆盖
  （既有坑，登记在 web-profile 系列口径里）；新增环境变量覆盖要顺带核对那份列表。
- 依赖方向红线：`core` 不许 import `executor`；`api` 可以 import `executor`（现状已如此）。
- 构造函数命名一律 `New*`（`Open` 属既有例外名单）。
- 涉及端点的卡片：`api/audit.go` 的 `auditActions` 映射（`:108-120`）要核对是否需要加行；
  本系列不新增写端点，因此预期是"不加"，但要用一条用例证明 adhoc 任务的建任务请求
  仍落进既有动作词而不是 `other`。
- 涉及前端的卡片：`cd web && npx vue-tsc --noEmit` 与 `npm run build` 必须通过；
  图标只用 lucide；改 `.vue` 之后要 `npm run build` 再 `go build -tags dashboard`
  才看得到变化（内嵌的是 `web/dist`，它被 `.gitignore` 排除）。
- 界面文案改动要同步 `web/src/content/job-template.md`（那份写着"任务名称=已注册的任务类型"
  一类说法，N07 要按新事实重写，N08 grep 复核没有残留）。
- 每张卡片一个提交（改动跨面大时可按"新增文件 / 接线 / 测试"拆 2-3 个），
  提交信息用 Conventional Commits，前缀按内容选 `feat` / `fix` / `test` / `docs` / `refactor`。
- 在共享 worktree 里不要直接 `git stash`；需要暂存时用带名字的 tag。
- 冒烟一律在系统临时目录里造独立的配置 + 数据目录 + 产物目录，跑完删除；
  不得写入仓库的 `configs/config.yaml` 与 `data/`；harness 不要放 `%TEMP%` 之外又被系统回收
  （既有实测口径）。执行器相关冒烟需要凭据，用 `GODELAYQ_SERVER_PORT` 挑一个
  IPv4/IPv6 都空闲的端口；残留的 `server.exe` 会锁住 `data/`，冒烟前先确认没有残留进程。
- 浏览器实测注意：应用内标签页被隐藏时时钟与定时器会节流，秒级现象一律标"未观测"；
  交互走脚本求值而不是真点击。

## 系列级待做项（本期只登记，不在任何一张卡里做）

| # | 事项 | 为什么本期不做 |
| --- | --- | --- |
| S-1 | 任务改名（`PUT /jobs/:id` 放开 `name`） | §D14：改名要连带决定"按名称筛选的历史任务怎么找""事件里的旧名字怎么处理"，是另一件事。N07 只在编辑模式把名称渲染成只读并写明原因 |
| S-2 | 目录任务加载器的名称/类型拆分 | `core/load.go:25` 的 `name` 仍同时是查找键。本期保持现状并在 N08 的文档里写明"加载器只支持旧写法" |
| S-3 | 观测层按类型查询 | §D13：事件表只有 `job_name` 一列（`store/sqlite/schema.go:35`），解耦后它是标签。要按类型查事件得加列与迁移，本期不做，详情页按 `job_id` 回查 |
| S-4 | adhoc 的执行位置事前预检（列出 workspace 下可选脚本） | 需要新增文件浏览接口与越界读取面，与本期"放宽"的方向相反，单独评审 |
| S-5 | 按任务类型的细粒度授权（谁能提交 `exec.http` 但不能提交 `exec.php`） | 现在只有一层 `executors.required_role`；细粒度要新的授权模型，与 web-profile 系列 S-2 是同一件事 |

## 待拍板（写卡时按推荐值落的）

| # | 问题 | 卡片采用的答案 | 设计文档 |
| --- | --- | --- | --- |
| P1 | adhoc 的路径范围默认 | 默认**不限**（`path_prefixes: []`），配置注释给收紧示例 | §12 P1 |
| P2 | 四条内置档位的键名 | `exec.php` / `exec.python` / `exec.shell` / `exec.http`（档位名字符集不含点，`core/executor_profile_store.go:22`） | §12 P2 |
| P3 | 内置条目要不要扩到 node/pwsh/cmd/java | 本期只做四条，其余由 `runtime_allow` 与用户自建普通档位承担 | §12 P3 |
| P4 | 名称长度上限 | 64 个字符，与档位名规则对齐 | §12 P4、§D3 |
| P5 | HTTP adhoc 能不能带请求体 | 能（`body: json`），仍走既有 body 形态校验 | §12 P5 |

## 状态

| 任务 | 内容 | 状态 |
| --- | --- | --- |
| N01 | 任务名称规则 | 已完成（2026-10-06，实现记录见该卡 §10；`core.ValidateJobName` + `core/job_name.go`，30 条子测试全绿，本卡无调用点） |
| N02 | api 名称/类型解耦 | 待执行 |
| N03 | `executors.adhoc.*` 配置节 | 待执行 |
| N04 | adhoc 档位构造与注册 | 待执行 |
| N05 | 提交期校验与执行侧渲染 | 待执行 |
| N06 | 档位元数据与启动告警 | 待执行 |
| N07 | 控制台表单与列表 | 待执行 |
| N08 | 文档收口与全量验证 | 待执行 |
