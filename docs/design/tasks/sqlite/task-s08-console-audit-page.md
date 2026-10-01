# TASK-S08　控制台写操作审计页

- 所属阶段：后续独立排期（不阻塞 S07 收口）
- 依赖任务：TASK-S06
- 涉及文件：新增 `web/src/views/AuditView.vue`；改 `web/src/router/index.ts`、`web/src/api/admin.ts`、`web/src/api/types.ts`、`web/src/api/keys.ts`；若需要则改 `web/src/plugins/realtime-effects.ts`
- 预计规模：中

## 1. 任务目标

在控制台加一页写操作台账：按身份、动作、结论、时间窗筛选，翻页查看，点开一行看该行的全部字段。ops 档可见。

## 2. 背景与当前问题

S06 提供了 `GET /api/v1/admin/audit`，但没有界面读它。一张只能靠命令行查的表在实操中不会被查：出问题时需要的是"过去两小时谁被拒过"这类筛选，而不是手写 URL 参数。

既有前端的四条约定必须照做，不能另起一套：

1. **菜单由路由表派生**：`menuItems()` 从 `routes` 里筛"有 `meta.icon` 且有 `meta.title`、路径不含 `:`"的项（`web/src/router/index.ts:100-122`）。因此新页面只要在路由表里给一条带 icon 与 `minimumRole: 'ops'` 的记录，侧边栏入口与前端拦截自动生效（守卫见同文件 `:125-141`）。
2. **Query 键有唯一出处**：`web/src/api/keys.ts` 是键的定义处，注释写明"失效管线与页面必须用同一套键，否则 invalidate 打不中缓存"。新页的键必须加在这里。
3. **分页沿用 filters + offset 的既有形状**：`queryKeys.jobs(filters, offset)` 是现行做法（`keys.ts:7`），`web/src/api/jobs.ts:25-26` 把 `limit`/`offset` 原样带进查询参数。审计页不发明第二套分页。
4. **图标只用 lucide**（仓库既有约束，全项目无手写 SVG）。
5. 前端角色拦截只是体验，真正边界在服务端 RBAC——`admin.ts` 文件头已经这么写着，新页沿用同样的措辞。

## 3. 要实现的功能

1. `web/src/api/types.ts`：新增 `AuditEntry` 与 `AuditListResponse`，字段与 S06 的响应**逐一对应**，不在前端重新解释后端语义。
   `verdict` 与 `exec_verdict` 定义成字面量联合类型（`'ok' | 'denied' | 'bad_request' | 'not_found' | 'conflict' | 'partial' | 'error'`），这样筛选下拉的选项与后端封闭集是同一份来源；`action` 同理，若 S06 把映射表透出到某个端点则从那里取，**没透出就在前端写一份常量并注释"与 `api/audit.go` 的映射表对照维护"**（这是本卡唯一允许前端复制后端枚举的地方，因为下拉需要完整候选项；如果嫌两份不同步风险大，可以让 `/admin/audit` 响应带一份 `actions: [...]`，选择记进第 10 节）。
2. `web/src/api/admin.ts`：新增 `fetchAudit(filters: AuditQuery): Promise<AuditListResponse>`，参数为 `limit/offset/actor/action/verdict/since/until`，空值不发送（`client.ts` 的 `query` 已约定 `undefined` 的键整个省略）。
3. `web/src/api/keys.ts`：新增
   ```ts
   audit: (filters: AuditQuery, offset: number) => ['audit', filters, offset] as const,
   auditAll: ['audit'] as const,
   ```
   与 `jobs`/`jobsAll` 的成对写法保持一致。
