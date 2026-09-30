# 执行器：进程 / 脚本 / HTTP 任务的可插拔执行层设计

> 状态：**已实现（M0–M5 全部落地）**，实现记录见 `tasks/executor/` 各卡的第 10 节。
> 实施拆分：本设计的 §9 里程碑已拆成 19 张执行卡，见 `tasks/executor/`（索引在该目录的 `README.md`）。
> 冲突处理：卡片里更新的实现细节（例如产物清理的具体策略、加载器的真实作用范围）以卡片为准，本文件是设计依据。
> 偏差标注：照 `web-console-design.md` 的体例，落地与本文件写法不一致的段落以 ⚠️ 就地标注并写明现状与去处，
> 避免读者把设计时的设想当成当前行为。面向使用者的结论一律写在 `docs/api.md` 与 `docs/deployment.md`。
> 引用约定：照 `web-console-design.md` 的体例，本文件按**符号**锚定代码（如 `core/scheduler.go` 的 `dispatch`），不写行号——行号每加一个里程碑就会漂移，符号不会。
> 前置阅读：`docs/design/web-console-design.md` §5.7（角色档位）、§5.6（事件缓冲）。

---

## 1. 背景与目标

`core` 的调度侧已经完整：四叉堆、有界 worker 池、取消表、暂停/强制暂停、调度总开关、重试与 Cron、事件总线、JSON 持久化与崩溃恢复。但**执行侧是空的**——全仓唯一的 Handler 是 `cmd/server/main.go` 的 `registerHandlers`，四个"打日志 + sleep"的示例。也就是说：任务能被准时、可靠、可观测地"叫醒"，但叫醒之后什么也干不了。

目标是补上这一层，让运维在**不改 Go 代码、不重新编译**的前提下，用配置声明一批可执行任务，通过既有的 REST / 文件 / Cron 三条提交路径调用：

- shell 类：`sh` / `bash` / `cmd` / `pwsh` 脚本
- 语言类：`php` / `node` / `python` 脚本文件；`go` / `java` 的**已编译产物**
- HTTP 类：回调、webhook、内部接口触发（含响应捕获）

以及它们必然要求的三样配套：**执行结果的回传通道**、**进程树级别的取消与强制超时**、**远程代码执行的安全边界**。

非目标（本期不做，见 §8）：容器/沙箱级隔离、CPU 与内存配额、分布式 worker、凭据托管与参数加密、审计落盘。

---

## 2. 决策摘要

三条口径由评审确定，其余是本设计的推导结果。全表在这里一次说清，§6 展开实现。

| # | 决策 | 取舍与理由 |
| --- | --- | --- |
| D1 | **只有 profile 白名单，不提供 raw command 字符串** | 配置里声明"命令档位"（name → 解释器/脚本/参数/超时），payload 只能引用档位名并填受约束的参数。等于把 RCE 面从"任何能 POST 的人"收窄到"能改配置文件并重启的人"。代价：加一条命令要改配置重启——与既有账号表 `server.auth.users` 的口径一致，不新增例外 |
| D2 | **脚本文件 + 预编译产物，不接受内联源码** | `php`/`node`/`python` 只跑工作目录内**已存在**的脚本；`go`/`java` 只跑已编译产物（`bin/etl`、`java -jar app.jar`）。"payload 里塞源码写临时文件再执行"是最直白的任意代码执行注入面，而且要求服务器装编译器，两条都不可接受 |
| D3 | **argv 直传，永不经过 shell** | 需要 shell 就在 profile 里写 `runtime: bash` + `script: deploy.sh`，仍然是 `bash` 读一个受审过的文件。这样注入面只剩"argv 伪装成选项"（值以 `-` 开头），用参数校验挡掉，而不是靠转义拼字符串 |
| D4 | **结果摘要进快照，stdout/stderr 全量落外部产物目录** | `JSONFileStore` 的 flush 是**整文件重写**（`core/store.go` 的 `flushLocked`），把 128KB stdout 塞进 `JobSnapshot` 会让 `history_limit: 1000` 变成"每 200ms 重写 128MB"。所以快照只放 exit code / HTTP 状态 / 时长 / 截断标记 / 产物指针，全量输出按 `job_id/attempt` 落 `data/exec/`，带 TTL |
| D5 | **取消要杀进程树，超时必须强制** | `exec.CommandContext` 默认只杀直接子进程，`bash` 派生的孙子进程会变成孤儿继续跑；而 `Job.Timeout` 为 0 表示不限制（`core/job.go` 的 `Timeout`），一个挂死的 shell 会永久占住 worker。因此 profile 级 `default_timeout`/`max_timeout` 是硬约束，不是提示 |
| D6 | **崩溃时正在跑的 exec 任务不自动重跑** | `Restore` 把非终态快照复位为 pending（`core/scheduler.go` 的 `Restore`）——对 `email_send` 无害，对 `rm`/`INSERT`/重复下单是事故。默认 `restore_policy: pause`：崩溃瞬间状态是 `running` 的 exec 任务钉成 `paused`，等人判定。`pending`（从未跑过）照常重排 |
| D7 | **exec 走独立 worker 池，不在 Handler 里加信号量** | 信号量**不能**隔离：Handler 一边等 exec 槽位一边占着主池 worker，100 个 exec 任务照样饿死 `payment_check`。真隔离要在 `dispatch` 处按类别分流到两条队列/两个池。堆保持共享（时间轴与执行资源无关） |
| D8 | **提交 exec 任务默认要求 admin 档；文件投递默认拒绝** | `machine`（静态 token）档位等同 `operator`（`core/auth.go`），因此天然不达标——CI 里泄漏一个 token 不会变成命令执行。文件投递是另一条没有凭据的提交路径：注意服务端二进制今天并没有启用目录加载器（见 §6.9），这道边界是给自行接起 `DirectoryLoader` 的库使用者的 |
| D9 | **参数与输出的读取档位收严** | payload 现在原样出现在任务响应里（`api/dto.go` 的 `JobResponse.Payload`），viewer 可读。profile 可声明 `secret_args`，这些键在响应里掩码；env 注入值**永不**经 API 返回；事件只带摘要不带 stdout 正文（事件会广播给所有 WS 订阅者并进内存缓冲） |

**落地位置**（实现之后补写，方便按决策找代码）：

- D5 ⚠️ 整树终止：`executor/proc_unix.go` 的 `killTree`（`SysProcAttr{Setpgid:true}` + 进程组 TERM→宽限→KILL）与
  `executor/proc_windows.go` 的 `killTree`/`runTaskkill`。超时的生效值由 `executor/profile.go` 的 `effectiveTimeout`
  合成（档位没写就用 `executors.default_timeout`，一律受 `max_timeout` 封顶）。**与原设计的差别**：
  请求里带的 `timeout` 超过档位上限的，在提交期被 `api/handlers_executors.go` 的 `gateExecutorSubmission`
  以 400 `invalid timeout` 拒掉，
  而不是"静默夹到上限后照常入队"——让调用方在提交时看到上限，比执行到一半被中止更好排查（TASK-E16）。
