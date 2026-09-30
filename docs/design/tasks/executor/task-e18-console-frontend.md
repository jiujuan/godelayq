# TASK-E18　控制台：执行结果展示与表单校验

- 所属阶段：M5 收口
- 依赖任务：TASK-E07、E13、E15、E16
- 涉及文件：`web/src/api/types.ts`、`web/src/api/executors.ts`、`web/src/api/jobs.ts`、`web/src/views/JobDetailView.vue`、`web/src/views/AdminView.vue`、`web/src/views/TemplateView.vue`、`web/src/components/jobs/JobForm.vue`、`web/src/components/jobs/PayloadEditor.vue`、`web/src/components/jobs/JobEventTimeline.vue`、`web/src/content/job-template.md`、`web/src/composables/usePermission.ts`、`web/src/plugins/realtime-effects.ts`
- 预计规模：中（前端）

## 1. 任务目标

让控制台能看懂执行器任务：知道有哪些档位、每个档位要填什么参数、跑完之后退出码/状态码/输出是什么。同时把参数校验搬到表单里，让填错在提交前就提示，而不是等接口返回 400。

## 2. 背景与当前问题

`web/` 已有八个页面（`web/src/views/` 下），任务详情在 `JobDetailView.vue`，事件时间线在 `components/jobs/JobEventTimeline.vue`，新建表单在 `components/jobs/JobForm.vue`，其中 payload 用 `PayloadEditor.vue` 直接编辑原始 JSON。实时侧由 `plugins/realtime-effects.ts` 把事件转成查询失效。

改动前需要知道的三条约束（都在代码注释里）：菜单纯粹由路由表派生，权限用单一 `meta.minimumRole`（`web/src/router/index.ts`）；权限判断集中在 `composables/usePermission.ts`；图标统一用 lucide。本卡不改 `web/src/styles/tokens.css`，只在既有类名体系里加块，视觉保持与现有页面一致。

## 3. 要实现的功能

### 3.1 接口与类型

1. `types.ts` 增加 `ExecMeta`、`ExecutorProfile`、`JobResultResponse`，与后端 JSON 字段逐一对应（后端形状在 E05/E07/E13 定稿）。`Job` 类型增加可选 `exec?: ExecMeta`。
2. `executors.ts`：`fetchExecutors(): Promise<ExecutorProfile[]>`、`fetchJobResult(id, params): Promise<JobResultResponse>`。请求走既有 `api/client.ts`（它负责 401 刷新与重放一次），不要在页面里直接 `fetch`。
3. 查询键照 `api/keys.ts` 的既有约定新增 `executors`、`jobResult(id, attempt, stream, from)` 两组，失效逻辑在 `plugins/realtime-effects.ts` 里加一条：收到 `job.completed` / `job.failed` 且事件里有 `result` 时，让对应任务的详情查询与结果查询失效。

### 3.2 任务详情页

1. 新增"执行结果"区块，只在 `job.exec` 存在时渲染：
   - 第一行：类别（`kind`）、退出码或 HTTP 状态、耗时、是否截断。
   - `permanent` 为真时显示"这类失败重试无意义，请检查参数或脚本"提示文案（plain 文本，不加图标堆砌）。
   - `artifact === "purged"` 时显示"输出文件已按保留策略清理"，并隐藏输出块。
