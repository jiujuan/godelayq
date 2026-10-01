# 档位在线管理设计（Web 上增删改执行器档位）

> 状态：设计定稿，等待拆卡实施（卡片在 `tasks/web-profile/`，编号 TASK-W01…W09）。
> 冲突处理：本文与 `executor-design.md` 冲突时，那份的**执行侧语义**（payload 校验、argv 直传、超时上限、
> 产物与恢复）仍然有效，只有"档位从哪来、能不能改"这一条被本文改写；§7 逐条列出偏离。
> 引用约定：正文只写 `文件`、`文件:行号` 与既有文档的 `§`，不写"上面那段"这类无法定位的指代。

## 1. 背景与目标

执行器（TASK-E01…E19 已落地）把"能执行什么"钉在 `executors.commands` 上，改一条要重启进程。
使用者已经实测确认这条限制（`web/src/content/job-template.md:272`："改 `executors.commands` 需要重启进程，
页面上不能新增或编辑档位"），并提出了明确诉求：**在 Web 控制台上增删改档位，改完立即生效，重启后仍然在**。

本设计要交付四件事：

1. 一档持久化：档位可以存在仓库之外的一个运行时可写文件里，形态照抄分组（`core/group_store.go`）。
2. 一条构造路径：对"还没落盘 / 刚落盘"的档位定义执行与启动期同一套校验与探测，不存在第二套规则。
3. 一次热生效：注册表与调度器可以在运行期增删条目，且原有的并发承诺不被削弱。
4. 一份来源标注：`GET /api/v1/executors` 说清每条档位来自配置还是页面、能不能改、当前能不能跑。

不在本设计范围内：脚本文件本身的管理（上传/编辑/预览）、按档位分权的细粒度授权、
档位改动的专门审计列。这三项在 §7 与 §11 里登记为后续卡。

## 2. 决策摘要

四条由评审拍板（标 ★），其余是推导结果。全表在此一次说清，§6 展开实现。

| # | 决策 | 取舍与理由 |
| --- | --- | --- |
| D1 ★ | **档位存独立 JSON 文件，不回写 `config.yaml`** | `configs/config.yaml` 含本机凭据且被 `.gitignore` 排除，让服务往它写东西等于把"改凭据"和"改档位"绑在同一个文件上，且要处理注释丢失与并发覆盖。分组已经确立了"低频实体各自一个文件"的先例（`core/config.go` 的 `store.groups_path`，`core/group_store.go:200-222` 的 tmp+rename 全量重写）。代价：档位的真相从此分散在两份地方，靠 §6.5 的合并规则与 `source` 标注收敛 |
| D2 | **整个能力由 `executors.web_enabled` 控制，默认 false** | 与 `executors.enabled`、`observability.enabled` 同一口径：打开前行为与本系列之前一字不差。写端点在关闭时回 503，启动不读文件，`GET /executors` 不给 `editable` 字段。这是"打开它等于把改配置的权限交给 HTTP 请求"这一事实要求的表态。另有一条配套约束：`enabled=false` 而 `web_enabled=true` 属于配置写错，`Validate` 直接拒（与既有的 `loader_allow` 同一条规则，实施时按该先例补上） |
| D3 | **校验只有一条路径：`executor.LoadProfiles`** | 现有签名吃整个 `core.Config`（`executor/profile.go:169-202`），本设计给它加一个可选的宽松入口，而不是复制一份"页面用的小校验"。两份校验迟早分叉，而分叉的表现是"页面上保存成功、重启后启动失败" |
| D4 ★ | **档位在合并后的注册表里全局唯一，同名即拒** | 与启动期已有的查重同源（`executor/register.go:69-74`：键被占用直接启动失败）。写入期冲突回 409，不做"页面覆盖配置"那种两套来源回答同一个问题的形态 |
| D5 ★ | **`web_enabled` 打开后，script/binary 档位允许指向 workspace 之外的本机路径** | 这是本次评审明确同意的**主动偏离**，与原设计 §7 的越界拒绝（`executor/profile.go:867-888`）冲突，详见 §7。`config.yaml` 侧口径不变：绝对路径与 `..` 仍然启动失败。代价与收口方案见 §7.2 |
| D6 ★ | **删除档位时把该类型的未终态任务置 `paused`（默认），正在执行的 attempt 不强杀** | 与重启后的既有行为一致：`cmd/server/main.go:610-612` 已经写明"档位被删掉之后它的历史任务查不到类别，重新排期但在 `executeJob` 里因找不到处理函数直接判失败，外部副作用不会真的再来一遍"。置 `paused` 是把这条结论提前交给人看一眼，而不是替人决定。强杀是 `force-pause` 的既有语义（admin 档），删除档位不该附带它 |
| D7 | **修改档位不允许改 `kind` 与 `script`/`program`** | 这两项是"哪一个可执行体"的身份，换掉等于新建一条档位。允许原地换执行体，会让"这条任务类型上周跑的是 A 现在跑的是 B"在任务留痕里看不出来。要换就删了重建（走 D6 的删除语义） |
| D8 | **改动只影响之后触发的执行，不回溯已在堆里的任务** | `job.class` 在入堆时按注册表盖章（`core/scheduler.go:266-267` 的注释已经把这条说死）。热注册只换注册表里的条目与 handler 闭包，堆与快照都不动 |
| D9 | **`executor.Registry` 改为可变，整表原子替换** | 现在的承诺是"构造之后不再变化，因此读方法不需要加锁"（`executor/registry.go:23`，字段 33-34），而它的读方法全在请求路径上（`api/handlers_executors.go:509、560、594`，`api/handlers.go:697`）。整表 `atomic.Pointer` 替换比逐方法加锁改动面小，且保住"读者永远看到一份自洽的表"这条 |
| D10 | **本期写端点一律 `ops` 档，审计只进既有 `auditActions` 映射** | 门槛取自现成阶梯：删组是 admin（`api/server.go:288`），运维端点是 ops（`:293`）。往执行面注入命令比删组危险，所以取 ops 起步而不是 admin。细粒度授权与"改前改后值"的专门台账列属于 §11 的后续卡，本期不做，但**动作词必须现在登记**——漏配只会落进 `other`（`api/audit.go:260-270`），台账中间件本身零改动（`api/audit.go:239-254`） |