- D6 ⚠️ 崩溃不重放：`cmd/server/main.go` 的 `installRestoreGuard`/`pauseRunningExecOnRestore` 决定装不装守卫
  （`enabled=false` 与 `restore_policy: replay` 都不装），改判动作在 `core/scheduler.go` 的 `SetRestoreGuard`
  与 `holdRestored`。**与原设计的差别**：那段"需人工确认"的说明没有写进 `ExecMeta`，而是写在 `job.paused`
  事件的 `metadata.reason = "restore_after_crash"`（同批带 `forced: true` 与 `attempts`）——崩溃前那几轮的
  `job.started` 只存在于进程内存，重启即丢，来源标记只能落在事件里（TASK-E14）。
- D7 ⚠️ 独立池：`core/scheduler.go` 的 `JobClass`/`RegisterHandlerClass`/`SetExecConcurrency` 与 `dispatch`
  的双队列分流，字段名与 §6.5 一致。另有一条实现期加的能力原设计没写：档位级 `max_parallel`
  （`executor/proc.go` 的 `acquirePermit`），它在一个档位的名额排满时让任务等待而不是开更多协程。
- D9 ⚠️ 落地形态是**参数级**的 `args[].secret: true`（`core/config.go` 的 `ExecutorArg`），不是档位级
  `secret_args` 名字列表；本文余下段落里的 `secret_args` 一律按这一条读。对外表现为
  `GET /executors` 的 `has_secret_args`、任务对象六处出口的按值掩码与 `/result` 的档位收严
  （`api/handlers_executors.go`，细节见 `docs/api.md`）。同一条里还有两处设计没写：
  `args[].allow_dash`（默认不允许值以 `-` 开头，逐参数放开）与"`payload.body` 不参与掩码"
  ——档位没有声明"body 里哪个键是凭据"的能力，要传凭据就走 secret 参数 + `header_allow` 里的同名键。

---


## 3. 现状盘点（规划时基线，已核实）

| 事实 | 位置 |
| --- | --- |
| Handler 签名只有 `error`，全仓无 stdout/result/exit code 概念 | `core/job.go` 的 `Handler` |
| Handler 注册表只有调度器一份，且只能代码注册 | `core/scheduler.go` 的 `RegisterHandler`/`LookupHandler`/`HandlerNames`、`api/server.go` 的 `RegisterJobHandler` |
| REST 请求体**没有** `type` 字段，提交校验按 `req.Name` 查注册表 | `api/dto.go` 的 `CreateJobRequest`、`api/handlers.go` 的 `createJobFromRequest` |
| 执行侧按 `HandlerKey()`（`Type` 优先、回退 `Name`）补绑 Handler；查不到就钉 failed 并发 `no handler registered` | `core/scheduler.go` 的 `executeJob` |
| 失败原因只进事件 `Data`，事件是内存环形缓冲（每任务 100、全局 500），重启即清空 | `core/scheduler.go` 的 `executeJob`（失败分支）、`api/history.go` 的 `historyPerJobLimit`/`historyGlobalLimit` |
| 取消/超时分类：`context.Canceled` 走 `handleInterrupted` 不消耗重试，`DeadlineExceeded` 判 timeout | `core/scheduler.go` 的 `handleInterrupted`、`executeJob` 的 `timedOut` |
| `Timeout=0` 表示不限制，且注释明确"Handler 不检查 ctx 就中止不了" | `core/job.go` 的 `Timeout`、`core/scheduler.go` 的 `executeJob`（超时分支） |
| 单一 worker 池 + 共享队列，Start 后不可调 | `core/scheduler.go` 的 `dispatch`/`SetConcurrency`/`SetQueueCapacity`、`DefaultConcurrency` |
| 存储 flush 是整文件重写（tmp + rename） | `core/store.go` 的 `flushLocked` |
| 终态快照按 `history_limit`/`history_ttl` 淘汰，与堆内条目无关 | `core/store.go` 的 `trimTerminalLocked`、`Delete` |
| `CloneForRetry` 保留原 ID，指数退避改 `RetryDelay` | `core/job.go` 的 `CloneForRetry` |
| `JobStatus` 落盘是 int，新增值只能追加在末尾（既有约束） | `core/job.go` 的 `JobStatus` 常量块注释 |
| 快照字段是加法安全的 JSON 对象（缺键解零值） | `core/job.go` 的 `JobSnapshot`、`core/store.go` 的 `loadFromDisk` |
| 目录加载器按文件里的 `name` 绑 Handler，`name` 即查找键 | `core/load.go` 的 `FileJobFormat`/`HandlerMap` |
| 配置严格解码，未知键启动报错；两份 yaml 的键集合有守卫测试 | `core/config.go` 的 `LoadConfig`、`core/config_test.go` 的 `TestExampleConfigMatchesLocal` |
| 档位判定分两处：路由写死 `RequireRole`，档位取决于 body 的在处理器里 `allowRole` | `api/security.go` 的 `RequireRole`/`allowRole`、`api/handlers_lifecycle.go`（`force-pause` 是现成先例） |
| 访问日志只记 `path` 与 `query`，不记请求体 | `api/logging.go` 的 `requestLogger` |
| 可选依赖用 Option 注入、缺失即 503 的先例 | `api/server.go` 的 `WithGroupStore`、`requireGroupStore` |
| 全仓唯一的 Handler 装配点 | `cmd/server/main.go` 的 `registerHandlers`（被 `run` 调用） |

**结论**：`name` 就是 API 用户能用的执行器键。因此本方案不需要新增"动态注册"机制——启动时把 profile 注册成 `exec.<name>` 的 Handler，提交侧、恢复侧、重试侧全部沿用现有链路。

---

## 4. 总体架构

```
提交（REST name="exec.nightly_report" / 任务文件 / Cron）
        │
        ▼
┌──────────────────────── api 层 ────────────────────────┐
│ createJobFromRequest ── LookupHandler ──┐              │
│                        （既有，不改语义）│              │
│ 新增：executor.Registry 档位校验          │              │
│   · 参数 schema/pattern/secret_args      │              │
│   · 运行时可用性探测（node/php/…）        │              │
│   · required_role 档位（D8/D9）           │              │
│ 新增端点：GET /executors、GET /jobs/:id/result         │
└────────────────────────────┬───────────────────────────┘
                             ▼
┌──────────────────────── core 层 ───────────────────────┐
│ Scheduler（既有：堆/取消表/暂停/重试/事件）              │
│  + JobClass 分流：workCh(default) │ workCh(exec)        │  ← D7
│  + Restore 时 exec+running → paused                     │  ← D6
│  + Permanent() 不重试、只落终态                          │
│  + RuntimeStats 增加 exec 池占用                         │
└────────────────────────────┬───────────────────────────┘
                             ▼
┌──────────────────── executor 包（新增）─────────────────┐
│ profile.go   配置→档位→HandlerKey 渲染，提交期即校验      │
│ args.go      payload 参数校验与 argv 渲染（D1/D3）        │
│ proc.go      进程 runner：argv/cwd/env 清洗/输出限流截断  │
│ proc_unix.go Setpgid + 杀进程组（SIGTERM→宽限→SIGKILL）  │
│ proc_win.go  taskkill /T /F 兜底（Job Object 列二期）    │
│ http.go      方法/URL 模板/host+IP allowlist/响应捕获     │
│ result.go    ExecutionResult + ArtifactStore（D4）       │
│ probe.go     启动期工具链探测与档位可用性                 │
└─────────────────────────────────────────────────────────┘
```