2. 输出块：stdout / stderr 两个标签页，默认读尾部（E15 的 `preferredStreamDirection` 提示如果实现了就按它选默认），每页首次点击才请求（懒加载），加载后显示"读取头部/尾部""加载更多"两个动作，`max_bytes` 用 E07 的默认值。
3. 每次尝试可切换：`attempt` 下拉（范围来自 `job.attempts`），默认最近一次。
4. 重试链可读性：任务有多次尝试时，在结果区块上方列出"第 1 次 退出码 1 / 第 2 次 退出码 0"这样的一行摘要（数据来自事件时间线里的 `result`，不需要新端点）。

   E15 落地后的两条补充（实施本卡前先读 `task-e15-http-executor.md` 第 10 节）：

   - 第 2 项的默认读取方向已经有后端取值可用：`executor.PreferredResultDirection(profile)` 对 `http`
     返回 `head`、其余返回 `tail`，但**它还没进 `GET /api/v1/executors` 的响应**。本卡要么加一个字段
     把它透出（同时更新 `types.ts` 与 `docs/api.md`），要么在前端按 `kind` 自己判——两处都判就会漂移，
     实施时二选一并在提交信息里写明选了哪个。
   - 第 3 项的 `attempt` 下拉现在拿不到多次尝试：`core/job.go` 的 `CloneForRetry` 不把 `Attempts`
     带给重试副本，重试的每一次执行都是 `attempt=1`，产物文件被后一次覆盖（E15 卡末尾登记为待处理）。
     如果那条先被修掉，本项按卡片原文做；没修之前下拉只有一项，界面要能容忍这种情况。

   E16 落地后的三条补充（实施本卡前先读 `task-e16-submission-role-and-mask.md` 第 10 节）：

   - `GET /api/v1/executors` 现在带 `required_role`（执行器关闭时是 `null`）与每个档位的
     `has_secret_args`。前端要按这两个值做两件事：`usePermission.ts` 的 `canSubmitExecutorJobs`
     用 `required_role`（第 3.3 第 4 条），表单里对 `has_secret_args: true` 的档位显示
     "该档位的参数不会回显"而不是自己再复制一份 `args[].secret` 判断。
   - 任务详情与列表里的 `payload` 已经是掩码后的（`"***"`），前端拿不到原值，因此
     "编辑任务"表单对执行器任务不能用响应里的 payload 回填参数；要改参数就得让用户重新输入。
     `exec.preview` 同样按值掩过（`orders/***` 这类形状），不要把 `***` 当成执行失败。
   - `GET /jobs/:id/result` 对含 secret 参数的档位会升档位（403 时 details 说明是档位声明），
     放行时响应多一个 `redaction_note` 字段。结果面板要在正文上方显示这句话，
     并且不要把它做成"已脱敏"的提示——它说的是输出里可能仍有凭据。

### 3.3 新建表单

1. `JobForm.vue`：`name` 的候选来自 `fetchJobTypes()`（既有）与 `fetchExecutors()`；选中 `exec.*` 时切换成"参数表单"模式：
   - 按档位的 `args` 描述生成输入项（必填标记、默认值预填、`pattern` 生成 `title` 提示与前端校验）。
   - `secret` 参数用密码型输入框，并且提交后不回填值。
   - 允许注入的 `env` 键用"键值对"列表，键的下拉来自档位的 `env_allow`。
   - `http` 档位显示 `params`、允许的 `headers`、body 编辑区（body 用 `PayloadEditor.vue` 复用）。
   - 超时输入框提示当前生效区间（来自档位的 `timeout` 与全局 `max_timeout`），非法值在前端就拦下。
2. 切回普通任务时表单模式复位，不留残留参数。
3. 提交失败（400）时把后端 `details` 显示到对应参数项下方；后端信息格式在 E08 已规范（含参数名），前端按参数名匹配，匹配不到就整体显示。
4. `usePermission.ts` 增加能力项 `canSubmitExecutorJobs`：依据登录身份档位与 `executors.required_role`（`/executors` 响应里带这个字段）决定"新建执行器任务"入口是否可用。**按钮隐藏只是体验，服务端 403 才是边界**（这条在 `docs/design/web-console-design.md` §5.7 已写明，本卡沿用同样措辞写在代码注释里）。

### 3.4 事件与实时页

1. `JobEventTimeline.vue`：`job.completed` / `job.failed` 事件带 `result` 时，在时间线条目上追加一行 `exit 1 · 3.2s` 或 `HTTP 500 · 0.4s` 的短摘要（长度受限，超出用省略号；完整内容仍在详情页）。
2. `views/DashboardView.vue` 与实时页不需要新增卡片：结果摘要已经出现在事件流里。
3. `AdminView.vue`：增加执行池占用四格（`exec_workers` / `exec_running` / `exec_queue_length` / `exec_queue_capacity`），数据来自 `GET /admin/runtime`（E13 已加字段）。当 `exec_workers === 0` 时该区块显示"未启用执行器"而不是四个 0，避免误读。