4. 路由：`/audit`，`name: 'audit'`，`meta: { title: '审计', icon: <lucide 图标>, minimumRole: 'ops' }`，位置放在 `/admin` 之后、`/settings` 之前（运维性质）。
5. `web/src/views/AuditView.vue`：
   - 顶部筛选条：身份（文本）、动作（下拉）、结论（下拉）、时间窗（预设下拉：最近 1 小时 / 24 小时 / 7 天 / 全部，不引入日期选择器组件——现有 UI 组件集里没有，为一个页面加一套不划算）。
   - 表格列：时间、身份（+角色徽标）、动作、方法、结论、耗时、执行器结论（有值才显示内容，无值显示 `—`）。**默认列表不显示 `route`、`remote_ip`、`user_agent`**：列太多会读不清，而这三列在详情里能看到。
   - `verdict` 用 `UiBadge`：`ok` 中性、`denied`/`error` 警示、其余信息色。着色规则集中在一个映射函数里，不在模板里写三元表达式。
   - 行点开抽屉（`UiDrawer`）显示全部字段，含 `route`、`remote_ip`、`user_agent`、`job_id`、`handler_key`、`profile`、`exec_reason_code`。
   - 翻页：上一页/下一页 + "共 N 条"（用响应的 `total`）。空态用 `UiEmptyState`，且**要区分"没有记录"与"筛选太窄"**：前者文案说明台账从本版本开始，后者提示放宽条件。这个区分在 §5.7 的既有产物索引场景里同样存在（更早的产物不在列表里），措辞对齐。
   - 503（未装配审计）→ 页面显示"这次部署没有启用写操作台账"的说明态，不是错误弹窗：它和"未启用鉴权"一样是部署选择，不是故障。
6. 刷新策略：进入页面拉一次，**不做轮询**，也不接 WS 失效——审计是事后追溯用的，不是实时观测；`plugins/realtime-effects.ts` 不为它加规则。手动刷新按钮提供一次 `invalidateQueries(auditAll)` 即可。这条判断要写在页面注释里，避免后来人照抄 MonitorView 的实时接法。
7. 时间显示沿用控制台既有的时间格式化路径（不新增格式化函数）；后端给的是 RFC3339，展示为本地时间。

## 4. 实现步骤

1. 先补 types + api + keys 三层（纯增量，`vue-tsc` 立刻能验出字段不匹配）。
2. 加路由（此时页面文件还不存在，构建会报错，因此路由与视图同一提交）。
3. 写视图：先表格与翻页，再加筛选条，最后加抽屉。
4. 手工走查（§5.3），含低档位账号。

## 5. 测试要求

本仓库前端没有单测框架，因此验证分三层：类型、构建、手工走查。**逐条记录实测结果，不能只写"界面正常"**。

### 5.1 类型与构建

```bash
cd web && npx vue-tsc --noEmit && npm run build
go build -tags dashboard ./...
```

### 5.2 接口层手工（不经过界面）

用 `curl` 直接打 `/api/v1/admin/audit`：确认 `limit`/`offset` 翻页不重不漏、非法 `action` 值不报 500、静态 token 调它得到 403（machine 折算 operator，低于 ops）。

### 5.3 浏览器手工走查清单

后端要造得出对应数据，用 §5.2 的请求顺序制造台账（含成功、角色拒绝、执行器拒绝三类）。

1. ops 账号登录 → 侧边栏出现"审计"入口 → 页面有数据、时间列为本地时间。
2. admin 账号登录 → 入口不出现；直接访问 `/audit` 被弹回概览并给出提示句。
3. 筛选 `verdict=denied` → 只剩拒绝行；清空筛选 → 行数回到初始值。
4. 时间窗选"最近 1 小时"→ 更早的行消失；切"全部"→ 回来。
5. 翻页到底再点"下一页"→ 按钮禁用态而不是空表或报错。
6. 点一行开抽屉 → 全字段可见，`route` 列显示为模板（`/api/v1/jobs/:id/pause`）而不是真实路径。
7. **整页搜不到任何 payload 里的字符串**：先在 §5.2 里故意提交一条参数值可搜索的执行器任务，再回页面在表格与抽屉里搜那个字符串，命中数必须为 0。（这条是 S06 §6 第 3 条的界面侧复验，别省。）
8. 关掉 `observability.audit.enabled` 重启 → 页面显示未启用说明态，不弹错误 toast。
9. 空台账（新库 + 只有读请求）→ 空态文案是"没有记录"那一条。
10. 在浏览器隐藏标签页里挂机几分钟再切回 → 页面不自动刷新（§3.6 的判断），手动刷新能拿到新行。
    注意：隐藏标签页会被浏览器节流定时器，这类"页面停在那里会不会自己变"的观察在隐藏页里得不到结论，因此这条必须在可见标签页测，并在记录里写明测量条件。

## 6. 完成标准（DoD）