数据流向照控制台那一套：**结果摘要进快照**（所以重启后仍可见、可统计），**输出正文只走产物文件 + 专用端点**（所以列表与 WS 广播不会被撑爆）。

---

## 5. 任务契约与 payload

### 5.1 契约总则

1. 执行器键 = `exec.<profile.name>`，注册进既有注册表；`profile.name` 需符合 `core` 的命名规则，且禁止与已注册 Handler 同名（注册期冲突即启动失败，不静默覆盖）。
2. **所有执行参数只在 payload 里**。`JobSnapshot.Payload` 已落盘，因此崩溃恢复天然可重放，快照结构除 §6.4 的摘要字段外不动。
3. payload 的 `args`/`params` 只能出现 profile 声明过的键，多一个键就 400——白名单模式的全部意义在于此。
   ⚠️ 位置参数的实际写法是保留键 `args._positional`（字符串数组，`executor/args.go` 的 `positionalKey`），
   上限与字符集来自档位的 `positional`；`/executors` 会把这一条透出成 `positional{max, pattern}` 给表单用。
   http 档位的 payload 只认四个顶层键：`params`、`headers`、`body`、`timeout`，写成 `url`/`method`/`cmd` 按非法拒掉。
4. ⚠️ `Job.Timeout` 的归一化与本文写法不同：请求里的值**超过档位 `timeout` 就是 400 `invalid timeout`**，
   不做 `min(…, max_timeout)` 的静默夹取（TASK-E16 的决定，理由见 §2 D5）。生效值由
   `executor.Registry.EffectiveTimeout` 算一次并写进任务，落盘快照与任务详情显示的就是实际会断的那一个。


### 5.2 kind: `script`（php / node / python / bash / sh / pwsh）

```yaml
executors:
  commands:
    - name: nightly_report
      kind: script
      runtime: node                     # 必须是 runtime_allow 里的解释器
      script: scripts/report.mjs        # 相对 workspace，启动时校验存在且在 workspace 内
      args:
        - { name: day, required: true, pattern: '^(yesterday|today|\d{4}-\d{2}-\d{2})$' }
        - { name: level, default: info, pattern: '^(info|debug)$' }
      args_render: ["--day={day}", "--level={level}"]   # 只允许引用上面声明过的键
      cwd: "."                          # 同样 jail 在 workspace 内
      env: { REPORT_HOME: "/srv/report" }              # 固定注入，payload 不得覆盖
      timeout: 10m
      max_parallel: 1
```

```jsonc
// POST /api/v1/jobs  {"name":"exec.nightly_report", "delay":"1h", "payload":{…}}
{ "args": { "day": "yesterday", "level": "debug" },
  "env":  { "TRACE_ID": "abc-123" } }        // 需 profile 声明 env_allow 才能注入
```

最终执行的 argv 形如 `node /workspace/scripts/report.mjs --day=yesterday --level=debug`——**没有 shell 参与**（D3）。要 shell 语义就声明 `runtime: bash` + `script: deploy.sh`，脚本内容本身就是被审过的文件。

### 5.3 kind: `binary`（go / java / 任意编译产物）

```yaml
    - name: etl_full
      kind: binary
      program: bin/etl                  # 已编译产物，必须在 workspace 内
      positional: { max: 3, pattern: '^[A-Za-z0-9._/-]+$' }   # 位置参数：数量上限 + 字符集
      args: [ { name: window, pattern: '^\d{8}$' } ]
      args_render: ["--window={window}"]
      timeout: 30m
    - name: jar_batch
      kind: binary
      program: java
      fixed_args: ["-jar", "app/app.jar"]     # 写死前缀，payload 只能追加受校验的位置参数
```

不提供 `go run` / `javac`（D2）。

### 5.4 kind: `http`

```yaml
    - name: rebuild_index
      kind: http
      method: POST
      url_template: "https://api.internal/v1/tenants/{tenant}/rebuild"
      # URL 占位符仍然声明在 args 里（配置只有一套参数结构），payload 用 params 提供值
      args: [ { name: tenant, required: true, pattern: '^[a-z0-9-]{1,32}$' } ]   # 禁 `/` `?` `#` `@` `..`
      allowed_hosts: ["api.internal"]
      headers: { "X-Source": ["godelayq"] }        # 固定
      header_allow: ["X-Trace-Id"]                 # payload 可覆盖的白名单
      body: json                                   # json | raw | none
      max_body_bytes: 65536
      expect_status: [200, 201, 202]
      capture_response: true
      max_redirects: 0
      deny_private_ranges: true                    # 回环/RFC1918/链路本地/169.254.169.254
      timeout: 30s