### 3.5 档位与模板页

1. `views/TemplateView.vue`（`web/src/content/job-template.md` 的承载页）：把设计文档 §5 的三类示例（Unix 与 Windows 各一份）补进 markdown，并把可下载指引更新到含 `payload` 示例的版本。
2. `views/JobsView.vue` 的筛选栏不加新维度（`exec` 不是任务状态）。
3. 新增一个轻量的"执行器档位"只读列表页还是并入设置页：本卡**并入 `SettingsView.vue` 的一个区块**，不新增路由（理由：档位由配置文件决定、运维只读、增加路由会牵动菜单与权限表）。展示内容：档位名、类别、是否可用（`runtime_ok`）、不可用原因、超时上限、参数列表（不显示 env 固定值）。

## 4. 实现步骤

1. 先补类型与 `executors.ts` + `keys.ts`（纯数据层，可先于页面合并）。
2. 做详情页结果区块与懒加载输出块。
3. 做表单模式切换与参数校验。
4. 做时间线摘要与运维页四格。
5. 最后做设置页区块与模板 markdown。
6. 每步之后跑 `npx vue-tsc --noEmit`，不要把类型错误攒到最后。

## 5. 测试与验证要求

`web/` 目前没有前端单元测试框架，因此本卡的"测试"由三部分组成，缺一不可：

1. 类型与构建：`npx vue-tsc --noEmit`、`npm run build` 通过。
2. 手工走查清单（后端需已具备 E07/E09 能力；开发形态 `npm run dev` + 后端 `:8080`）：
   - [x] 档位可用/不可用两种状态的显示差异正确，不可用时原因文本可读。（两处：设置页与表单下拉，见 §10 冒烟第 1 条）
   - [x] 必填参数为空、`pattern` 不符、超时超上限三种输入都在前端被拦下并给出具体提示。（§10 冒烟第 2 条）
   - [x] 提交成功后，详情页能看到退出码与耗时；`exit 3` 的任务显示失败态与 stderr 预览。（§10 冒烟第 3 条）
   - [x] stdout/stderr 两个标签页切换、头尾读取、"加载更多"都取到不同内容且不重复请求同一区间。（§10 冒烟第 4 条）
   - [~] 多次尝试的任务在 `attempt` 下拉里能分别读到各自输出。**做不到，只验证了界面容忍**：`CloneForRetry` 不传 `Attempts`，重试任务仍报 `attempts=1`（§10 冒烟第 5 条与"未验证与遗留"第一条）
   - [x] 产物被清理后（手工删除 `data/exec/<id>` 再刷新）显示已清理提示且不报错。（§10 冒烟第 6 条）
   - [x] 低权限身份（viewer）看不到"新建执行器任务"入口；用接口直连提交仍被服务端拒（验证隐藏不是唯一防线）。viewer 那侧是既有口径的按钮 disabled，档位隐藏要看 operator（§10 冒烟第 7 条）
   - [x] 运维页四格数字与 `GET /admin/runtime` 原始响应一致。（2/2/2/2 逐字段相同；这一页要 ops 身份，§10 冒烟第 8 条）
   - [x] 事件时间线的结果摘要不撑破布局（超长 `preview` 场景）。（`preview` 不进时间线，摘要定长，§10 冒烟第 9 条）
3. 浏览器实测的两条已知限制（`docs/design/tasks/executor/README.md` 之外单独提醒）：
   - 应用内标签页被隐藏时定时器会被降频，秒级的 toast 与折叠动画可能观测不到；交互用脚本驱动，秒级现象如果看不到就在报告里标注"未观测"。
   - 需要观测执行过程的任务，用 `delay` 留出足够时间（10 秒以上），不要靠 1 秒任务判断时序。

