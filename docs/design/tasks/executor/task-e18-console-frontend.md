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
   - [ ] 档位可用/不可用两种状态的显示差异正确，不可用时原因文本可读。
   - [ ] 必填参数为空、`pattern` 不符、超时超上限三种输入都在前端被拦下并给出具体提示。
   - [ ] 提交成功后，详情页能看到退出码与耗时；`exit 3` 的任务显示失败态与 stderr 预览。
   - [ ] stdout/stderr 两个标签页切换、头尾读取、"加载更多"都取到不同内容且不重复请求同一区间。
   - [ ] 多次尝试的任务在 `attempt` 下拉里能分别读到各自输出。
   - [ ] 产物被清理后（手工删除 `data/exec/<id>` 再刷新）显示已清理提示且不报错。
   - [ ] 低权限身份（viewer）看不到"新建执行器任务"入口；用接口直连提交仍被服务端拒（验证隐藏不是唯一防线）。
   - [ ] 运维页四格数字与 `GET /admin/runtime` 原始响应一致。
   - [ ] 事件时间线的结果摘要不撑破布局（超长 `preview` 场景）。
3. 浏览器实测的两条已知限制（`docs/design/tasks/executor/README.md` 之外单独提醒）：
   - 应用内标签页被隐藏时定时器会被降频，秒级的 toast 与折叠动画可能观测不到；交互用脚本驱动，秒级现象如果看不到就在报告里标注"未观测"。
   - 需要观测执行过程的任务，用 `delay` 留出足够时间（10 秒以上），不要靠 1 秒任务判断时序。

## 6. 完成标准（DoD）

- [ ] 三个新类型与后端 JSON 字段逐一对得上，且后端字段改名会让 `vue-tsc` 报错（用显式 interface，不用 `any` 兜底）。
- [ ] 结果区块在 `exec` 缺省（普通任务）时完全不渲染，不出现空卡片。
- [ ] 输出内容是懒加载的：打开任务列表与详情页首屏都不会请求 `/result`。
- [ ] 表单的校验规则来自 `/executors` 响应，不在前端复制一份正则表（复制必然漂移）。
- [ ] `secret` 参数的输入值不出现在 URL、不回显、不写入 `sessionStorage`（检查 `stores/auth.ts` 之外没有额外持久化）。
- [ ] 没有新增路由、没有改 `tokens.css`、图标全部来自 lucide。
- [ ] 手工走查清单逐条有结果记录，观测不到的项目明确写"未观测"而不是打勾。

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