- [x] 菜单入口与前端拦截都靠路由表的 `meta` 生效，`menuItems()` 没有被改坏（其它页面的入口顺序不变）。（只加了一条路由记录与一个 `ScrollText` 导入；实测菜单为 `概览 任务 分组 任务模板 实时 运维 审计 设置`，见 §10.4 第 1、2 条）
- [x] Query 键定义在 `api/keys.ts`，视图里没有裸字符串键。（`grep -n "\['audit'\]" web/src/views/AuditView.vue` 无命中；失效只走 `queryKeys.auditAll`）
- [x] 分页沿用 `filters + offset`，与任务列表页同一形状；没有引入第二套分页交互。（`queryKeys.audit(filters, offset)`、`PAGE_SIZE=50`、`goPage` 的钳制写法照 `JobsView` 同形）
- [x] `verdict`/`action` 的候选项与后端封闭集一致，两份来源之间的维护关系有注释指向（或改为后端透出，二选一并在第 10 节记录选择与原因）。（九 + 二十一，选的是"前端常量 + 注释指向 `api/audit.go`"，理由见 §10.2 第 2 条；卡片 §3.1 那份七个取值的表缺 `throttled`/`other`，按实现写）
- [x] 503 与空台账是两种不同呈现（未启用说明态 / 空态文案），都不是错误弹窗。（§10.4 第 8、9 条）
- [x] §5.3 第 7 条通过：界面任何位置都不出现参数取值。（表格、抽屉、`document.body.innerText` 三处搜 canary 全 0 命中，同一词在产物文件里 1 命中作为反证）
- [x] 图标全部来自 lucide；无新增手写 SVG、无第二套图标库。
- [x] `npx vue-tsc --noEmit`、`npm run build`、`go build -tags dashboard ./...` 三项通过。（另跑 `go build/vet ./...` 与 `go test ./... -race -count=1`，见 §10.3）
- [x] 十条手工走查逐条记录实测结果；受平台或环境限制测不到的条目（例如需要非 ops 账号的那条，若本机没配该账号）明确标"未观测"并说明缺什么条件，不允许写成"应该没问题"。（十条都在 §10.4；第 9 条的"新库"前提在控制台形态下不成立、第 10 条的"可见标签页"条件未满足，两处都按现状标出并说明缺什么）

## 7. 验收方式

```bash
cd web && npx vue-tsc --noEmit && npm run build
go build -tags dashboard ./... && go test ./... -race
```

内嵌形态起服务（`-tags dashboard`，环境变量改端口与数据目录，避免占用本机既有部署），
浏览器登录 ops 账号走 §5.3 全部十条。

## 8. 不在本任务范围

- 不做导出 CSV。
- 不做按小时的趋势图（审计页的职责是"查一条具体记录"，趋势属于另一件事）。
- 不做任务详情的"谁提交了我"入口（需要 `job_id` 反查台账，那是后端的一个新查询）。
- 不给产物索引做独立页面：S05 的 `GET /jobs/:id/artifacts` 已经接进详情页的输出控件，本卡不再动它。
- 不改后端任何端点、不改 `api/audit.go` 的映射表内容（若走查发现映射缺项，登记为缺陷另立卡片，不在本卡夹带）。
- 不做在线用户/会话管理页（`web-console-design.md:987` 明确列为二期，与本卡无关）。

## 9. 风险与回滚

- 风险：前端复制一份 `action` 枚举会与后端映射表不同步，表现为下拉里少一个选项、或者选了之后查不到。两条出路：下拉改成"文本输入 + 建议列表"（不依赖完整枚举），或后端透出候选集。选哪条记进第 10 节，不要两份来源并存。
- 风险：`total` 是每次查询都做一次 `COUNT`，台账行数大时（默认上界 50 万）这一下可能比列表查询还慢。界面侧的缓解是"共 N 条"只在首屏与筛选变化时显示、翻页时不重算；若实测不可接受，把后端 `Query` 的总数改成可选参数（`?total=0`），这属于后端改动，另立卡片。
- 风险：审计行含账号名与拒绝原因，是运维页面里敏感度高的一类。低档位账号看不到入口不等于看不到数据——服务端已按 ops 拦，前端不做二次判断，但**不要把 `actor` 之外的字段塞进 URL 查询串**（浏览器历史与反向代理日志会留住它）。
- 回滚：删路由条目即撤掉入口，页面文件留着不影响任何东西；或整体 revert 单个提交。

## 10. 实现记录（执行时补写）

完成日期：2026-10-01。改动只落在 `web/`，Go 侧一个文件都没动（与 §8"不改后端"一致）。