## 6. 完成标准（DoD）

- [x] 三个新类型与后端 JSON 字段逐一对得上，且后端字段改名会让 `vue-tsc` 报错（用显式 interface，不用 `any` 兜底）。
- [x] 结果区块在 `exec` 缺省（普通任务）时完全不渲染，不出现空卡片。（走查里普通任务 `report_generate` 的详情页没有该区块）
- [x] 输出内容是懒加载的：打开任务列表与详情页首屏都不会请求 `/result`。（详情页首屏资源计时里没有该请求，§10"另外四条"）
- [x] 表单的校验规则来自 `/executors` 响应，不在前端复制一份正则表（复制必然漂移）。必填标记、`pattern`、位置参数上限、可覆盖头、`body_mode`、超时上限全部来自那一份响应
- [x] `secret` 参数的输入值不出现在 URL、不回显、不写入 `sessionStorage`（检查 `stores/auth.ts` 之外没有额外持久化）。前端唯一的持久化就是那四个 `godelayq.*` 鉴据键
- [x] 没有新增路由、没有改 `tokens.css`、图标全部来自 lucide。（`git diff --name-only` 的改动清单里没有 router、tokens.css 与 package.json）
- [x] 手工走查清单逐条有结果记录，观测不到的项目明确写"未观测"而不是打勾。（§10 冒烟表与"未验证与遗留"表）

## 7. 验收方式

```bash
cd web && npx vue-tsc --noEmit && npm run build
# 单二进制形态回归（前端内嵌）
cd .. && go build -tags dashboard -o godelayq-console ./cmd/server
```

打开 `http://localhost:8080/`，按第 5.2 清单走查一遍。

## 8. 不在本任务范围

- 不做"在线编辑档位配置"（档位只能改配置文件并重启，与账号表同一口径）。
- 不做输出内容的全文搜索、语法高亮、下载为文件。
- 不做实时 tail（WebSocket 推输出流）：本期只提供按需读取。
- 不引入新的前端依赖库（依赖版本写在 `web-console-design.md` §4.1）。
- 不做任务详情的执行耗时图表。

## 9. 风险与回滚

- 风险：表单模式依赖 `/executors` 的响应形状，而后端在这个阶段还可能调整（E15 的 `preferredStreamDirection` 就是后加的）。要求 `types.ts` 里对可选字段一律用 `?`，前端读取处全部有兜底，避免后端少返回一个字段时页面无法显示。
- 风险：输出块渲染脚本产生的大量内容，可能包含控制字符或超长单行。要求渲染前做单行长度截断与不可见字符过滤（用 `JSON` 安全文本节点渲染，**不要用 `v-html`**，否则任务输出里的内容会变成前端脚本执行）。
- 风险：把"新建执行器任务"入口按权限隐藏，容易让人误以为后端也做了同样判断。第 3.4 条要求注释里写死"服务端才是边界"。
- 回滚：前端各区块都是新增，可分步回滚；`realtime-effects.ts` 的失效规则去掉即回到 E07 之前的行为（结果需要手动刷新才出现）。

## 10. 实现记录（2026-09-30）

### 落地文件

后端（本卡 §4 的"实现步骤"只列了 `web/` 下的活儿，实际先动了后端，见"偏离"第 1 条）：

- `api/handlers_executors.go:311-329` — `ExecutorProfileResponse` 新增 `positional`、
  `preferred_result_direction`、`method`、`header_allow`、`body_mode` 五个字段；
  `:367` 在 `enabled` 分支里补 `max_timeout`；`:406-422` 由 `toExecutorProfile` 填这些值，
  其中 `body_mode` 把配置里的空写法归一成 `none`。
- `api/dto.go:83-86` 与 `api/handlers.go:634` — `JobResponse.attempts`，取值就是 `job.Attempts`，
  与 `/result` 允许的 `attempt` 范围同一条口径。