```

```jsonc
{ "params": { "tenant": "acme" }, "headers": { "X-Trace-Id": "t-1" }, "body": { "force": true } }
```

SSRF 防护必须在**拨号层**做，不是拼 URL 时做一次字符串判断：profile 的 `http.Client` 用自定义 `DialContext` 钩子，解析出的每个 IP 都要过 `deny_private_ranges`/`allowed_hosts`，否则 DNS rebinding 一次校验形同虚设。

### 5.5 失败语义

| 情形 | 结果 | 是否重试 |
| --- | --- | --- |
| exit 0 / 状态码在 `expect_status` 内 | success | — |
| exit 非 0 且在 `retry_on_exit` 内 / 5xx / 连接失败 / 超时 | failed | 是（沿用指数退避） |
| exit 非 0（默认）/ 4xx / 127 找不到命令 / 权限拒绝 | failed | 否（`Permanent()`，D 见 §6.5） |
| 被 Cancel 或关停打断 | 既有 `handleInterrupted` 语义，不消耗重试 | — |

---

## 6. 后端改动方案

### 6.1 档位与注册（executor/profile.go、cmd/server/main.go）

- 档位配置结构体定义在 `core/config.go`（`ExecutorCommand`/`ExecutorArg`/`ExecutorPositional`，与 `UserConfig`
  等既有配置结构同处）；`executor.LoadProfiles(cfg core.Config)` 只做校验与解析，产出运行期 `Profile`。
  方向固定为 `executor → core`，`core` 不依赖本包（TASK-E12 的 DoD 有断言）。
  `LoadProfiles` 做全部静态校验：name 合法且不与其他档位重名、`script`/`program`/`cwd` 解析后仍在 `workspace` 内
  （绝对路径、`..`、符号链接三种逃逸写法都拒；Windows 上 `filepath.IsAbs("/tmp/x")` 返回 false，
  所以以分隔符开头的写法一律拒绝）、`args_render` 引用的键都已声明、正则能编译、
  `env` 不含 `GODELAYQ_` 前缀、timeout 落在 `[default,max]` 内。**任一项不过就启动失败**，
  与 `LoadConfig` 的"未知键即报错"同一口径。
  文件是否存在、程序是否在 PATH 里属于探测（E03），不在这里让进程起不来；
  与代码注册键的重名冲突属于注册环节（E04）。
- `Register(s *core.Scheduler)` 为每个 profile 注册 `Handler` 闭包（`exec.<name>`），并 `RegisterHandlerClass(..., core.JobClassExec)`。
- `Registry` 独立于注册表暴露给 api 层（`api.WithExecutorRegistry(r)`）。未注入时 `GET /executors` 返回
  `{"enabled":false,"profiles":[]}`——默认关闭不是错误态；只有真正需要读产物文件的
  `GET /jobs/:id/result` 在未注入时才 503（照 `requireGroupStore` 的先例）。
- `cmd/server/main.go` 的 `registerHandlers` 改为"示例 Handler + 配置档位"两部分；`executors.enabled: false` 时一个 `exec.` 键都不注册，`GET /job-types` 与提交侧行为回到今天的样子。

### 6.2 进程 runner（proc.go / proc_unix.go / proc_windows.go）

- `exec.Cmd` + `Env` **重建**：只带 `executors.env_allow` 列出的键（默认 `PATH`/`LANG`/`LC_*`/`TZ`/`HOME`）加 profile 的 `env`。进程 env 里现在有 `GODELAYQ_SERVER_AUTH_JWT_SECRET`、静态 token（`core/config.go` 支持环境变量覆盖），全量继承等于把这些交给子进程。
- 取消：`Cmd.Cancel` 自定义——Unix 用 `SysProcAttr{Setpgid: true}` + `syscall.Kill(-pid, SIGTERM)`，宽限后 `-pid` SIGKILL；Windows 一期用 `taskkill /PID <pid> /T /F`（不加依赖），并在文档写明它与"子进程新派生分支"之间存在竞态，Job Object（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`）列 §9。
- 输出：`stdout`/`stderr` 各一个带上限的 `io.Writer` 落产物文件，**同时**保留滚动尾部用于事件摘要预览；`max_bytes` 达到后停止写入并置 `truncated: true`（继续写会撑爆磁盘，静默丢弃又不诚实）。
- 超时：`context.WithTimeout` + 上面的 `Cancel`；返回 `*ExitError{TimedOut: true}`。

### 6.3 与调度器的衔接（不动语义，只补三处）

- 超时/取消的错误要能被 `executeJob` 认出来：`ExitError` 实现 `Is(target error) bool`，`TimedOut` 时对 `context.DeadlineExceeded` 返回真、`Cancelled` 时对 `context.Canceled` 返回真。这样既有的 `timedOut` 判定与"取消不算失败"分支**不用改**就继续正确工作，`job.failed` 事件的 `metadata.timeout` 也自动准确。
- `Job.Handler` 显式绑定路径不受影响（档位 Handler 由注册表提供）。
- `handleFailure` 的重试判定前加一次 `errors.As(err, &permanent)`（core 侧定义 `interface{ Permanent() bool }`，executor 实现，core 不反向依赖 executor）。

### 6.4 结果与产物（result.go）

```go
// core 新增，仅被快照引用；executor 负责填充
type ExecMeta struct {
    Kind       string `json:"kind"`                 // script | binary | http
    Profile    string `json:"profile"`
    ExitCode   int    `json:"exit_code,omitempty"`
    Signal     string `json:"signal,omitempty"`
    HTTPStatus int    `json:"http_status,omitempty"`
    DurationMs int64  `json:"duration_ms"`
    OutBytes   int64  `json:"out_bytes"`
    ErrBytes   int64  `json:"err_bytes"`
    Truncated  bool   `json:"truncated,omitempty"`
    Preview    string `json:"preview,omitempty"`    // 尾部若干字节，硬上限 inline_preview
    Artifact   string `json:"artifact,omitempty"`   // "available" | "purged" | ""
}
// JobSnapshot 追加：Exec *ExecMeta `json:"exec,omitempty"`
```

- 加法安全：`JobSnapshot` 是 JSON 对象，旧 `jobs.json` 缺 `exec` 键解出 nil；`ToSnapshot`/`FromSnapshot`/`CloneForRetry` 三处搬运要一起补（照 `Group` 字段的既有教训：一个字段四处搬）。
  ⚠️ 落地时 `CloneForRetry` 这一处**故意不搬**：副本代表一次新的执行，留着旧结论会让 `GET /jobs/:id`
  在新一轮还没跑完时就显示上一次的退出码与输出预览（TASK-E05 的结论）。搬运的是前两处。
- ⚠️ 结构体实到多了两个字段：`Permanent`（这次失败不该重试，TASK-E12 的判定依据）；`Signal` 与设计的写法一致。
  另外 `ExitCode` 与 `HTTPStatus` 互斥——http 档位永远不写 `exit_code`（TASK-E15）。
- **不动 `JobStatus` 枚举**——它有 int 落盘错位的既有约束（`core/job.go` 注释），退出码之类的新维度一律走 `ExecMeta`。
- ⚠️ 产物布局：`<output.dir>/<job_id>/a<attempt>.out`、`.err`、`a<attempt>.meta.json`（`executor/artifact.go`
  的 `streamPath`/`metaFileName`；meta 先写临时文件再改名，读侧因此可以认为"存在的 meta.json 内容是完整的"）。
  这一条与设计**不符**：`CloneForRetry` 不搬运 `Attempts`，计数在 `core/scheduler.go` 的 `executeJob` 里从副本的 0
  重新加到 1，所以重试链的文件名始终是 `a1.*`，后一次执行会覆盖前一次的产物，`?attempt=2` 读不到东西
  （`Job.Attempts` 的既有缺陷，已登记在 §11 第 7 条，未修）。也就是说"按 attempt 分文件才看得到重试链各自的输出"
  这句在实现里不成立，`docs/api.md` 与 `GET /jobs/:id` 的 `attempts` 字段都按现状写明。
- 产物索引（TASK-S05 追加）：每次执行的两侧字节数、截断与状态另登记一份在观测库的 `artifact_index` 表里，
  由 `GET /jobs/:id/artifacts` 透出（`docs/api.md`）。定位是**加速器而不是账本**：文件仍是权威，索引写失败
  只记一条日志、不改执行结果，快照里 `exec.artifact` 的取值语义照旧，那张表只是它的第二份记录。
  写侧在两个 Runner 各一处（`executor/proc.go`、`executor/http.go`），删侧跟着 `Purge*` 的实际删除成功
  （先删目录、再删行），另有启动对账把"有行、目录已不在"标成 purged。
  上面那条"重试始终写 `a1.*`"的现状在这里同样成立：同一个 attempt 被二次登记是**覆盖**，
  所以重试过的任务在列表里仍是一行，看不到两条尝试。