### 10.1 落点

| 文件 | 内容 |
| --- | --- |
| `web/src/api/types.ts` | `AuditEntry`（17 个字段与 `AuditItem` 的 JSON 键逐一对应）、`AuditVerdict`（九个取值）、`AuditExecVerdict`（五个）、`AUDIT_ACTIONS`（21 项：19 个动作 + `unmatched` + `other`，注释指向 `api/audit.go` 的映射表）、`AUDIT_VERDICTS`、`AuditQuery`、`AuditListResponse` |
| `web/src/api/admin.ts` | `fetchAudit(query)`，七个参数逐字段列出（与 `listJobs` 同一条理由：类型里长出后端不认的键要在这里露头） |
| `web/src/api/keys.ts` | `audit(filters, offset)` 与 `auditAll`，与 `jobs`/`jobsAll` 成对写法一致 |
| `web/src/router/index.ts` | `/audit`（`name: 'audit'`、`title: '审计'`、`icon: ScrollText`、`minimumRole: 'ops'`），位置在 `/admin` 之后、`/settings` 之前；`menuItems()` 与守卫一字未改 |
| `web/src/views/AuditView.vue` | 新增页：筛选条（身份输入 + 动作/结论/时间窗三个原生 select）、七列表格、上一页下一页、行点开抽屉显示全部 17 列、503 说明态、两种空态、"重新查询"按钮 |
| `docs/design/web-console-design.md` | §5.7.6 的"两条出口"补第三条"界面"，指向本页与任务卡（卡片 §涉及文件没列这一份，见 §10.2 第 8 条） |

`web/src/plugins/realtime-effects.ts` 没有动（§3.6 的判断），确认过它失效的四个键
（`stats`/`jobsAll`/`groups`/`job(id)`/`jobResultAll`）没有一个会前缀命中 `['audit']`。

### 10.2 与卡片的偏离

1. **`verdict` 的联合类型是九个取值不是七个**：卡片 §3.1 列的表缺 `throttled`(429) 与 `other`（兜底），
   S06 落地时补进了封闭集（`api/audit.go:47-57`、导出的 `AuditVerdicts`）。前端照实现写，
   否则筛选下拉会少两项、后端也不会认这两个词。`action` 同理按 21 项写。
2. **`action` 候选项走"前端常量 + 注释指向"**：`AuditActions()` 只用于服务端校验，没有任何端点透出它，
   而 §8 又禁止改后端，所以卡片给的第二个选项（响应带 `actions: []`）不可选。
   两份来源的维护关系写在 `types.ts` 的常量注释里。
3. **时间窗折算成绝对 `since` 并且必须进查询键**（卡片没规定）。第一版把 `since` 只放在 `queryFn` 里
   （想做成"每次请求重算的滚动窗口"），实测三个窗口的计数一模一样——换窗不触发重取。
   改成"点下选项那一刻折算一次、结果进键"，顺带修掉一个更严重的问题：滚动窗口会让第 2 页的过滤条件
   与第 1 页不是同一件事。代价是窗口不再随时间前移，要更新的窗口重点一次选项或按"重新查询"。
   这条改动是实施中抓到的缺陷，见 §10.5 的 D-0801。
4. **§9 第二条的界面侧缓解（翻页时不重算"共 N 条"）没做**：后端 `Query` 恒发两条语句
   （`store/sqlite/audit.go:245`），少显示一次数字并不省掉那次 COUNT，只是让界面读数变得不一致。
   真要省这一次查询得走卡片说的 `?total=0`，那是后端改动、另立卡片。实测 110 行量级下翻页无感。
5. **筛选条件不写回 URL**：卡片只禁止"把 `actor` 之外的字段塞进查询串"（§9 第三条），
   没有要求同步。这一页没有跨页跳转与分享需求，而 `JobsView` 那套 URL 驱动是为了"分组页跳来筛好的列表"，
   所以选择不写回，比只禁一部分更严格，也不会把账号名留在浏览器历史里。
6. **登录行的档位显示为 `—` 而不是"匿名"**：`role` 在鉴权建立之前是空串（`api/audit.go:314-331`），
   登录行因此没有档位。第一版写成"匿名"，实测会把"谁在试着登录"说反；
   真相在抽屉的"身份类型"列（`user`/`machine`/`anonymous`）。