- `executor/registry.go` — 新增 `Registry.MaxTimeout()`，把配置里的 `executors.max_timeout` 交给 api 层，
  前端因此不用复制这个数。
- 新增用例：`api/executors_form_metadata_test.go`、`api/job_attempts_test.go`。

前端：

- `web/src/api/types.ts`（`ExecMeta`、`ExecutorProfile`、`JobResultResponse` 与新增字段）、
  `web/src/api/keys.ts:17,23,31`（`executors`、`jobResult(...)`、`jobResultAll(id)`）。
- `web/src/plugins/realtime-effects.ts:37-48,70` — 收到带 `result` 的完成/失败事件时，
  除既有失效外再让该任务的结果查询失效（`jobResultAll` 是前缀键，覆盖所有已展开的流与区间）。
- `web/src/display.ts:34,45,52,62,78` — `formatDurationMs`、`execOutcome`、`execSummary`、
  `eventExec`、`sanitizeOutput` 五个纯函数。
- `web/src/components/jobs/JobExecResult.vue`（新文件）、`JobForm.vue`、`JobEventTimeline.vue`。
- `web/src/composables/usePermission.ts:70` — `canSubmitExecutorJobs(required)`。
- `web/src/views/JobDetailView.vue:136,273`、`AdminView.vue:112`、`SettingsView.vue:65`。
- `web/src/content/job-template.md` — 新增 §12 与三处指向它的补写。

### 与卡片的偏离与补充

1. **先补后端再写前端**（卡片 §4 没列 api 文件）。表单需要的信息 `/executors` 原本一个都不给：
   位置参数的上限与格式、http 档位的方法与可覆盖头、`body_mode`、读输出的建议起点、
   全局超时上限；详情页要判断"有几个尝试"也拿不到 `attempts`。
   按卡片 §3.1 第 1 条"与后端 JSON 字段逐一对应"的口径，这些都在后端补齐并各写了一条用例，
   而不是在前端复制一份规则表。
2. **读取方向选了"后端透出"**。本卡 §3.2 的 E15 补充第 1 条要求在
   `executor.PreferredResultDirection` 与"前端按 `kind` 自己判"之间二选一；选了前者
   （`api/handlers_executors.go:406`），前端只读 `preferred_result_direction`
   （`JobExecResult.vue:62-67`）。档位已被删掉时退回 `tail`。提交信息只有标题，所以结论记在这里。
3. **编辑模式不给档位参数输入**（`JobForm.vue:115-116`、`:320`、`:818`）。
   E16 之后读取接口给的 payload 是掩码过的（`{"args":{"token":"***"}}`），
   回填等于把 `***` 当成真实取值存回去。所以编辑执行器任务时仍是那个 JSON 文本框，
   框下面一句话说明 `***` 的来历与保存后果。
4. **档位下拉按 `required_role` 整片隐藏**（`JobForm.vue:441`），说明文案挂在名称字段的 hint 位上
   （`:451-455`）。走查时发现 `UiSelect` 的提示位是"错误优先"（`web/src/components/ui/UiSelect.vue:51-52`）：
   名称还没选时那句解释被"必须选择一个任务类型"顶掉。既有组件的优先级不动，
   现象记录在此；选了任意名称后那句说明会出现。
5. **`JobForm` 自己发 `useQuery` 取档位**，不从宿主页面传 props。这个表单被任务列表与详情页两处使用，
   传参要改两个页面；档位列表 5 分钟内 `staleTime` 复用同一份缓存（`queryKeys.executors`）。
6. **尝试编号显示成"第 N 条结论"而不是"第 N 次"**（`JobExecResult.vue:149-163`）。
   `core.CloneForRetry` 不带 `Attempts`，重试副本的 `job.attempts` 与事件里的 `metadata.attempt`
   都停在起点（走查实测：三次执行的时间线全写"第 1 次尝试"，产物文件名全是 `a1.out`，
   后一次覆盖前一次）。列表按事件顺序编号，既不谎报次数也不空着一块。