- ⚠️ 清扫：实现是 `ArtifactStore.Start`（`executor/artifact.go`）——**启动时同步跑一轮完整扫描**（孤儿目录 +
  TTL 过期），之后每 `purgeInterval`（24 小时）只按 TTL 过期删除，不再重跑孤儿扫描；孤儿清理只做一次的理由写在
  `PurgeOrphans` 的注释里（运行期"存储与堆里都没有"的 ID 可能正在执行，删不得）。"每日跑一次 + 启动跑一次"
  的设想因此偏成"启动完整、周期只按 TTL"。不挂进 store 的 trim 这条保持不变，理由同原设计。


### 6.5 并发隔离（D7）

```go
// core/scheduler.go
type JobClass int
const (JobClassDefault JobClass = iota; JobClassExec)
func (s *Scheduler) RegisterHandlerClass(jobType string, h Handler, class JobClass)
func (s *Scheduler) SetExecConcurrency(n int)   // 同样 Start 后忽略并告警
```

`dispatch` 按 `classOf(job.HandlerKey())` 选队列，两套 worker 各自 `sync.WaitGroup`；`RuntimeStats` 增加 `ExecRunning`/`ExecQueueLen`，`/admin/runtime` 与 `/stats` 一并输出。**默认 `executors.concurrency: 4`**，与主池互不侵占；队列容量沿用"0 = 与 worker 相等"的既有约定。

### 6.6 崩溃恢复不重放（D6）

`Restore` 里加一个回调钩子（core 不 import executor）：

```go
// Scheduler.SetRestoreGuard(func(snap JobSnapshot) (newStatus JobStatus, ok bool))
// executor 注册的守卫：Kind 属于 exec 且 snap.Status == StatusRunning 且 restore_policy == "pause"
//   → StatusPaused + 一条 ExecMeta 说明"崩溃时正在执行，需人工确认"
```

paused 而非 failed 的理由：执行结果**未知**，钉 failed 会在统计里谎报一次失败，而 `Resume` 给了运维一个明确的"我确认可以重跑"动作——与既有"暂停是人为决定，重启不该替用户取消它"的注释口径同构。`Shutdown` 正常打断的路径不经此处（`handleInterrupted` 已把状态落成 pending），所以只有"崩溃瞬间仍是 running"才触发。

### 6.7 配置（新配置段）

```yaml
executors:
  enabled: false                # 总开关：false 时一个 exec.* 键都不注册
  required_role: admin          # 提交 exec 任务的最低档位（machine 等同 operator，天然不达标）
  workspace: ./exec-workspace   # script/program/cwd 的 jail 根
  runtime_allow: [bash, sh, cmd, pwsh, node, php, python, java]
  env_allow: [PATH, LANG, LC_ALL, TZ, HOME]
  concurrency: 4                # 独立执行池（D7）
  queue_capacity: 0             # 0 = 与 concurrency 相等
  default_timeout: 5m
  max_timeout: 30m
  restore_policy: pause         # pause | replay（D6）
  loader_allow: false           # 目录加载器是否接受 exec.*（D8）
  output:
    inline_preview: 2048        # 事件与快照里的尾部预览字节上限
    max_bytes: 262144           # 单流（stdout/stderr）落盘上限，超出置 truncated
    dir: ./data/exec
    ttl: 168h
  commands: []                  # §5 的 profile 列表
```

同步清单（缺一即测试红）：`core/config.go` 的 `Config`/`DefaultConfig`/`Normalized`/`Validate`、`configs/config.example.yaml`、本机 `configs/config.yaml`（`TestExampleConfigMatchesLocal` 守卫，见该文件头部注释的键对应约定）。

⚠️ 三处取值规则与本节写法不同（`core/config.go` 的 `ExecutorsConfig` 注释与 `configs/config.example.yaml` 是权威）：

- `default_timeout` 在 `enabled=true` 时必须是正值，`max_timeout` 超上限的档位与任务请求**直接 400 拒掉**（§2 D5）。
- `concurrency` 与"0 = 用默认值"的既有约定**不同**：`enabled=true` 时显式写 0 会被拒，
  要默认值就留空或删掉该项。这是为了让打开执行器的部署必须明确表态。
- `output.max_bytes` 在 `enabled=true` 时有 1024 的下限；`output.ttl` 写 0 表示"不按时间清理，只做启动时的孤儿清理"。
- 平台细节：Windows 的 `SystemRoot`、`COMSPEC`、`PATHEXT` 与 `PATH` 由执行器无条件透传（子进程启动的最小必需集），
  不必也无法靠 `env_allow` 关掉；`GODELAYQ_` 前缀则无论写不写都被强制排除。


环境变量覆盖不是自动的：`LoadConfig` 里是一张 `BindEnv` 逐键白名单（`server.port`、`scheduler.workers` 等），要让 `GODELAYQ_EXECUTORS_ENABLED`、`GODELAYQ_EXECUTORS_WORKSPACE` 这类标量键可覆盖，必须把它们逐个加进那张列表。`executors.commands` 是嵌套列表，与 `server.auth.users` 同一条限制：不做环境变量绑定，并把理由写进 `LoadConfig` 的既有注释里。解码用 `UnmarshalExact`，所以配置里出现未定义键会直接启动失败。

### 6.8 API 增量

| 端点 | 档位 | 说明 |
| --- | --- | --- |
| `GET /api/v1/executors` | reader | 返回 `exec.*` 键、kind、timeout 上限、参数 schema、`runtime_ok`（探测结果）；**不返回** env 值与脚本绝对路径（只给相对 workspace 的路径） |
| `GET /api/v1/jobs/:id/result` | reader；含 `secret_args` 的档位提至 admin | `?attempt=N&max_bytes=`；`no-store`；只读产物，不进内存缓冲 |
| `POST /api/v1/jobs`、`/batch` | 不变（operator），**处理器内**再判 `exec.` 前缀的 `required_role` | 档位取决于 body，正是 `allowRole` 的既有用法（`api/handlers_lifecycle.go` 的 force-pause 先例） |
| `PUT /jobs/:id` | — | 允许改 `payload`，但改后必须重过 profile 参数校验；不允许把非 exec 任务改成 exec 前缀 |

⚠️ 两行的最终形状以 `docs/api.md` 的"读取执行输出"一节为准（TASK-E07 定稿、E15/E16/E18 各自扩过键）：