## 3. 现状盘点（规划时基线，已核实）

| 事实 | 证据 |
| --- | --- |
| 档位定义结构体 `core.ExecutorCommand` 只有 mapstructure 标签，没有 json 标签 | `core/config.go:259-319` |
| `LoadProfiles` 吃整个 `core.Config`，逐条 `buildProfile`，重名即错 | `executor/profile.go:169-202` |
| `resolveInside` 拒绝绝对路径、以分隔符开头的写法与 `..`；空 cwd 表示 workspace 本身 | `executor/profile.go:867-888` |
| `Probe` 只查 PATH 与文件系统，不启动进程；http 恒可用；脚本只要求"存在且是普通文件" | `executor/probe.go:44-62、69-78、92-111` |
| `Registry` 构造后不再变化、读方法无锁；`entries`/`keys` 在 `NewRegistry` 里一次填好 | `executor/registry.go:23、33-34、44-85` |
| 注册时按 kind 分流，`handlerFor` 是唯一分流点 | `executor/register.go:84、137-148` |
| 键冲突在启动期是致命错误 | `executor/register.go:69-74` |
| 调度器 handler 表**已有** `s.mu` 保护，但没有任何运行期写入路径 | `core/scheduler.go:269-272、276-279、289-294、303-309` |
| 调度器对"运行期改配置"的既有态度是拒绝：running 时 warn 并忽略 | `core/scheduler.go:249-252` |
| 崩溃恢复按 `HandlerClass` 的第二个返回值判断"这个键还注册着吗"，未注册则不改判 | `cmd/server/main.go:602-624` |
| api 层提交期四道判定集中在一个函数，且必须在 `Schedule` 之前 | `api/handlers_executors.go:547-597` |
| 角色序 viewer<operator<admin<ops，`machine` 折算为 operator | `api/handlers_executors.go:491-499` 读 `RequiredRole`；档位比较入口 `allowRole` |
| 分组是这份设计要照抄的形态：内存 map 权威 + 每次改动同步落盘 + tmp/rename + 损坏即报错 | `core/group_store.go:82-87、91-107、110-130、154-194、200-222` |
| 分组端点的角色阶梯与 503 守卫（未注入 store 时明确拒绝） | `api/server.go:280-289`、`api/handlers_groups_test.go:18` |
| 审计动作映射表与路由表是两处，漏配落 `other` | `api/audit.go:97-118、260-270` |
| 前端能力表与后端路由必须同步 | `web/src/composables/usePermission.ts:4、24-40` |
| 设置页当前明说"改档位要重启、页面不能增删改" | `web/src/views/SettingsView.vue:100-104` |
| 全仓没有任何配置写回代码 | 无 `WriteConfig`/`yaml.Marshal` 命中，`config.yaml` 只被读取 |