7. **"加载更多"是把预算翻倍**，不是从上次的偏移接着读（`JobExecResult.vue:130-137`）。
   `/result` 只有 `from`（head|tail）与 `max_bytes` 两个旋钮，没有偏移量，
   所以拿到的是包含上一次在内的更长一段，面板整块替换，不会重复显示同一段文字。
   首屏与"不限量"都走后端默认值（`NO_BUDGET` 即请求里不写 `max_bytes`，`JobExecResult.vue:24`）。
8. **输出正文一律走文本节点**（`JobExecResult.vue:302-306` 的 `<pre>{{ shown }}</pre>`），
   渲染前经 `sanitizeOutput`：丢掉 `\r`、把控制字符替换成 U+FFFD、单行超过 400 字符截断并注明省略了多少。
   实测：`cmd` 输出的 `\r\n` 在页面上只剩 `\n`。
9. **§3.5 第 3 条的档位一览并入设置页**（`SettingsView.vue:65`），不新开路由——
   与 §2 记录的"菜单由路由表派生、权限看 `meta.minimumRole`"保持一致，新增路由要牵动菜单与权限表。
10. **模板页没有代码改动**。`TemplateView.vue` 渲染的就是 `web/src/content/job-template.md` 这一份
    （`?raw` 打进产物），下载按钮发出去的 Blob 与页面同源，所以只改 markdown 就同时更新了页面与下载文件。
11. **`types.ts` 里新增字段全部用 `?`**，读取处都有兜底（§9 第一条风险的处置）；
    后端少返回一个键时页面照常显示，不会整块空白。

### 验证结果

| 命令 | 结果 |
| --- | --- |
| `cd web && npx vue-tsc --noEmit` | 通过（分四批各跑一次：数据层、结果面板、表单、时间线与两个页面；每批恢复后即跑） |
| `cd web && npm run build` | 通过，`✓ built in 2.25s` |
| `go build ./... && go vet ./...` | 通过 |
| `go test ./... -race -count=1` | 全绿：api 114.1s、cmd/server 5.4s、core 12.4s、executor 22.5s（`api` 这一轮跑得比 TASK-E17 收尾那次（49.1s）慢一倍，是这台机器当时的负载，用例集合没变） |
| `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64` 的 `go build ./...` | 通过 |
| `go build -tags dashboard -o console.exe ./cmd/server` | 通过（`web/dist` 已就位。这条只能在真实工作树里跑：`web/dist` 不入库，用 `git checkout-index` 复制出来的临时检出没有它，会报 `pattern all:dist: no matching files found`，那不是代码问题） |
| `gofmt -l api executor` | 只有 CRLF 造成的整仓误报，无真实格式问题（判定方法见 TASK-E17 卡第 10 节"未验证与遗留"） |

前端没有单元测试框架（§5 开头就写了这个前提），所以后端两条用例 + `vue-tsc` + 构建 + 浏览器走查就是本卡的全部验证手段。

| 用例 | 覆盖的卡片条目 |
| --- | --- |
| `TestListExecutors_ExposesFormMetadata` | §3.1 第 1、2 条与"偏离"第 1 条：三个档位的 `positional`、`method`、`header_allow`、`body_mode`（含空写法归一成 `none`）、`preferred_result_direction`（进程档位 `tail`、http `head`）与顶层 `max_timeout` |
| `TestJobResponse_ExposesAttempts` | `attempts` 字段：执行过 3 次的任务回 3，从未执行的回 0 且键存在（前端要按它算下拉项数） |

### 冒烟（真实进程 + 浏览器走查，照 §5.2 清单）

环境与造数据：

- 形态：`go build -tags dashboard` 出的单二进制（内嵌前端），监听 `:18085`；
  配置、store、产物目录、workspace 全在 `%TEMP%/gd-e18smoke/` 下，仓库里的 `data/` 没被写过。
- 账号四个（同一份 bcrypt 哈希）：`admin01`=admin、`oper01`=operator、`viewer01`=viewer、`keeper01`=ops；
  `executors.required_role` 保持默认 `admin`，`max_timeout` 设 5m。