7. **耗时列复用 `formatDurationMs`**（`Math.round(latency_us / 1000)`），没有新增格式化函数（§3.7）。
   快请求显示 `0ms` 与 S06 的 D-0602 同一条口径（Windows 单调时钟粒度）。
8. **多改了一份文档**：`web-console-design.md` 不在卡片 §涉及文件 里，但它是控制台的现行规格，
   §5.7.6 只写了"两条出口"会让新增的页面没有出处。补一条指向本页，不动其它句子。
9. **§8 说"S05 的 `GET /jobs/:id/artifacts` 已经接进详情页的输出控件"与代码不符**：
   `web/src` 里没有任何地方请求该端点（只有 `ExecMeta.artifact` 这个状态位被 `JobExecResult.vue` 用着）。
   本卡按"不动它"执行，登记见 §10.5 的 D-0805。
10. 图标用了 `ScrollText`（路由 meta 与两处空态）和 `RefreshCw`（重新查询），都来自 `lucide-vue-next`；
    新页无手写 SVG、无第二套图标库。着色规则集中在 `VERDICT_TONES` 一张表 + `verdictTone()`，
    模板里没有三元表达式：`ok` 中性、`denied`/`bad_request`/`conflict`/`throttled` 警示、
    `error` 用 danger（比"警示"更重一档，卡片只说"警示"，这一处是解释性收紧）、其余信息色。

### 10.3 类型、构建与 Go 侧验证

```
cd web && npx vue-tsc --noEmit      通过（无输出）
cd web && npm run build             通过；AuditView 独立 chunk 9.8 KB（gzip 4.4 KB）
go build -tags dashboard ./...      通过（内嵌的就是上面这份 dist）
go build ./... / go vet ./...       通过（Go 侧零改动，跑一遍确认）
go test ./... -race -count=1        通过（§7 第二条；本卡没动 Go 代码，这条守的是"没碰坏"）
```

`gofmt -l` 按系列 README 的口径不作判据（CRLF 全量误报），本卡没有新增 Go 文件。

### 10.4 手工走查实测

环境：系统临时目录里独立的一份 `config.yaml` + `data/` + `-tags dashboard` 二进制（端口 8123），
四个账号 `viewer01/operator01/admin01/ops01`（密码 bcrypt 现造）、静态 token 一个，
`observability` 全开、`executors` 开一个 `node` 档位 `exec.hello`（`required_role: admin`）。跑完删除。

**§5.2 接口层（curl）**

- 造数据后共 100 行：`verdict` 一支覆盖了 `ok`/`denied`/`bad_request`/`not_found`/`conflict`/`partial`
  （另有 `job.create` 的 `exec_verdict` 取 `accepted` 与 `role_denied` 各一行），
  `actor_kind` 覆盖 `user`/`machine`/`anonymous`，`role` 覆盖四档 + `machine` + 空串。
  静态 token 的身份行是 `machine|machine|job.create`。
- **翻页不重不漏**：`limit=20` 逐页取到底（5 页）与 `limit=500` 一次性取，
  `time` 序列逐条相同、页内无重复；`offset=999` 给 `count=0` 而 `total` 仍是 100。
- **非法参数不出 500**：`action=../x`、`verdict=nonsense`、`limit=-5`、`limit=abc`、`offset=-1`、
  `since=01/01/2026`、`limit=501` 全是 400 且 `details` 给出合法取值；
  `actor=<中文>` 是 200 空列表（不是 400，长度未超 128 字节）。
- **档位**：`machine`/`admin01`/`operator01`/`viewer01` 全 403，只有 `ops01` 200。
- **canary**：以 `who=CANARY-s08-param-value` 提交 `exec.hello`（admin 档接受、operator 与 machine 档被拒），
  端点响应 0 命中、`data/exec/` 的产物文件与 `a1.meta.json` 各 1 处命中（证明这个词确实进了系统），
  `write_audit` 侧 0 命中；`observe.sqlite-wal` 有 10 处命中，那些是 `job_events` 的输出预览
  （S07 的 D-0704 已定口径：事件表可能带脚本输出，台账不带）。
  `route` 列的 8 个取值全是模板（含 `/api/v1/jobs/:id/pause`），整个响应里 `token=` 0 命中。

**§5.3 浏览器走查（十条逐条）**