## 4. 总体架构

```
              ┌── 启动 ──────────────────────────────────────────┐
 config.yaml ─┤ executors.commands  → LoadProfiles(严格)  → Probe │
              │ web_enabled=true 时 profiles.json → 同一校验(宽松) → Probe │
              └──────────────┬───────────────────────────────────┘
                             ↓ 合并（同名：config 赢，store 条目标冲突）
                    executor.Registry（可变，atomic.Pointer 整表）
                             ↓ 每条 → handlerFor(profile) 闭包
                    core.Scheduler.RegisterHandlerClass / UnregisterHandler
                             ↑
              ┌── 运行期 ────────────────────────────────────────┐
   POST/PUT/DELETE /api/v1/executors/profiles   （ops 档 + 审计）
        ① 结构校验 ② BuildProfile(单条, 宽松路径模式) ③ Probe ④ 冲突检查
        → ProfileStore.Save/Delete（落盘）→ Registry.Apply → Scheduler 注册/摘除
        →（删除时）PauseByHandlerKey 把该类型的**待执行**任务置 paused，运行中的那条不动
              └──────────────────────────────────────────────────┘
```

三条不变量，每张卡的 DoD 都要各守一条：

- **I1 一份规则**：页面写入用的校验与探测，和启动时走的是同一个函数，不接受"页面宽松一点"。
- **I2 落盘先于生效**：先写文件成功，再动内存表与注册表；写文件失败时注册表不动，接口回 500。
  反过来（先生效后落盘）会在进程崩溃后留下"页面上见过、重启后没有"的档位。
- **I3 读者永不自相矛盾**：任何时刻 `Registry` 的一次读取得到的表，其 `entries` 与 `keys`
  必须来自同一次构造（D9 的整表替换就是为了这条）。

## 5. 数据契约

### 5.1 磁盘文件

默认 `./data/exec-profiles.json`，路径由 `executors.profiles_path` 决定（可 `GODELAYQ_EXECUTORS_PROFILES_PATH` 覆盖）。

```json
[
  {
    "name": "nightly_report",
    "kind": "script",
    "runtime": "python",
    "script": "D:/srv/report/main.py",
    "args": [{ "name": "day", "required": true, "pattern": "^(yesterday|today)$" }],
    "args_render": ["--day={day}"],
    "timeout": "10m",
    "max_parallel": 1,
    "retry_on_exit": [75],
    "created_at": "2026-10-01T18:00:00+08:00",
    "updated_at": "2026-10-01T18:00:00+08:00"
  }
]
```

- 顶层是数组，与 `groups.json` 同形（空文件与缺失文件视为空集合；**JSON 损坏必须报错**，
  照 `core/group_store.go:110-130`，不得静默当空集合把页面上建过的档位抹掉）。
- 记录字段与 `core.ExecutorCommand` 一一对应，另加两个时间戳。**不给 `ExecutorCommand` 加 json 标签**：
  那份结构是配置面（mapstructure + YAML 键名守卫 `UnmarshalExact`），存储面与它的演进方向不同，
  新结构 `core.ExecutorProfileRecord` 提供 `Command()` 转换。
- `timeout` 用字符串（`10m`），读取时按 `time.Duration` 解析；存非法值时启动报错并指名档位。

### 5.2 来源与冲突

每条合并后的条目带 `source`：`config`（来自 yaml，只读）或 `store`（来自文件，可编辑）。
同名冲突时 `config` 生效、`store` 那一条**不进注册表**，改由登记表的降级面单独给出
（W03 落地形态：`snapshot.degraded` + `Registry.Degraded()`，而不是塞进 `entries`——
注册键是 map 的键，同名两条无法共存，塞进去要么顶掉配置那条、要么让 `Lookup` 语义二义）。
这一条仍然出现在 `GET /executors` 里（`degraded=true`、`available=false`、
`reason` 写明"与 `executors.commands` 中的同名档位冲突，未注册"），但 `Lookup(key)` 给的是生效那一条。
理由：页面上的一次点击不该让整个进程起不来；而"文件条目让位于 yaml 条目"这个方向与 D4 一致。

`executors.commands` 内部重名、或与代码注册的类型名（`payment_check` 等）冲突，仍然启动失败
（`executor/register.go:69-74` 的行为对 config 侧原样保留）。

### 5.3 写端点契约