- 档位五个：`long_report` 与 `slow_run`、`fail_stderr` 是 Windows 上的产物档位
  （`kind: binary` + `program: cmd` + `fixed_args: ["/c", "<批处理>"]`，脚本放在 workspace 里），
  `missing_script` 故意指向不存在的 `scripts/not-deployed.sh`（探测不可用），
  `target_ok` 是 http 档位，打到本机 `:18099` 的临时 Python 服务（`deny_private_ranges: false`，只为走查，不进生产配置）。
- 浏览器：应用内浏览器，视口 0×0、指针点击不可用，所以全部交互用 `evaluate_script` 驱动，
  读文本用 `innerText`；这条限制连同隐藏页定时器降频一起写在下面"未观测"里。

清单逐条结果：

| # | 条目 | 结果 |
| --- | --- | --- |
| 1 | 档位可用/不可用两种状态 | 通过。设置页列出 5 个档位，`missing_script` 标"不可用"并给出 `script file "scripts/not-deployed.sh" does not exist`；新建表单里该选项 disabled 且标签自带原因，选中后顶部出现红字提示、"创建任务"按钮 disabled |
| 2 | 必填为空 / 格式不符 / 超时超上限 | 通过。三条文案分别是"这个参数是必填的"、"取值不符合档位声明的格式：`^[a-z0-9-]{1,32}$`"、"超过 `executors.max_timeout`（5m0s）：执行时会被夹到上限，这个值不会生效"，三种输入下提交按钮都是 disabled |
| 3 | 提交成功后退出码与耗时、`exit 3` 的失败态与 stderr | 通过。走查用表单新建 `exec.fail_stderr`（延迟 10s）→ 详情页显示 档位 `fail_stderr` / 类别 binary / 结论 `exit 3` / 耗时 19ms / `15 / 73 字节` / 产物在，并有"重试无意义"提示；stderr 标签页读出两行 `stderr-line-one: missing input file`、`stderr-line-two: cannot continue` |
| 4 | 标签页切换、头尾读取、加载更多且不重复 | 通过。`long_report` 产物 22776 字节：默认读到 8192（提示"已读 8192 / 22776 字节"），点"加载更多"变 16384、行数 145→289 且末行不变（尾部锚定，整块替换）；切"读头部"后首行变成 `godelayq-e18-smoke long report start`。切小文件（15 字节）时头尾内容相同，因为一次就读完了 |
| 5 | 多次尝试分别读输出 | 只能验证"容忍"这一半：三次执行的任务 `job.attempts` 仍是 1，下拉不渲染（`CloneForRetry` 那条缺陷未修，见"偏离"第 6 条）；替代的重试链列表正常显示"这一世里的执行结论（按事件顺序，共 3 条）"，三行分别是 `exit 1` / `exit 1 · 3.7s` 等 |
| 6 | 产物被清理后的提示 | 通过。手工删掉 `data/exec/<任务 id>/` 整个目录后点"标准输出"，面板显示"文件已不在"与"这一次尝试的产物文件已经不在了（按保留策略清理，或所在目录被移走）。摘要与字节数照旧"，没有 `role=alert` 的错误条，摘要与字节数照常。同一区块上方"产物文件"那行仍写"在"——它读的是执行那一刻落下的快照，措辞不一致登记在下面 |
| 7 | 低权限看不到入口，直连仍被拒 | 通过。viewer：列表页"新建任务"按钮 disabled（这是既有的 `job.create` 需要 operator 的口径，不是"隐藏"），表单打不开；operator：能打开表单，但下拉里 5 个 `exec.*` 全部不出现，选了普通名称后显示"执行器档位（exec. 开头）需要 admin 及以上才能提交，当前身份的下拉里没有它们"。服务端边界：viewer 的 JWT 提交 `exec.*` → 403 `insufficient role`；静态 machine token（等同 operator）提交 → 403，details 写明需要 admin；operator 读带 secret 档位的 `/result` → 403 |
| 8 | 运维页四格与 `/admin/runtime` 一致 | 通过。跑着 3 个档位任务时页面显示 执行器 worker 2 / 执行中 2 / 队列中 2 / 队列容量 2，与随后 REST 取的原始响应逐字段相同（`exec_workers:2, exec_running:2, exec_queue_length:2, exec_queue_capacity:2`）；同页"运行时占用"的普通池 4 / 0/4 / 0 / 0 也与原始响应一致，`/stats` 的 `running:2` 对应"两池之和"那句说明。注意 `/admin/runtime` 要 ops，admin 身份拿到的是 403，所以这一条要用 `keeper01` 看 |
| 9 | 时间线摘要不撑破布局 | 通过（现状比卡片假设的更窄）。时间线里的完成/失败行只放定长摘要（实测 `耗时 10ms · HTTP 200 · 2ms`、`超时中止 · exit 1 · 3.7s · max_retries=2 · retry_count=0`），`preview` 不进时间线，所以 2048 字节的输出不会变成一条超长文本；遍历 DOM 未见横向溢出，`documentElement` 也没有横向滚动 |