- `GET /executors` 的响应除表里那些之外还有：顶层 `enabled`/`required_role`/`max_timeout`
  （`required_role` 在执行器关闭时是 `null`，`max_timeout` 关闭时整个键省略），每个档位
  `reason`、`timeout`、`max_parallel`、`args[]`（含 `pattern`/`secret`）、`positional{max,pattern}`
  （档位没声明位置参数时整个键省略）、`env_allow`（只有变量名）、`has_secret_args`、
  `preferred_result_direction`，以及只在 http 档位出现的 `method`/`header_allow`/`body_mode`/`url`。
  实现里 `env_allow` 与 http 的 `header_allow` 空表给 `[]` 而不是省略，`body_mode` 的空取值归一成 `none`。
- `GET /jobs/:id/result` 的查询参数是四个：`stream`（`out`|`err`，默认 `out`；http 档位的 `err` 是两侧头而不是
  stderr）、`from`（`tail`|`head`，默认 `tail`；`head` 是 http 档位的建议起点，由响应里的
  `preferred_result_direction` 给出）、`attempt`（省略或 0 用快照的 `attempts`，显式给值必须在 `1..attempts`）、
  `max_bytes`（默认 `executors.output.inline_preview` 的 4 倍，上限是单请求 8 MiB 与 `output.max_bytes`
  的较小者，超过单请求上限返回 400）。响应恒带 `Cache-Control: no-store`；产物读不到时是 `200` +
  `found: false` + `meta.artifact: "purged"`（不是 404），404 只表示任务快照不存在，未装配产物存储时 503。
- `POST /jobs` 的 `required_role` 判定与 `PUT /jobs/:id` 的四条约束（禁改名、payload 重跑提交期判定、
  判定顺序 404→改名 400→档位 403→非 pending 409）见 `docs/api.md`；两处都落在
  `api/handlers_executors.go` 的 `gateExecutorSubmission`，`allowRole` 的先例仍是
  `api/handlers_lifecycle.go` 的 force-pause。


事件契约：`job.completed`/`job.failed` 的 `Data` 追加 `result` 摘要对象（exit code / http status / duration / truncated / `preview` 上限 `inline_preview` 字节）。**stdout 正文绝不进事件**——事件会广播给全部 WS/SSE 订阅者，并按 §3 的环形缓冲驻留内存。SSE 的类型白名单（`api/sse.go` 的 `publishedEventTypes`）本期不加新类型，只扩 `Data`。

⚠️ 落地后的事件形状（`core/scheduler.go` 的 `eventData` 与各发布点，字段说明见 `docs/api.md` 的"事件里的执行结论"）：
`Data` 是 `{"error": …, "result": …}`，失败时两者同时出现；`job.failed.metadata` 在既有三项之外**只在永久失败时**
加 `permanent: true`（不写真值以外的取值，为了让既有事件的 JSON 一字不变）；`job.completed.metadata` 是
`{duration_ms}`；`job.started.metadata` 是 `{attempt}`；`job.paused.metadata` 带 `reason`/`forced`/`attempts`（§2 D6）。
`api/sse.go` 的类型白名单确实一字未改。

### 6.9 目录加载器边界

`core/load.go` 在 `HandlerMap` 绑定处之前加一道前缀判断：`exec.` 开头且不允许 → 文件移入 `error_dir` 并记 warn。**理由要写进代码注释**：`job_queue/` 的写权限今天只是"能提交任务"，开了执行器之后它是"能执行命令"，而这条路径上没有凭据。

⚠️ 落地的判断位置与去向（`core/load.go` 的 `rejectExecJob`，TASK-E17）：判断在 JSON 解析之后、绑定 Handler
与入队之前，所以被拒绝的文件既没进调度器也没调用任何 Handler。文件不是"移入"而是**复制一份进 `ErrorDir`
并在末尾追加一行原因**，源文件仍按 `PostLoadAction` 处理（删除策略下由 `handleErrorFile` 顺手删源，避免对
同一个不存在的文件删第二次）；没配 `ErrorDir` 时源文件照常按 `PostLoadAction` 走，`KeepAfterLoad` 会把该文件
记入已处理集合，所以不会每轮扫描重复拒绝同一个文件。日志原文是
`executor job file rejected by the loader`（warn，含文件路径与任务名）。

一条必须澄清的现状：`cmd/server` **并没有启用目录加载器**（`LoaderOptions` 只在 `core/load.go`、`examples/demo2` 与测试里出现，`configs/config.example.yaml` 里也没有加载器配置项——`core/config.go` 的注释说明未实现的选项故意不收录）。所以这条边界是**给库使用者的防护**：谁在自己的程序里把 `Scheduler` 与 `DirectoryLoader` 接起来，谁就继承这道判断；`executors.loader_allow` 的取值由 `Registry` 提供给库调用方读取，服务端二进制本身今天不受它影响。这个前提要写进 `LoaderOptions` 的字段注释，避免误以为已经堵住了服务端的一条提交路径。

### 6.10 控制台增量（web/）

- 任务详情页加"执行结果"卡片：kind/退出码/HTTP 状态/时长/截断标记 + stdout/stderr 尾部折叠块（懒加载 `/result`），`status=failed` 且 `ExecMeta.Permanent` 时给"参数/脚本问题，重试无意义"提示。
- 实时页与列表：`preview` 一行摘要，别把输出塞进表格。
- 任务模板页（`web/src/content/job-template.md`）补 §5 的四类示例，Unix 与 Windows **各一份**（D3 的 argv 差异、`cmd`/`pwsh` 的存在与否都靠它说明）。
- 新建任务表单：`name` 命中 `exec.*` 时按 `/executors` 的 schema 生成 args 输入项并做前端 pattern 校验；`secret_args` 的输入框用密码型控件，回填时显示掩码。**服务端校验才是边界**，与既有的按钮隐藏同理。
- 运维页加 exec 池占用（`ExecRunning`/`ExecQueueLen`）。

---

## 7. 安全模型

| 威胁 | 现状放大点 | 缓解 |
| --- | --- | --- |
| 任意命令执行（RCE） | `executors.enabled` 默认 false；未配凭据时鉴权整体关闭（`core/auth.go` 与 `RequireRole`：未启用鉴权即放行） | D1 白名单 + D8 档位 + `deployment.md` 明确警告：**开执行器的部署必须启用鉴权并配 workspace 独立用户/最小权限运行**，默认关闭不是"可选加固"而是先决条件 |
| CI token 泄漏即 RCE | 静态 token 身份 `machine` = operator 档 | `required_role: admin` 默认把 machine 挡在外面（比较逻辑无需新代码） |
| shell 元字符注入 | — | D3 不经 shell，无字符串拼接 |
| argv 伪装选项（`--` 前缀） | — | 参数 `pattern` 白名单 + `args_render` 模板只允许具名占位符；位置参数需显式声明 `positional` 并给 `max` |
| 路径逃逸（脚本/cwd 指向任意文件） | — | 启动期 `Abs` + workspace 前缀校验；拒符号链接解析后的逃逸；payload 不能指定路径 |
| 内联源码执行 | — | D2 明确不支持 |
| 环境变量泄漏（JWT secret、token 在进程 env） | viper `GODELAYQ_*` 覆盖机制 | 子进程 env 重建 + `env_allow` 白名单 |
| SSRF / 内网扫描 / 云元数据 | HTTP 执行器天然是一个代理 | URL 模板写死 + `allowed_hosts` + **拨号层** IP 校验（含 `deny_private_ranges`、`169.254.169.254`）+ `max_redirects: 0` |
| 参数与输出泄密 | payload 在 `JobResponse` 里对 reader 可见；`jobs.json` 明文落盘 | D9 `secret_args` 掩码、env 值不出 API、`/result` 档位收严；文档明写"白名单不等于保密：命令参数会明文进 `jobs.json`" |
| 磁盘撑爆 | 输出无限 | `max_bytes` 截断 + `ttl` 清扫 + 产物目录独立于 `store.path` |
| 重复副作用（崩溃重放） | `Restore` 无条件复活 running | D6 `restore_policy: pause` |
| worker 饿死 / 挂死进程占坑 | 单池共享 + `Timeout: 0` 无限 | D7 独立池 + D5 强制 `default_timeout`/`max_timeout` |