| 端点 | 方法 | 档位 | 语义 |
| --- | --- | --- | --- |
| `/api/v1/executors/profiles` | POST | ops | 新建；重名（任一侧）409；校验不过 400（探测不可用不拒，见下面第 2 条） |
| `/api/v1/executors/profiles/:name` | PUT | ops | 修改；`:name` 与体内 `name` 不一致 400；改 `kind`/`script`/`program` 400（D7）；键不存在 404 |
| `/api/v1/executors/profiles/:name` | DELETE | ops | 删除；`?jobs=pause\|block`，默认 pause（D6），其它取值 400（照抄删组的策略白名单） |

- 三个端点都在 `executors.web_enabled=false` 时回 503（"未装配"与"没打开"要能区分：`enabled=false` 也 503）。
- 探测失败**不拒绝保存**（与启动期同口径：文件不存在是这台机器的状态，不是配置错误），
  保存后条目 `available=false`，提交任务时才被 `gateExecutorSubmission` 挡下（`api/handlers_executors.go:560-569`）。
  这是与直觉相反的一条，务必在卡片与界面文案里写明。
- 响应体复用 `ExecutorProfileResponse`（`api/handlers_executors.go:341-354`）加 `source`/`editable` 两字段，
  不新开一种 DTO——前端任务表单与设置页已经在读它。

## 6. 后端改动方案

### 6.1 `core`：档位存储

新增 `core/executor_profile_store.go`，形态照 `core/group_store.go`：

- `ExecutorProfileRecord`（§5.1）+ `Command() (ExecutorCommand, error)`。
- `ExecutorProfileStore` 接口：`List` / `Get` / `Save` / `Delete`，**不含 Flush/Close**（同分组的理由：低频实体每次改动同步落盘）。
- `JSONFileExecutorProfileStore`：`sync.Mutex` + `map[string]ExecutorProfileRecord`（键为档位名小写）+
  `flushLocked`（排序 → MarshalIndent → `.tmp` 0644 → rename）。
- 只校验文件级不变量：名字符合 `profileNamePattern` 同源规则、数组内不重名。
  **字段组合的合法性不在这里判**，那属于 `LoadProfiles`（I1）。

### 6.2 `executor`：构造入口的严格/宽松两模式

`LoadProfiles(cfg)` 保持原签名与原行为（严格模式）。新增单条入口：

```go
func BuildProfile(cmd core.ExecutorCommand, ec core.ExecutorsConfig, mode PathMode) (*Profile, error)
```

- `PathMode` 取 `PathWithinWorkspace`（默认，等价现状）与 `PathAnywhere`（D5：跳过越界拒绝，
  仍要求非空、仍算出绝对路径）。
- `buildProfile`（`executor/profile.go:205`）改为接受 mode 的内部实现，`LoadProfiles` 恒传严格模式，
  于是 yaml 侧一行行为都不变。
- 宽松模式产出的 `Profile`，其 `ScriptRel`/`ProgramRel` 在无 workspace 根可比时取 `relativeTo` 的既有兜底
  （`executor/profile.go:948-954` 已返回 `filepath.Base`）；展示口径见 §6.8。
- 探测与超时合成不需要改：`Probe` 只认绝对路径（`executor/probe.go:69-78`），
  `submissionKeys`/`Render` 只认 `Profile` 字段。

### 6.3 `executor`：可变 Registry

- `entries`/`keys` 合并成一个不可变的 `snapshot` 结构，由 `atomic.Pointer[snapshot]` 持有；
  所有读方法改为一次 `Load()`（D9），删掉 `registry.go:23` 那句注释并换成新承诺：
  "构造与替换都在写锁内完成，读侧永远看到一整份自洽的表"。
- 新增写入侧方法（全部只在 `web_enabled=true` 时被调用）：
  `ApplyStore(items []StoreEntry) error`（`StoreEntry{Profile, Probe, Source, Degraded, Reason}`，
  整批重建 store 部分、config 部分保留；启动与页面写入共用这一条路径）、
  `Lookup`/`Profiles`/`Keys`/`Available`/`ProbeOf` 保持签名不变。
- 来源与降级状态放在登记表的条目上，不放进 `Profile`：`SourceOf(key)` 与 `Degraded()` 两个访问器给接口用
  （W03 落地形态；`Profile` 是档位定义，"来自哪份来源""有没有被降级"是登记表对它的位置判断）。
- `ApplyStore` 在执行器关闭时明确返回错误：那时一个 exec.* 都不会注册，
  往里合 store 条目会造出"接口看得见、调度器跑不了"的半状态。