| # | 条目 | 实测 |
| --- | --- | --- |
| 1 | ops 登录 → 入口出现、有数据、本地时间 | ✅ 菜单 `概览 任务 分组 任务模板 实时 运维 审计 设置`（其余入口顺序不变），首屏 50 行，时间列形如 `2026/10/1 16:11:12` |
| 2 | admin 看不到入口；直接访问被弹回 | ✅ 菜单无"审计"；用路由 `push('/audit')` 触发守卫 → 落在 `/`，页面出现提示句"需要 ops 及以上角色" |
| 3 | `verdict=denied` 只剩拒绝行；清空回到初始值 | ✅ 6 行全 `denied`（含两行 `role_denied` 与一行手工夹具），清空后回到全量计数 |
| 4 | 时间窗收窄→更早的行消失；切回全部→回来 | ✅ 同一份数据上 1h=106 / 24h=107 / 7d=109 / 全部=110（四个数互不相同；库里有 4 行夹具造的 2 小时/26 小时/3 天/9 天前）。**前提**：真实数据全是"刚刚"，所以往库里直接 `INSERT` 了 4 行旧时间戳（seq 递增，因此它们排在最前，时间列看着非单调——这是 S06 的"顺序按 seq 降序"既有口径） |
| 5 | 翻页到底→下一页禁用 | ✅ `actor=admin01` 下 73 行两页：第 1 页 `上一页=true 下一页=false`，第 2 页（23 行）`上一页=false 下一页=true`，再点不动、表格不变；单页场景（筛 `job.pause`，1 行）两个按钮都禁用而不是空表或报错 |
| 6 | 抽屉全字段可见、`route` 是模板 | ✅ 17 行字段全在（含 `route`、`remote_ip`、`user_agent`、`job_id`、`handler_key`、`profile`、`exec_reason_code`），空值显式 `—`；`job.pause` 那行显示 `/api/v1/jobs/:id/pause` |
| 7 | 整页搜不到参数取值 | ✅ 表格与抽屉内文、`document.body.innerText` 三处搜 `CANARY` 均 0 命中（同一词在产物文件里 1 命中，见上面 §5.2） |
| 8 | 关掉 `observability.audit.enabled` → 说明态不是错误弹窗 | ✅ 环境变量置 false 重启，页面显示"这次部署没有启用写操作台账"那一段（含开启方式与"仍有结构化日志"），错误横幅"台账读取失败"未出现、无 toast、页脚计数位显示"未启用"、下一页禁用 |
| 9 | 空台账走"没有记录"文案 | ⚠️ **卡片的"新库 + 只有读请求"在控制台形态下拿不到**：新库首屏第一行就是控制台自己的 `POST /auth/ws_ticket`（16:26:29 实测），它必然进台账。改用 `fetch` 拦截返回一份真形状的空响应（`{"count":0,"total":0,"limit":50,"offset":0,"items":[]}`）验证了这一支文案分支，实测显示"台账里还没有记录 / 台账从启用观测层的这个版本开始记……"；另一支（`actor=nosuchuser` 的真实空响应）显示"当前筛选没有命中 / 放宽条件试试……"。**两支行分开取证，前者是拦截响应不是空库** |
| 10 | 挂机不自动刷新、手动刷新能拿到新行 | ✅（条件打折）在页面里包一层 `fetch` 计数器：基线 `hits=1`、页脚"共 110 条"，随后用 curl 写入 1 条（服务端 `total=121`），静置 22 秒后 `hits` 仍是 1、页脚仍是"共 110 条"、首行时间不变；点"重新查询"后 `hits+1` 且行更新。**测量时标签页是 hidden 状态**（这个浏览器实例只有一个标签页且 `visibilityState=hidden`，无法做成可见），因此按卡片要求标"可见标签页未测"；不受影响的理由是这条结论不依赖定时器（`refetchOnWindowFocus` 在 `plugins/query.ts:24` 全局关掉，本页也没有 `refetchInterval`） |

其它实测到的两处界面现象，不是缺陷但要知道：

- 控制台自身会写台账（`auth.login`、`auth.refresh`、`auth.ws_ticket`），所以这一页的"共 N 条"
  在挂机期间会与屏幕上的行数不一致（行数停在旧值），并且越用越多。见 §10.5 的 D-0806。
- 该浏览器视口 530×554，七列表格横向溢出（`scrollWidth 547 > clientWidth 226`），
  由表格自带的 `overflow-x-auto` 兜住。与任务列表页在窄视口下的既有表现同类。