---

## 8. 明确不做（本期）

容器/gVisor/seccomp 级沙箱；CPU、内存、fd、进程数配额（`ulimit`/cgroup）；`go run`/编译型语言的构建步骤；凭据托管与参数加密；执行结果的长期审计（产物按 TTL 删除，长期留痕请接外部日志）；跨实例的 exec 去重与分布式 worker；写操作审计落盘（沿用 README 已声明的边界：只有结构化日志）。

---

## 9. 实施计划

五个里程碑已全部落地（M0=E01–E04、M1=E05–E07、M2=E08–E12、M3=E13–E14、M4=E15、M5=E16–E19），
逐卡的状态与实现记录见 `tasks/executor/README.md` 的状态表和各卡第 10 节。下表保留规划时的判据原文，
⚠️ 标注的是落地时与判据不同或只做到一部分的项。

| 里程碑 | 内容 | 完成判据 |
| --- | --- | --- |
| M0 | 配置段 + `executor` 包骨架（profile 校验、探测、登记表、注册）+ `cmd/server` 装配 | `enabled: false` 时全仓测试行为零变化；非法 profile 启动即失败并有可读原因；两份 yaml 键同步（守卫测试绿） |
| M1 | `ExecMeta` + `ArtifactStore` + `GET /executors` + `GET /jobs/:id/result` + 事件摘要 | ⚠️ 旧 `jobs.json` 能读；TTL 与孤儿清扫各有测试。**"产物按 attempt 分文件"只做对了 http/首次执行的那一半**：`Attempts` 不被 `CloneForRetry` 搬运，重试仍写 `a1.*`，见 §6.4 与 §11 第 7 条 |
| M2 | script/binary runner + 进程树取消 + 强制超时 + `Permanent()` 重试分类 | ⚠️ Windows 侧"kill 后无残留进程"在实测中未稳定成立（`taskkill /T /F` 非原子，见 §11 第 2 条）；`exit 3`、`sleep 100` + cancel、超时三条链路集成测试有（Windows 实跑，Linux 侧只交叉编译验证） |
| M3 | `JobClass` 双池分流 + `restore_policy` + `RuntimeStats`/`/stats`/运维页字段 | 压测：100 个 exec 任务到期时主池 Handler 仍按时执行；崩溃恢复断言 running→paused、pending→pending |
| M4 | HTTP 执行器（模板/参数/headers/body/响应捕获 + SSRF 防线） | `httptest` 覆盖 2xx/4xx/5xx/超时/重定向/私有 IP/元数据地址/超大响应 |
| M5 | 档位收严（`required_role`、`allowRole`、`secret_args` 掩码）+ loader 边界 + 控制台结果面板与模板 + 文档收尾 | ⚠️ 权限矩阵逐条 403/200 断言、四份文档与 README 更新已完成；`docs/api.md` 写的是超时会 **400 拒掉**而不是夹取，与 M5 判据无关但与 §10 那条措辞相反，以 §2 D5 的标注为准 |


依赖：只有既有依赖 + 可选 `golang.org/x/sys`（若 Job Object 提前到本期）。核心路径不需要新依赖。

---

## 10. 验收清单

验证口径照仓库既有约定：`go build ./...`、`go vet ./...`、`go test ./... -race` 全绿；`go test -run TestExampleConfigMatchesLocal ./core`；前端 `npx vue-tsc --noEmit` + `npm run build`。**注意 `gofmt -l` 在本仓因 CRLF 全量误报**，格式化检查须走既有流程，不要按它的输出判断。

- [x] `executors.enabled: false`（默认）时：`GET /job-types` 与 `GET /executors` 不含任何 `exec.` 项，提交 `exec.*` 返回 400 `unknown job type`，全仓既有测试零改动通过
- [x] profile 校验逐条报错：脚本越出 workspace、`args_render` 引用未声明键、与既有 Handler 同名、timeout 超 `max_timeout`、`runtime_allow` 外的解释器
- [x] 运行时缺失（未装 node/php）时：`/executors` 显示 `runtime_ok: false` 与探测到的路径，提交该 profile 返回 400 且 `details` 说明原因；其余 profile 不受影响
- [x] 参数校验：多传键、值不符 pattern、值以 `-` 开头（未声明 positional）、`args` 非对象 → 全部 400，且在 `POST /jobs` 与 `POST /jobs/batch` 两条路径上行为一致
- [ ] ⚠️ 进程取消：`bash` 脚本内部再 `sleep 600 &`，Cancel 后**孙子进程也消失**。Windows 实测：`bash.exe` 立即消失，
  但一条 `sleep.exe` 活过了它自己的 60 秒——`taskkill /T /F` 的父子快照与派生之间有竞态（§11 第 2 条）。
  Unix 侧（`Setpgid` + 进程组信号）只有单元测试与交叉编译，本机没有可跑 `-race` 的 Linux 环境，**未实跑**。
- [ ] ⚠️ 强制超时：payload 不带 `timeout` 时生效值为 `default_timeout`（已实测）；带超大值时**400 拒掉**而不是夹到
  `max_timeout`（TASK-E16 改了策略，见 §2 D5 的标注）；落盘快照即为生效值
- [x] 输出上限：单流超 `max_bytes` 后停止写入并置 `truncated: true`，`/result` 响应带截断标记；`inline_preview` 在快照与事件两处都不被越过
- [ ] ⚠️ 重试链产物：**未达成**。`CloneForRetry` 不搬 `Attempts`，重试仍写 `a1.*` 并覆盖上一次，`?attempt=2` 读不到
  （登记为 §11 第 7 条，本卡只写文档不改代码）
- [x] ⚠️ 崩溃恢复：任务 running 时 kill -9 进程，重启后该任务为 `paused`（`restore_policy: pause`）且 `ExecMeta` 写明原因；pending 任务照常复活。
  实测成立（本机 Windows 用 `taskkill /F` 强杀留 `running` 快照，重启日志 `paused executor jobs after crash count=1`），
  但"写明原因"的位置不是 `ExecMeta` 而是 `job.paused` 的 `metadata.reason=restore_after_crash`，见 §2 D6 的标注