- `RequiredRole`/`MaxTimeout`/`InlinePreview`/`EffectiveTimeout` 仍读配置，与档位来源无关。

### 6.4 `core`：调度器的热注册与按类型暂停

- `UnregisterHandler(jobType string) bool`（已落地，`core/scheduler.go:292-300`）：写锁内同时删
  `handlers` 与 `handlerClasses`（成对写已在 `:274-275`，成对删是同一处的对称动作）。
  只删一张表的后果写进了方法注释：`HandlerClass` 会回 `(JobClassExec, true)`，
  崩溃恢复守卫据此把一条已经没有处理函数的任务钉成 `paused`，变成永远等不到档位的僵尸任务。
- `PauseByHandlerKey(handlerKey string) (int, error)`（已落地，`core/scheduler.go:924-956`）：
  照 `RetagGroup` 的形状——遍历存储快照、按 `HandlerKey()` 匹配、逐条处置、返回条数。
  处置只覆盖 pending：逐条走 `Pause`（因此复用 `pausePendingJob`，与页面单个暂停同一条路径）。
  正在执行的 attempt **不强杀**（D6，与 `ForcePause` 划清界限）——`Pause` 回 `ErrJobNotPending`
  时跳过、不计入条数，因此调用方不能把返回数当成"这个类型的任务都停了"。
  `paused` 与终态快照先跳过，重复调用返回 0（幂等）。
  `store == nil` 或空白键 → `(0, nil)`，与 `RetagGroup:840-842` 同一条。
  非原子与幂等口径同 `RetagGroup` 的注释。
- `RegisterHandlerClass` 的运行期使用**不新增锁**；`core/scheduler.go:266-267` 那句
  "运行期没有人重新注册同一键"的注释已按事实改写（W04）：运行期确实会重注册与摘除，
  但类别盖章仍只发生在入堆时，堆里的任务不受影响（D8）。
  `SetEventPreviewLimit`/`SetRestoreGuard` 的 running-即-忽略（`:249-252`）保持不动：
  它们改的是全局执行参数，不是注册表，与本设计是两回事。

### 6.5 装配（W05 已落地）

`cmd/server/main.go` 的实际形状：