### 10.5 缺陷处置

| 编号 | 严重度 | 事实 | 处置 |
| --- | --- | --- | --- |
| D-0801 | 中 | 第一版把时间窗折算的 `since` 只放在 `queryFn` 里、不进查询键，实测切换 1 小时/24 小时/7 天/全部 四档的"共 N 条"完全相同（换窗不重取），而 `resetPage()` 又把 offset 归零，用户看到的就是"筛了没用" | **已修**（`web/src/views/AuditView.vue`：`since` 提到 `filters` 计算属性里，因此进 `queryKeys.audit` 的键）；反向验证就是 §10.4 第 4 条那四个互不相同的计数（修复前是 110/110/110/110，修复后 106/107/109/110）。同时放弃了"滚动窗口"这个原本的好处，理由与取舍记在 §10.2 第 3 条 |
| D-0802 | 低 | 卡片 §5.3 第 9 条的"新库 + 只有读请求 → 空台账"在控制台里取不到：加载页面必然先打一次 `POST /auth/ws_ticket`，那一行进台账，新库首屏就是 1 行 | **已按现状取证并记录**：`没有记录` 那一支用 `fetch` 拦截空响应验证（明确标注是拦截不是空库），`筛选太窄` 那一支用真实空响应验证。不改后端（`auth.ws_ticket` 进台账是 S06 的设计决定） |
| D-0803 | 低 | `total` 每次翻页都做一次 COUNT，卡片 §9 提的默认上界是 50 万行 | **登记不修**：界面侧的缓解（翻页不重算）省不掉后端那一次查询，做了只会让读数不一致；真正的解法是 §9 说的 `?total=0`，属后端改动，另立卡片。本卡实测 110 行量级下翻页无可感延迟 |
| D-0804 | 低 | §5.3 第 10 条要求"在可见标签页测"，本环境的浏览器只有一个标签页且始终 `visibilityState=hidden` | **已标注未观测的部分**：结论本身有证据（fetch 计数 + 页脚计数不变），但"可见标签页"这个条件未满足，记在 §10.4 表格里。要补测需要有人工在前台的浏览器窗口 |
| D-0805 | 低 | 卡片 §8 写"S05 的 `GET /jobs/:id/artifacts` 已经接进详情页的输出控件"，实测 `web/src` 没有任何地方 fetch 该端点（`JobExecResult.vue:176` 用的是 `ExecMeta.artifact` 这个状态位） | **登记不修**（本卡不动产物侧）：卡片文字保留原样作为当时的认知，实际状态以这条记录为准。若要接入详情页，是另一件事 |
| D-0806 | 低 | 控制台自身的写请求会污染它自己看的这张表：每次登录、每次刷新令牌、每次 WebSocket 重连都加一行，于是"共 N 条"里有相当比例不是业务动作 | **不修，已文档化**：`auth.*` 进台账是设计文档 §10.2 的既定口径（登录失败正是要看的行）。本页的动作下拉里 `auth.login`/`auth.refresh`/`auth.ws_ticket` 三项可用来把它们筛出去；抽屉里的"身份类型"列能区分 user/machine/anonymous |

### 10.6 未覆盖与已知边界

- 只在 Windows + 一个内置浏览器的窄视口（530px）上走过一遍；没有在任何浏览器里做过
  1440px 以上的排版核对，也没测过表格在超宽视口下的列宽分配。
- 键盘可达性没有专门走查：行是 `<tr @click>`，没有 `tabindex` 与回车触发，
  抽屉的关闭靠 `UiDrawer` 自带的 Esc。这一条与本系列前几页的现状一致（`JobTable` 同样如此），没有额外做。
- 没有做前端单测：`web/` 至今没有测试框架（§5 的前提），验证只有类型、构建与手工走查三层。
- `AUDIT_ACTIONS` 与后端映射表的同步只靠注释与 §5.3 的下拉核对，没有用例守住；
  真正的守门用例在后端侧（`api/audit_test.go` 那条"未配置动作落 other"）。
- 429（`throttled`）与 `profile_unavailable`/`payload_rejected`/`timeout_rejected` 三支执行器结论
  在本轮手工数据里没有出现过（登录限流要 5 次失败、超时上限要造档位），
  它们只有 S06 的 Go 用例覆盖。下拉里能选到它们，界面分支与其它结论同一条路径。