- [x] 隔离：`executors.concurrency: 1` + 20 个 exec 任务到期，主池 Handler 的执行间隔不受影响；`/admin/runtime` 能看到 exec 池占用
- [x] 重试分类：`exit 1`（默认 permanent）不产生 `job.retrying`；`retry_on_exit: [75]` 的退出码走退避；`RetryDelay` 上限仍受 `scheduler.max_retry_delay` 约束
- [x] SSRF：`http://127.0.0.1`、`http://169.254.169.254`、`http://<内网 IP>`（`allowed_hosts` 未含）、指向私有 IP 的域名（DNS rebinding 模拟）→ 全部拒绝，且拒绝发生在建连之前（`executor/http_test.go` 的 `TestHTTP_Dial_Rebinding`；实跑侧另以 TCP 监听器证明连接根本没到对端）
- [x] HTTP 响应：超大 body 被 `max_body_bytes` 截断；`expect_status` 外的状态码判 failed；重定向默认不被跟随
- [x] 权限矩阵（启用鉴权）：operator/machine 提交 exec → 403；admin → 201；`secret_args` 的值在 `GET /jobs/:id`、`/jobs`、`/executors`、事件流四处均不外泄；`/result` 档位符合 §6.8
- [x] ⚠️ 目录加载器：`loader_allow: false` 时 `exec.*` 的任务文件进 `error_dir` 且不执行（去向是"复制一份带原因的副本"，
  源文件仍按 `PostLoadAction` 处理，见 §6.9 的标注；E17 用 `examples/demo2` 真实跑过）；开启后按 §6.9 的注释可查到风险声明
- [x] env 清洗：子进程内 `env` 打出的清单不含 `GODELAYQ_*`；`env_allow` 增删能生效
- [x] 未启用鉴权（本机开发形态）：所有档位判定视为通过，但 `enabled` 仍是 false，需显式打开——`deployment.md` 的警告段可读到这条因果
- [x] ⚠️ 前端：任务详情页结果卡片在 script/binary/http 三种 kind 下渲染正确（含截断与产物已清理态）；模板页四类示例照抄可跑。
  走查在 Windows 的内嵌形态完成（TASK-E18 第 10 节，逐条结果与"未观测"项都在那里）；`cmd`/`pwsh` 那两条示例走的是
  `kind: binary` + `fixed_args` 的写法（§11 第 8 条的现成绕法）。同一次走查**没有在 Linux/macOS 图形浏览器上复走**：
  本机没有可用的 Linux 图形环境，只有交叉编译产物（TASK-E19 第 10 节）


---

## 11. 风险与后续演进

1. **默认关闭挡不住"顺手打开"**。最大的真实风险不是设计漏洞，而是有人在公网可达、未配凭据的实例上把 `enabled` 拧成 true。缓解：启动时若 `executors.enabled && !auth_enabled` 直接记 **error 级**横幅（与 `allow_credentials` 与 `"*"` 互斥那种启动期硬拦同级），并在 `deployment.md` 把它写成必做项。
2. **Windows 进程树终止非原子**。`taskkill /T /F` 依赖快照式父子遍历，杀的瞬间新派生的分支可能漏网；正解是 Job Object + `KILL_ON_JOB_CLOSE`，列为 M2 之后的加固（需要 `x/sys` 或 `NewLazyDLL` 三函数）。文档与验收清单都要如实标注这一条，别让读者以为两端等价可靠。
   ⚠️ 加固仍未做，且 M2 之后实测坐实了这条风险：TASK-E19 在 Windows 上取消一条派生子进程的档位任务，直接子进程
   `bash.exe` 立刻消失，一条孙子 `sleep.exe` 活过了它自己的 60 秒。`docs/deployment.md` 的"Windows 整树终止"一节
   按这个现场写明：终止是尽力而为，运维要用 `tasklist` 而不是"任务已 cancelled"来判断机器干净。
3. **白名单会让"再灵活一点"的诉求不断回来**（能不能 payload 传脚本路径？传命令行？换个解释器？）。每一次松动都是在重开 D1/D2/D3 挡住的洞。演进方向应当是"更多已声明的档位"，必要时允许 profile 级 `fixed_args` 前缀 + 位置参数的组合，而不是引入 raw 通道。
4. **参数仍会明文进 `jobs.json`**（D9 只解决"不显示"，不解决"不落盘"）。真正的解法是 payload 级加密或凭据托管，本期不做；若走到那一步，产物文件（含 stdout，可能回显参数）必须一起纳入同一保密边界。
5. **至少一次语义的根因在架构里**（单进程内执行 + 快照落盘），D6 只是把"未知结果"这一类从静默重跑变成人工确认。彻底解法是执行租约/幂等键或外部编排（Step/Argo 式），超出本仓库定位。
6. **输出正文与任务快照分处两地**，会漂移（快照在、产物被 TTL 清了）。`ExecMeta.Artifact` 的 `purged` 态与 UI 的降级提示是为此而设，不是装饰。
7. **`Job.Attempts` 在重试链上断掉**（TASK-E19 实测确认，不是推测）。`core/job.go` 的 `CloneForRetry` 不搬运 `Attempts`，
   而 `core/scheduler.go` 的 `executeJob` 每次执行把它加 1，于是重试副本又从头数到 1。后果有三处：产物文件恒叫
   `a1.*` 且后一次覆盖前一次（§6.4）、`GET /jobs/:id` 的 `attempts` 恒为 1（`docs/api.md` 已按现状写明）、
   控制台详情页的"尝试"下拉因此只有一项（TASK-E18 第 10 节"未验证与遗留"第一条）。
   修法要动 core 的字段搬运并同步 `/result` 的 `attempt` 上界，不属于 E19 的文档范围，留作后续任务卡。
8. **`cmd` / `pwsh` 作为 `script` 档位跑不起来**：`script` 形状生成的 argv 是 `[解释器, 脚本路径]`，而这两个解释器
   必须带 `/c`、`-File` 之类的开关才会去执行文件。当前可用写法是 `kind: binary` + `fixed_args: ["/c", 脚本名]`
   （`docs/deployment.md` 第 7 条与 `web/src/content/job-template.md` 都给了可照抄的 yaml）。
   把"解释器的执行开关"做成档位声明的一部分需要扩配置形状与 argv 合成规则，属后续任务卡。
9. **Windows 控制台输出是系统本地代码页**：中文 Windows 上脚本写出的 stdout 是 GBK，产物文件按原样字节保存，
   于是接口与 `meta.json` 的尾部预览把非 UTF-8 字节显示成替换字符。本期只写文档口径（`chcp 65001` 或在脚本里
   重定向编码）；按档位声明代码页转码若要实现就开新卡（TASK-E18/E19 的实测现场如此）。