1. 档位文件走一个独立的依赖闭包 `runtimeDeps.newExecutorProfiles(cfg) ([]core.ExecutorProfileRecord, error)`，
   默认实现打开 `core.NewJSONFileExecutorProfileStore(cfg.Executors.ProfilesPath)` 再 `List()`。
   它与登记表构造分成两个闭包：文件读不出来与档位写错是两种后果（前者挡启动、后者单条跳过）。
   损坏文件的错误文案已落地为"路径 + `fix that file or delete it to fall back to profiles
   declared only in executors.commands`"（路径由存储那层的 `parse executor profile file %q` 带出）。
   `web_enabled=false` 时这个闭包一次都不被调用：不建目录、不建文件，档位文件损坏也与本进程无关
   （用例 `TestRun_WebDisabledNeverTouchesTheProfilesPath`，真实冒烟同口径验过）。
2. 顺序：读文件 → `NewRegistry`（config 侧，一条非法即启动失败）→ `executor.MergeStoreProfiles(cfg, records)`
   → `Registry.ApplyStore(entries)` → `registerHandlers` → `installRestoreGuard` → `Start`。
   合并函数在 `executor` 包里（`executor/merge_profiles.go`），`main.go` 里没有档位规则；
   它只管 store 侧（config 侧仍由 `NewRegistry` 加载），因此 W06 的运行期写入用的是同一个函数。
   注册早于守卫由用例钉住（`TestRun_StoredProfilesAreRegisteredBeforeTheRestoreGuard`
   断言 `register:<键>` < `restore_guard` < `start` 的调用次序，并喂守卫一条 store 档位的 running 快照）。
   `ApplyStore` 返回错误时启动失败：那是"文件里两条记录占同一个注册键"或编程错误。
3. `executor.Register` 确实不需要"跳过 degraded"的分支：降级条目从不进 `Keys()`（W03 的
   `snapshot.degraded`），启动日志因此带出 `degraded=N`（真实进程实测 `total=3 registered=3
   unavailable=1 degraded=1`）。
4. **api 注入留给 W06**：本卡没有任何读取方，提前注入只会多一个没人看的字段与一条没法断言的 503 分支。
   ⚠️ **W06 注意**：本卡的闭包只交回记录、没有交回存储实例，写端点要的是活的
   `core.ExecutorProfileStore`（`Save`/`Delete`）与 `Registry.ApplyStore`，
   因此那一步要把依赖闭包的返回值改成存储本身（或再加一个闭包），别指望这里已有的形状能直接写盘。
   `WithExecutorRegistry` 保持原样（`api/server.go:96-98`）。

### 6.6 写端点与鉴权

处理器 `api/handlers_executor_profiles.go`，每条写请求的固定五步（顺序即 DoD）：

1. `web_enabled` 判定 → 503。
2. 结构体解码：未知键拒绝（与 `UnmarshalExact` 同口径），`:name` 一致性检查。
3. `BuildProfile(..., PathAnywhere)` + `Probe` → 不合法则 400，**错误原文进响应与日志、不进台账**
   （沿用 `api/handlers_executors.go:581-590` 的理由：原文可能带取值）。
4. 冲突与不变量检查（重名 409；PUT 时 D7 的禁改字段 400）→ 落盘（I2）→ `ApplyStore` 热生效 → 删除时 `PauseByHandlerKey`。
5. 回一份 `ExecutorProfileResponse`（含 `available`/`reason`，让调用方立刻看到探测结论）。

鉴权：路由上 `s.RequireRole(core.RoleOps)`（照 `api/server.go:293` 的写法）。
**同时**在 `web/src/composables/usePermission.ts` 的能力表里登记三个能力（那个文件第 4 行的注释
就是在提醒这两处必须一起改）。

### 6.7 审计

`api/audit.go:97-118` 的 `auditActions` 加三行：

```go
http.MethodPost   + " /api/v1/executors/profiles":           "executor.profile_create",
http.MethodPut    + " /api/v1/executors/profiles/:name":     "executor.profile_update",
http.MethodDelete + " /api/v1/executors/profiles/:name":     "executor.profile_delete",
```

动作词集合变了，`AuditActions()` 的导出（`api/audit.go:129-132`）与查询端点的 `?action=` 校验自动跟上，
但**台账查询页的下拉列表**是前端常量，要一起改（W08）。
本期不新增台账列：档位改动的"改前改后值"不在表里，与现有"表里没有请求体列"的口径一致。

### 6.8 `GET /api/v1/executors` 扩展

`ListExecutorsResponse`（`api/handlers_executors.go:341-354`）每条 profile 加：

- `source`: `config` | `store`；
- `editable`: `web_enabled && source==store && !degraded`（W07 实施时补上最后一项：
  改一条没生效的档位等于让人去编辑一份不会被读到的记录，页面上不该给这个入口）。
  这条判断只在 `executor.Registry.List` 里有一份，api 与前端都只读结果。
- `degraded`: bool（§5.2 的冲突条目）；
- `path_display`: 脚本/产物的展示写法（workspace 内是相对路径，越界的是绝对路径原样）。
  ⚠️ W02 实施时核实：**判"在不在 workspace 内"不能看 `ScriptRel` 的形状** ——
  `relativeTo` 只在 `filepath.Rel` 失败（Windows 跨盘符）时兜底成文件名，
  同盘越界会老实给出 `..\..\srv\report\main.py` 这种上跳形式。
  判据只能是 `withinDirectory(workspace, 绝对路径)`，而它目前是 `executor` 包内的函数，
  W07 要在包里给一个只读访问器（例如 `Profile.PathDisplay()`），别让 api 层自己拼。

顶层加 `web_enabled`: bool 与 `runtime_allow`: []string（`executors.runtime_allow` 原样透出）。
后者是给 W08 的"解释器"下拉用的：不透明的文本框会让拼错的解释器一路走到保存之后才由 400 暴露，
而这本来是一行配置就能说清的信息。**`env` 的固定取值仍然不返回**（`web/src/views/SettingsView.vue:61-64` 已钉住这条：
只列键名）；页面写档位的表单因此也不能回显 yaml 侧的 `env` 值，只回填键名。

### 6.9 前端

设置页的只读档位区块（`web/src/views/SettingsView.vue:113-117`）改成"列表 + 新建/编辑/删除"，
新增 `web/src/api/executor-profiles.ts` 与 `components/executors/ProfileForm.vue`。
任务创建表单不用改数据源（它已经在读 `GET /executors`，`web/src/components/jobs/JobForm.vue:89`），
但要把"改档位要重启进程"那句文案换掉（`SettingsView.vue:100-104`、`web/src/content/job-template.md:272`）。

### 6.10 重启与崩溃一致性

- 页面建的档位活过重启（评审拍板）：重启时读文件重建，探测结论按新机器重算。
- 崩溃恢复不受影响：守卫按 `HandlerClass` 查表（`cmd/server/main.go:613-624`），
  而注册发生在 `Restore` 之前——这条顺序是硬约束，W05 的 DoD 要钉住。
- `restore_policy: pause` 对 store 档位同样生效，不需要区分来源。

## 7. 安全模型与本期刻意留下的口子

### 7.1 与 `executor-design.md` §7 的偏离（⚠️ 必读）

原设计把"能执行什么"钉死在三处：workspace 内的脚本、`runtime_allow` 里的解释器、
不接收内联源码（那份文档 D2）。本设计改了**第一处**：

| 项 | 原口径 | 本设计 | 依据 |
| --- | --- | --- | --- |
| `executors.commands` 的 script/program | 必须相对 workspace 且解析后落在其内 | **不变** | `executor/profile.go:867-888` |
| 页面建的档位 | 不存在 | 允许任意本机路径 | 评审拍板（D5） |
| 内联源码 | 拒绝 | **仍然拒绝** | 页面只能引用已存在的文件，不接受脚本正文 |
| 解释器白名单 | `runtime_allow` | **不变**（两种来源共用） | `executor/profile.go:303-313` |

风险从此由"能改配置并重启的人"扩大到"能拿到一个 ops 档 JWT 的人"。具体新增的攻击面：

1. 指到任意可读文件并用白名单解释器执行（例如 `runtime: bash` + `/etc/crontab`）。仍然受解释器语法与
   `Probe` 的"存在且是普通文件"约束（`executor/probe.go:69-78`），但不是受路径约束。
2. 路径存在性探测：400 的具体原因（不存在 / 是目录 / 无权限）会告诉调用者本机文件系统的情况。
   本期统一成一句"文件不存在或不可用"，但保存成功本身就是一次确认。
3. 绝对路径会出现在 `GET /api/v1/executors` 里，而该端点的路由档位是 reader
   （`api/server.go:275`）——viewer 档因此能读到本机目录结构。这是本设计**已知的泄露面**，见 §7.2 S-3。
4. 越界档位不进 git，也就没有任何评审痕迹。唯一的留痕是 `write_audit` 里那一行动作记录（不含值，§6.7）。

### 7.2 收口清单（后续卡，本期只登记不动）

| 编号 | 内容 | 触发条件 |
| --- | --- | --- |
| S-1 | `executors.path_roots` 目录白名单：store 档位的 script/program 必须落在其一（默认只有 workspace） | 生产部署前必须补 |
| S-2 | 细粒度授权（谁能建、谁能删）与档位改动的专门审计列（改前/改后摘要，不含 secret 取值） | 接入真实多租户前 |
| S-3 | `GET /executors` 的绝对路径对 viewer 遮蔽（只给文件名或提到 admin 档才给全路径） | 与 §7.1 第 3 条配对 |
| S-4 | workspace 与 `path_roots` 内的脚本文件管理（上传/编辑/只读预览） | 用户提出时 |
| S-5 | 页面上编辑 `env` 固定取值（本期只列键名，值不可回显也不入库） | 与 D6 的 secret 口径一起判 |

## 8. 明确不做（本期）

- 不改 `executors` 一节的全局参数（`required_role`、`max_timeout`、`concurrency`、`env_allow` 等）——
  它们仍然只能写在 yaml 里。本设计的"在线"只覆盖 `commands` 这一张列表。
- 不做档位级 `required_role`：提交门槛仍是全局一份（`executor/registry.go:49`），
  页面建的档位不能比它更低。
- 不做目录加载器（`loader_allow`）与 store 档位的交互，那条路径今天在服务端二进制里根本没启用。
- 不做草稿态/审批态、不做"改完一段时间后再生效"、不做版本历史与回滚。
- 不做前端隐藏当安全：`usePermission` 只决定渲染，边界是服务端 403
  （`web-console-design.md:767` 那条口径原样适用）。

## 9. 实施计划

| 里程碑 | 内容 | 完成判据 |
| --- | --- | --- |
| M0 基础 | W01 存储与配置键、W02 构造入口两模式 | `ProfileStore` 读写与损坏用例全绿；`web_enabled=false` 时全仓行为零变化 |
| M1 可变注册表 | W03 可变 Registry、W04 调度器热注册与按类型暂停 | `-race` 下并发读写注册表用例绿；`UnregisterHandler` 成对删；`PauseByHandlerKey` 不动运行中 attempt |
| M2 接线与端点 | W05 装配合并、W06 三个写端点、W07 `GET /executors` 扩展 | 起进程 → POST 一条 python 档位 → 立刻提交 `exec.<name>` 任务跑通，全程不重启 |
| M3 界面 | W08 控制台档位管理页 | 界面上建/改/删各一遍，低档位看不到入口且直接调 API 回 403 |
| M4 收口 | W09 文档同步与全量验证 | 四份使用者文档与本设计一致；§7.2 待做项逐条落地成登记 |

依赖：W01→W02→W03→W05；W04 独立于 W01-W03，但 W06 需要 W03、W04、W05；W07 需要 W03；
W08 需要 W06、W07；W09 需要全部。

## 10. 验收清单

1. `executors.web_enabled: false`（默认）：三个写端点全部 503，`GET /executors` 不带 `editable`，
   不创建 `exec-profiles.json`，其余行为与本设计之前逐字节一致。
2. 打开后 POST 一条 `runtime: python` 的档位 → 不重启，`GET /executors` 里它在、
   `source=store`、`available=true`；提交 `exec.<该档位>` 任务能跑到 success 并读到产物正文。
3. 同一档位指向一个不存在的脚本 → 保存**成功**，`available=false` 且 `reason` 可读；
   此时提交任务被 400 `profile_unavailable` 挡下。
4. POST 与 `executors.commands` 同名的档位 → 409，注册表不动。
5. 重启后第 2 条的档位仍在（文件生效），且探测结论按新机器重算。
6. DELETE 一个还有 pending 任务的档位 → 该任务变 `paused`，档位消失，
   之后恢复它 → 因找不到处理函数判失败（与 `cmd/server/main.go:610-612` 的既有口径一致）。
7. PUT 改 `kind` 或 `script` → 400；改 `args`/`timeout` → 200 且下一次执行按新规则。
8. operator 档与 `machine` token 调三个写端点 → 403；ops → 通过。台账里能查到三行动作。
9. `go test ./... -race`、`go vet ./...`、`cd web && npx vue-tsc --noEmit && npm run build`、
   `go build -tags dashboard ./cmd/server` 全部通过。

## 11. 风险与后续演进

| 风险 | 说明 | 缓解 |
| --- | --- | --- |
| 执行面的权限扩大了 | 从"改文件+重启"变成"一个 ops JWT" | §7.2 S-1/S-2 是收口项；本期靠 ops 档 + 默认关闭 + 台账动作行 |
| 档位的真相分两处 | 排查"这台机器能跑什么"要看 yaml 和文件两份 | `source` 与 `degraded` 字段强制把来源说出来；`GET /executors` 是唯一读口 |
| 整表替换的窗口 | 替换瞬间正在处理的请求读旧表还是新表 | `atomic.Pointer` 保证读者只看到一份自洽表（I3）；不承诺"跨方法原子"，页面写入的串行性由 store 的锁给 |
| `handlerFor` 闭包持有旧 `cfg` | 全局参数改了但没重启，store 档位的闭包仍用旧值 | 本期不接受 `executors` 全局参数的在线修改（§8 第一条），风险面为空；将来要支持就得让闭包读 Registry 而不是捕获 cfg |
| 删档位与在跑任务的竞态 | `PauseByHandlerKey` 之后任务可能刚好被 worker 弹出 | 与 D6 一致：不强杀，跑完就落终态；不承诺"删除即中止" |

## 12. 待拍板（写卡时按推荐值落的，执行前请复核）

| # | 问题 | 本文取的答案 | 反过来的代价 |
| --- | --- | --- | --- |
| P1 | store 文件里某条与 yaml 同名（人后来改了 yaml）：启动失败还是条目降级？ | **降级**（§5.2，config 赢） | 选启动失败更"响亮"，但页面上的一次保存之后改 yaml 能让人把整个队列停摆 |
| P2 | `exec-profiles.json` 损坏：拒启动还是忽略该文件继续起？ | **拒启动**（§6.5，照分组先例 `core/group_store.go:110-130`） | 忽略能保命，但会出现"页面上见过的档位凭空消失"的现场，且此后每次写盘都把损坏抹平 |
| P3 | 界面落点：独立视图还是把设置页那段只读区块改造成可编辑？ | **独立视图 `ProfilesView.vue`**（W08 §3.1），设置页保留只读概览 + 一个入口 | 内改造少一个路由与导航项，但档位表单有 12 组字段加一张 `args` 表，塞进设置页会把设置页变成第二个编辑页，还要维护第二套表单 |