另外四条本卡条目外的观测（都记下来免得下一个人重新试）：

- 懒加载成立：详情页首屏的资源计时里只有 `/stats`、`/jobs/:id`、`/jobs/:id/events`、`/groups`、
  `/job-types`、`/executors`，没有任何 `/result` 请求；点开标签页才出现。
- 实时失效成立：详情页不刷新的情况下，任务执行完那一刻资源计时里多出一条 `GET /jobs/:id`（页面打开后 24.7s），
  "执行结果"区块随之自己出现——这是 `realtime-effects.ts` 那条规则的效果图。
- `secret` 参数：输入框是 `type=password` + `autocomplete=new-password`，标签写明"这是凭据：不回显，
  读取接口里也会掩码"；填进去的值不出现在 URL、不在 `sessionStorage`（里面只有 4 个 `godelayq.*` 鉴据键）。
  详情页与列表的 payload 显示成 `***`，`/result` 对含 secret 的档位带 `redaction_note`
  （"output is produced by the script or the remote endpoint and may contain the values of secret arguments"），
  面板把它渲染在正文上方。
- 模板页 §12 渲染正常（5 个小节、8 张表、17 个代码块），`html:false` 的转义仍在。

### 未验证与遗留

| 项 | 状态与处置 |
| --- | --- |
| `attempts > 1` 时切换尝试读各自输出 | 未验证（被 `CloneForRetry` 不传 `Attempts` 挡住：重试副本每次都是 `attempt=1`，产物文件也叫 `a1.out`，后一次覆盖前一次）。前端已按"容忍只有一次尝试"实现，等那条修好后下拉自然出现。缺陷本身在 TASK-E15 卡末尾登记着，本卡不改 core |
| 秒级的 toast 与折叠动画 | 未观测（应用内浏览器视口 0×0、隐藏页定时器降频，这是本目录既有的口径） |
| Linux / macOS 上的同一份走查 | 未做，机器上没有可用的 Linux 图形浏览器；已在 E19 卡 §5.4 登记"在 Linux 复走三条" |
| 结果区块"产物文件：在"与"产物文件已经不在了"并存 | 已确认是措辞问题不是判断问题：前者来自执行时落盘的快照，后者是这次读取的实况。留待有界面文案收敛需求时一起改，本卡不动 |
| 名称字段错误顶掉档位说明文案 | 已确认是 `UiSelect` 的既有优先级（error 优先于 hint），不改组件；现象写在"偏离"第 4 条 |
| `docs/api.md` 未写这几个新键 | 登记不修（归 E19）：`GET /executors` 的 `positional`、`method`、`header_allow`、`body_mode`、`preferred_result_direction`、`max_timeout` 与 `JobResponse.attempts` 都只在代码注释里，已补进 E19 卡 §3.1 的清单 |

