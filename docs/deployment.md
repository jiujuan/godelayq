## 生产环境配置

可配置项定义在 `core/config.go`，通过 `-config` 指定文件路径；留空时按 `configs/config.yaml`
自动查找，文件不存在则使用代码默认值。任何键都可以用环境变量覆盖，前缀 `GODELAYQ_`、
层级用下划线连接（如 `GODELAYQ_SERVER_PORT=9090`、`GODELAYQ_SCHEDULER_WORKERS=32`）。

仓库里入库的是模板 `configs/config.example.yaml`；`configs/config.yaml` 已被 `.gitignore`
排除，部署前先 `cp configs/config.example.yaml configs/config.yaml` 再填凭据。
两份的键由 `core.TestExampleConfigMatchesLocal` 钉住，改一份就要同步另一份。
生产机器上更省事的做法是干脆不放这个文件：全部键都能走环境变量，凭据交给
`EnvironmentFile` 或编排系统注入。

```bash
./godelayq-server -config=/etc/godelayq/config.yaml
```

```yaml
# config.yaml
server:
  port: "8080"                # HTTP 监听端口
  auth:
    token: ""                 # 留空即不启用鉴权；生产必须设置（建议用环境变量注入）
  cors:
    allow_origins: ["*"]      # 跨域来源白名单；["*"] 或留空表示任意来源
    allow_credentials: false  # 与 "*" 互斥，开启前先写具体 origin

scheduler:
  workers: 100                # 并发执行协程数；0 表示 core.DefaultConcurrency
  queue_capacity: 0           # 执行队列容量；0 表示与 workers 相等
  max_retry_delay: 30m        # 指数退避的单次重试延迟上限
  shutdown_timeout: 5s        # 优雅关闭等待时长；开启执行器时建议 >=10s（见"开启执行器"第 4 条）

store:
  type: json                  # 目前仅支持 json
  path: /var/lib/godelayq/jobs.json
  flush_interval: 200ms       # 合并落盘周期；崩溃时最多丢失一个周期的状态
  history_limit: 1000         # 终态快照留痕条数；0 用默认值，-1 不留痕
  history_ttl: 0s             # 终态快照保留时长，如 24h；0 不按时间淘汰
  groups_path: /var/lib/godelayq/groups.json  # 分组注册表（改动即同步落盘）

logging:
  level: info                 # debug|info|warn|error
  format: text                # text|json
```

这份骨架里没有 `executors:`：执行器默认关闭，需要用时照抄
`configs/config.example.yaml` 的那一节，并把下面"开启执行器"一节的每一条落实。

任务状态与分组是**两个文件**：前者按 `flush_interval` 合并写盘（崩溃最多丢一个周期），
后者每次改动同步原子重写。备份或迁移时两个都要带走——只带 `jobs.json` 的话，
任务的 `group` 标签还在，但分组的描述与颜色没了。

两点设计取舍，配置校验会直接拒绝未知键，因此不要照抄旧文档里的其它字段：

- **没有 `read_timeout` / `write_timeout`**：`http.Server` 的这两个超时是按连接生效的，
  而 `/ws`（hijack 后长连接）与 `/sse/events`（持续写）会被它们掐断。
  服务器只固定设置 `ReadHeaderTimeout: 10s`。
- **`loader` / `websocket.max_connections` 尚未实现为配置项**：
  目录加载器需在代码中显式创建（见 `core/load.go`），WebSocket 发送缓冲固定 256 条。

### 日志

全进程统一使用标准库 `log/slog`，输出到**标准输出**（systemd/docker 可直接采集），
由 `logging.level` 与 `logging.format` 控制：

- `level`：低于该级别的记录不写出。排障时临时设 `debug` 可见目录加载器等细粒度日志。
- `format`：`text` 适合人读；`json` 适合日志采集端建索引（每行一个 JSON 对象）。
- 非法取值在启动时就报错，不会静默退回默认。

级别约定：任务生命周期与访问请求为 `info`，4xx 请求、重试排队、发送缓冲丢弃等
可恢复异常为 `warn`，落盘失败、执行失败、panic 为 `error`。
HTTP 访问日志与 panic 恢复由 `api/logging.go` 的中间件产出，替代了 gin 自带日志，
因此 `GIN_MODE` 的 debug 启动横幅仍会打印，但每条请求只走 slog 一份。

各组件（调度器、存储、加载器、WebSocket、API）都可接收注入的 `*slog.Logger`，
未注入时回退到 `slog.Default()`；`cmd/server` 启动时会把按配置构建的日志器设为进程默认，
因此自定义 Handler 里直接用 `slog.Info(...)` 即可与主日志同格式。

### 终态留痕与容量

任务执行成功后，快照不再被直接删除，而是与最终失败记录一起作为终态留痕保存，
`GET /jobs?status=success|failed`、`POST /jobs/:id/retry` 与统计里的 completed/failed 都依赖它。
淘汰只作用于终态记录（`pending`/`running` 永不回收），在每次写入终态快照时顺带完成：

- `history_limit`：保留最近 N 条（按 `updated_at` 排序）。默认 1000，写 `0` 等价默认，
  写 `-1` 表示完全不留痕——回到"完成即删"，此时 completed/failed 计数与历史查询都为空。
- `history_ttl`：超过该时长的终态记录被清掉，`0` 表示不按时间淘汰。

存储是一个 JSON 对象文件，整文件重写，因此留痕条数直接决定文件大小与每次刷盘的开销：
1000 条约几百 KB 到 1 MB 量级。任务量大又只需观测当前状态时，把 `history_limit` 调小、
或配合 `history_ttl` 限定窗口；不需要历史时设 `-1`。

注意两点边界：Cron 重复任务沿用同一 ID，每轮成功都会被下一轮的 `pending` 覆盖，
所以看不到逐轮历史；进程重启后 `uptime` 归零，但 completed/failed 会随留痕记录一并恢复。

### 控制台与前端产物

控制台有两种形态，API 契约一致，生产上推荐第二种（一个进程、一个端口、同源）：

```bash
# 开发形态：两个进程，Vite 代理 /api、/ws、/sse 到 :8080
cd web && npm run dev

# 单二进制：先产前端，再编后端
cd web && npm run build && cd ..
go build -tags dashboard -o godelayq-server ./cmd/server
```

- **顺序不能反**。`-tags dashboard` 下 `//go:embed all:dist` 在编译期就要求
  `web/dist` 存在，缺了它构建直接失败（`pattern all:dist: no matching files found`），
  不会给出一个"看起来成功、打开没有页面"的二进制。发布流水线里两步之间要有依赖关系，
  不能并行。
- **不带 tag 的构建不含前端**：`go build ./...` 与全部测试因此不需要 Node，
  `web.Dist` 恒为 `nil`，访问 `/` 得到 JSON 404。启动日志里的
  `embedded console enabled mount=/` 是判断"这个二进制到底带没带页面"的最快办法。
- **同源之后跨域不再是问题**：`/api`、`/ws`、`/sse` 与页面同一个 Origin，
  浏览器不做预检。`allow_origins` 依然要收紧——它管的是*其它*站点访问这台服务，
  跟控制台从哪来无关。
- **缓存**：`index.html` 是 `no-cache`，`/assets/*` 是 `max-age=31536000, immutable`
  （文件名带内容哈希）。反代或 CDN 上如果给 `index.html` 加了缓存，升级后旧页面会去取
  已经不存在的哈希资源，表现是白屏而不是报错。
- `web/dist` 不进版本库，发布时构建。

### 接入层安全（账号、token 与跨域）

**默认是敞开的**：不配任何凭据时，任何能连上端口的主机都能创建/取消任务并订阅全部事件，
`allow_origins` 默认 `*`，任意网页也能跨域调用。公网或多人环境至少做四件事：

1. 启用鉴权。两种凭据可以并存：
   - **控制台账号** `server.auth.users`：只存 bcrypt 哈希（`go run ./cmd/hashpassword` 生成，
     cost 至少 10），配 `server.auth.jwt.secret` 签发 JWT。哈希写进配置文件是可以的（它不可逆），
     **签名密钥不要落盘**：先用 `go run ./cmd/gensecret` 生成（密钥走 stdout，赋值语句走 stderr，
     `export X=$(go run ./cmd/gensecret)` 拿到的是纯密钥），再用 `GODELAYQ_SERVER_AUTH_JWT_SECRET`
     注入，systemd 下放进 `EnvironmentFile=/etc/godelayq/auth.env`（权限 0600）。
     轮换密钥会让全部已发令牌立即失效，所有人都要重新登录。账号本身不支持环境变量覆盖，
     增删账号需重启进程。这一点连配置热重载也不代劳：`reload.enabled: true` 时改动
     `server.auth.users`（或 `server.auth.token`、`server.auth.jwt.secret`）会让**整次重载被拒绝**，
     一项都不会应用，旧凭据照旧可用——见"配置热重载"一节。
   - **静态 token** `server.auth.token`：给脚本与 CI 用的全局口令，身份是 `machine`
     （能读写任务，没有 admin/ops 能力）。同样优先用 `GODELAYQ_SERVER_AUTH_TOKEN` 注入。
2. 按人分配角色（`viewer`/`operator`/`admin`/`ops`）。最小可用的一组通常是：一个 `ops` 给值班、
   若干 `operator` 给业务、`viewer` 给只看监控的人。机器凭据不给 admin 能力，
   需要自动化做强制暂停/删组时，请为脚本单建一个 `admin` 账号并单独保管其口令。
3. 收紧跨域。把 `allow_origins` 写成实际前端地址列表；需要 Cookie 时配
   `allow_credentials: true`（此时不允许 `*`，否则启动报错）。
4. 前置 TLS。本服务不内置 HTTPS，用 Nginx/负载均衡终结证书后回环转发（见下文反向代理）。
   登录限流按 `c.ClientIP()` 计数，反代必须正确传 `X-Forwarded-For`，
   否则所有请求会被算成同一个来源 IP（详见 gin 的代理信任配置）。

已知边界：

- 令牌与登录态都在内存里。**进程重启 = 所有人重新登录**，登出拒绝表也随之清空
  （重启后旧 access token 依然过不了验签，因为 refresh 表没了、且部署本身已被认为可信边界内）。
- 浏览器页面的 WS/SSE 无法自定义请求头。控制台走一次一用、5 秒过期的 `?ticket=`；
  机器凭据仍可用 `?token=`。后者会出现在访问日志与浏览器历史里，
  暴露给不可信网络前请在反向代理层关掉 `/ws`、`/sse/events` 的 query 日志，或只在内网开放。
- 健康检查 `/api/v1/health` 在保护范围内，探针需要带凭据。
- 带前端产物的二进制里，`GET`/`HEAD` 的静态资源与 SPA 深链**免凭据**（登录页本身就在产物里）。
  放行范围只到产物为止：`/api/`、`/ws`、`/sse/` 三个名字空间与一切写方法照旧要凭据，
  判定见 `api/console.go` 的 `consoleRequest`。产物里不含任何业务数据，但会暴露"这里有个控制台"。
- 写操作台账：未启用观测层时只有结构化日志，没有可查询的审计存储，要留证据链请收集 stdout 日志（见下文日志与观测）；
  打开 `observability.audit.enabled` 之后每个写请求还会落一行 `write_audit`，用 `GET /api/v1/admin/audit` 查
  （见"启用观测层"一节）。两种出口都不记请求体与参数取值。

数据目录需提前创建并保证进程可写：

```bash
mkdir -p /var/lib/godelayq && chown godelayq:godelayq /var/lib/godelayq
```

## 开启执行器

执行器把"能提交任务"扩展成"能执行档位声明的命令"，默认是关的（`executors.enabled: false`）。
打开它之前，下面每一条都要落实——这一节的每条都写明了"为什么"，不要只照着抄配置。

字段全表见 `configs/config.example.yaml` 的 `executors:` 一节，接口行为见 [API 文档](./api.md) 的"执行器 API"。

### 1. 必须先有鉴权

`executors.enabled: true` 且 `server.auth.token` 与 `server.auth.users` 都没配时，进程会记一条
error 级日志后**照常启动**（本机实测原文）：

```
level=ERROR msg="executors are enabled while server authentication is disabled" hint="set server.auth.token or server.auth.users before exposing executors"
```

为什么不直接拒绝启动：测试环境需要在没有凭据的情况下打开执行器跑用例。代价是这条界限交给部署检查，
所以生产部署看到这一行就要停下来补配置——未鉴权的执行端点等于把这台机器的命令执行能力开放出去。

### 2. 运行账号与 workspace 的归属

用最小权限的系统用户跑服务，`executors.workspace` 目录的属主就是这个用户，权限建议 `0750`。

理由：**脚本内容等同于该用户的执行权限**。档位只能引用 workspace 内的脚本与已编译产物
（越出 workspace 的绝对路径、`..`、指向外部的符号链接在启动阶段就报错），所以
"谁能改这个目录"就等于"谁能配置命令"。给这个目录写权限的人不是运维协助，而是拿到了这台机器的执行权。

同理，`runtime_allow` 是解释器与程序名的白名单，加一项就是放开一个可执行程序；
`env_allow` 里 `GODELAYQ_` 前缀的键名会被启动阶段直接拒绝——服务端的 token 与 JWT 密钥就放在进程环境的
`GODELAYQ_*` 里，透传给子进程等于把服务凭据交给脚本。

### 3. 数据文件与产物目录按凭据等级设权限

`store.path` 指向的 `jobs.json` 里有任务参数**原文**；`executors.output.dir`（默认 `./data/exec`）
里的产物文件里有脚本输出原文。权限建议 `0640` 起（服务账号可读写，属组只读，其他用户无权限）。

为什么单列一条：API 层会掩码，磁盘上不会。含 `secret` 参数的档位，读取接口给出的 payload 与输出预览是
`***`（按值替换），但 `jobs.json` 里的 `payload`、产物正文与 `meta.json` 仍是原文；
接口层也没有"整段抹掉"的能力——脚本自己把值打印出来，框架层管不住。
要传凭据给 HTTP 档位，走请求头：把它声明成 `secret` 参数并放进 `header_allow`（请求体没有声明"哪个键是凭据"的能力，不会被掩码）。

还有一条联动容易漏：`executors.required_role` **同时**是提交执行器任务所需的档位和读取含 `secret`
参数档位的产物正文所需的档位（没有另设一个键）。把它降到 `operator` 意味着连带把"输出里可能回显的凭据"
交给 operator 一档，降档的代价不止是放开提交。

### 4. 关停时长要留出收尾时间

`scheduler.shutdown_timeout` 建议 ≥ 10s（代码默认 5s）。

为什么：中止一个执行器任务要"终止进程 + 宽限 + 等待输出管道收尾"三段，后两段固定是 2s 与 4s
（`executor/proc.go` 的 `killGrace` 与 `processWaitDelay`），加起来约 6 秒是处理函数最晚返回的时长。
默认 5s 下，关停会先于收尾触发强制退出。脚本 fork 出去的子进程会继承 stdout/stderr 的写端，
直接子进程退了、拷贝协程还在等一个没人写的 EOF，这条上限就是防止 worker 名额还不回来。

### 5. Windows 的整树终止不是原子的

Windows 上终止进程树用的是 `taskkill /T /F`，它逐个遍历子进程，极端情况下会有派生进程残留。
本机实测：取消一个 `bash` 脚本档位任务（脚本里 `sleep 60 &` 起了子进程）后，`bash.exe` 立刻消失、
`sleep.exe` 又活了约一分钟才自然退出。

彻底方案是 Job Object（把整棵树绑进一个对象，杀对象即杀全树），尚未实现。
在此之前，长驻型的脚本请自己保证子进程会随父进程退出。Linux/macOS 走进程组信号，没有这个问题
（但用 `setsid`/`nohup` 主动脱离进程组的进程同样管不住，这是脚本的行为，不是执行器的漏洞）。

### 6. Windows 的输出编码是本地代码页

控制台程序写出的文本按系统本地代码页编码（中文 Windows 是 GBK），产物文件按**原样字节**保存，
不做转码。因此接口里的尾部预览与 `meta.json` 里的预览遇到非 UTF-8 字节会显示成替换字符。

口径：要么在脚本侧把输出编码统一成 UTF-8（`chcp 65001`，或脚本内部显式按 UTF-8 写），
要么按档位声明代码页后转码——后者尚未实现，需要的话开新任务卡，不要临时在读取路径上加猜测式转换。

### 7. Windows 上脚本档位的参数形状

`cmd`、`pwsh` 这类解释器必须带 `/c`、`-File` 之类的开关才会去执行文件，而脚本档位生成的命令行是
`[解释器, 脚本路径, 参数...]`——没有放开关的位置。Windows 上的可用写法是改用产物档位，把开关写进固定前缀：

```yaml
executors:
  commands:
    - name: cleanup_win
      kind: binary
      program: cmd                    # 必须在 executors.runtime_allow 里
      fixed_args: ["/c", "cleanup.bat"]   # 前缀固定，payload 只能追加被校验过的位置参数
      positional: { max: 1 }
```

`bash`（Git for Windows / MSYS）不受这条影响，可以按脚本档位正常用。参数形状的改造归后续任务卡。

### 8. `deny_private_ranges: false` 只能待在开发机上

HTTP 档位在建立连接之前就判地址：回环、私网、链路本地、`100.64/10`、组播与未指定地址一律拒绝。
把 `deny_private_ranges` 关掉，等于允许档位访问内网与本机服务（SSRF 的正面通路），
所以这个取值只允许出现在开发机上。

启动阶段这种配置是合法的（只有"关掉防线 + 通配主机名"这一组合被直接拒绝），注册完成时会列出这些档位：

```
level=WARN msg="executor http profiles accept private and loopback addresses" profiles=exec.target_ok reason="deny_private_ranges is false" hint="intended for reaching a service on the same development machine; remove it from production configs"
```

生产部署在启动日志里看到这一行就是要去改配置。被拒时的现场线索有三处：一条同主题 warn（含档位名、主机与被拒 IP，
不含完整 URL）、产物 `.err` 末尾一行 `! request failed: address refused by the profile's network policy: ...`、
以及这类失败算永久失败、不消耗重试名额。

HTTP 档位的产物文件里 `a<attempt>.out` 是**对端返回的响应体**，`a<attempt>.err` 是请求与响应的两侧头
（`Authorization`、`Cookie`、`Set-Cookie`、`X-Api-Key`、`Proxy-Authorization` 的值写成 `<redacted>`）。
所以第 3 条的文件权限建议对 HTTP 同样成立：响应体里可能带着对端回显出来的请求参数，
而 `secret` 参数只在请求行与响应读取层打码。

### 9. `restore_policy: pause` 的运维含义

崩溃（`kill -9`、`taskkill /F`、断电）之后重启，默认会把"上次正在执行"的执行器任务停在 `paused` 上等人确认
（`executors.restore_policy: pause`）；`replay` 才是照常重新入队。

判据是存储里的 `running` 快照，这一点最容易误解：强杀来不及写任何结论，所以留下 `running`；
**正常停服不会**——优雅关闭路径把被打断的任务落成 `pending`，重启后照常重跑。
外部副作用的结果未知时不替人决定重复执行，这就是默认 `pause` 的理由。

重启后的现场（本机实测）：

```
level=INFO msg="paused executor jobs after crash" count=1 reason=restore_after_crash
```

`GET /api/v1/stats` 的 `paused` 计数包含它们；逐条有 `job.paused` 事件带
`metadata.reason="restore_after_crash"`、`forced: true` 与 `attempts`。用 `restore_policy: replay` 时
这一条汇总日志不会出现，只有 `restored jobs from store count=1`。

确认与恢复的操作：单条 `POST /api/v1/jobs/{id}/resume`（200，任务回到 `pending`），
批量 `POST /api/v1/jobs/batch-ops` 配 `{"action":"resume","ids":[...]}`（207，逐条结果在 `items`/`errors`）。
恢复后按既有的"触发时间已过就立即补跑"走——确认一次就跑一次，所以先看一眼这批任务的副作用能不能重复。

### 10. 产物保留策略与留痕配对

`executors.output.ttl` 控制产物目录保留多久；清理发生在两处：进程启动时一轮完整扫描（孤儿目录 + 过期目录），
之后每 24 小时只做一次过期扫描（`executor/artifact.go` 的 `purgeInterval`）。

启动那一轮的现场（本机实测，`ttl: 1m`）：

```
level=INFO msg="artifact orphan directories purged" count=3
level=INFO msg="artifact expired directories purged" count=24 ttl=1m0s
```

配对建议：把 `executors.output.ttl` 与 `store.history_ttl`、`history_limit` 一起设。
任务留痕被清掉之后，它的产物目录会在**下次启动**的孤儿清理中被删（"没有对应任务的目录"就是孤儿）；
`store.history_limit: -1`（关闭留痕）时执行器的产物也就只能活到下一次重启。
产物被清掉之后详情页与 `/result` 仍能给出摘要，只是 `found: false`、`meta.artifact` 变成 `purged`，
不是报错。

### 11. 反向代理与结果端点不要缓存

沿用下面"反向代理"一节的既有建议，补一条：`GET /api/v1/jobs/{id}/result` 的响应带
`Cache-Control: no-store`，正文来自磁盘上的产物文件，同一任务的不同尝试、不同读取方向返回的内容都不同。
不要把 `/api` 整体配成可缓存。

### 12. 目录任务加载器不接受执行器任务

服务端二进制**不启用**目录加载器（`core.DirectoryLoader` 只给自行接入的程序用），
所以这一条不影响本仓库的默认部署，别以为改个配置就能让服务端去读任务目录。

对自行接入加载器的程序，默认口径是：任务文件里 `name` 以 `exec.` 开头的会被拒绝。
判断在 JSON 解析之后、绑定 Handler 与入队之前，因此被拒绝的文件既不会进调度器，也不会调用任何 Handler。
理由：这条路径上没有身份凭据，"能往这个目录写文件"如果不拒绝就等于"能在这台机器上执行档位声明的命令"。

被拒绝文件的去向（本机实测）：

- 一行 warn：`level=WARN msg="executor job file rejected by the loader" path=job_queue\exec_try.json job_name=exec.echo reason="executor jobs are not accepted from the loader" hint="set LoaderOptions.AllowExecJobs only when writing into this directory is meant to grant execution"`；
- 配了 `LoaderOptions.ErrorDir` 时复制一份进错误目录，文件名末尾追加 `.error`，副本尾部带一行原因；
- 源文件仍按 `PostLoadAction` 处理（删除或归档）。`KeepAfterLoad` 会把该文件记入已处理集合，所以不会每轮扫描重复拒绝。

打开这条边界的唯一方式是程序显式设置 `LoaderOptions.AllowExecJobs`：配置项 `executors.loader_allow`
只由 `executor.Registry.LoaderAllowed()` 读取，调用方把结果填给加载器；加载器自己不回读配置，
服务端也没有这条链——未接线时这个配置项没有任何行为差别。打开之前请确认监控目录的写权限已收窄到服务账号独占，
并清楚放弃的是哪一层防护。

### 13. 档位的在线管理（`executors.web_enabled`）

默认关闭。打开之后控制台多一个"档位"页、REST 多四个端点，ops 身份可以在**不重启**的前提下
新建/修改/删除档位；这些档位落在 `executors.profiles_path`（默认 `./data/exec-profiles.json`），
重启时与配置的 `executors.commands` 合并，**同名以配置为准**，文件里那条只出现在展示面并标记未生效。

两条硬前提（`core.Config.Validate` 当场拒，不是运行期才发现）：

- `web_enabled: true` 必须同时 `enabled: true`，否则启动即报
  `executors.web_enabled requires executors.enabled to be true`（与 `loader_allow` 同一条口径）；
- 打开之后必须已有鉴权（`server.auth.token` 或 `server.auth.users` 任一），否则这条链路上没有任何身份判定。

**这份文件必须随 `data/` 一起备份**。它是"这台机器能执行什么"的一部分：只备份 `jobs.json`
而漏掉它，恢复出来的进程会少掉所有页面建的档位，而那些任务类型可能正有历史任务挂着。
权限与 `groups.json` 同档（`0640` 起）：里面是脚本路径、固定参数与固定环境变量的**取值**，
按凭据等级设权限，别放进全局可读的目录。

文件形态与故障口径：

- 顶层是 JSON 数组，`created_at` / `updated_at` 可以缺省；**文件不存在是正常状态**
  （还没人在页面上建过档位），启动不会创建它。
- **内容损坏会启动失败**，不是"当成空文件继续跑"——静默清空会抹掉已建好的档位。
  error 日志同时给出路径与自救指引：
  `fix that file or delete it to fall back to profiles declared only in executors.commands`，
  删掉或修好该文件即可退回"只看 yaml"的形态。
- 多机部署请把它当**本机文件**：档位里存的是本机路径，换一台机器探测会失败
  （`GET /executors` 那行的 `runtime_ok` 变 `false`，`reason` 说清是文件不在还是程序不在），
  **这是预期而不是故障**。页面建的档位照常看得见、只是跑不了，提交它会被拒。

一句话警告：**打开 `web_enabled` 等于把"往执行面注入一条可执行命令"的权限交给一个 ops 档 JWT**。
本期档位允许指向 workspace 之外的任意本机路径（设计文档决策 D5），
`executor-design.md` §7 那条越界拒绝只对配置侧的 `executors.commands` 保留。
生产部署前应补设计文档 §7.2 的 S-1（页面侧路径白名单）与 S-2（档位写入的独立凭据/双确认）；
在那之前，请把 ops 档账号的数量与口令强度按"能执行命令"来评估，而不是按"能改配置"。

另外两条与凭据有关的口径：档位里 `env` 的固定取值**不出现在任何读口**
（`GET /executors` 与 `GET /executors/profiles/:name` 都只给键名），也不进写操作台账；
它只落在这份文件里。`PUT` 的请求体不带 `env` 这个键表示"不改"，显式 `"env": {}` 才是清空。

## 启用观测层（可选）

`observability.enabled` 默认是 `false`。打开之后进程多写一个 SQLite 文件，把三类可观测数据落盘：

| 子开关 | 表 | 记什么 | 打开后能多看到什么 |
| --- | --- | --- | --- |
| `events.enabled` | `job_events` | 事件总线广播的 `job.*` | 重启后详情页时间线仍有历史；`GET /jobs/:id/events` 与 `GET /events` 改读库 |
| `artifacts.enabled` | `artifact_index` | 每次执行的输出文件属性 | `GET /jobs/:id/artifacts` 按尝试列出尺寸与清理状态 |
| `audit.enabled` | `write_audit` | 每个写请求一行台账 | `GET /api/v1/admin/audit` 查"谁在什么时候改了什么" |

三个子开关都只在总开关为真时生效。字段与键值说明见 `configs/config.example.yaml` 的
`observability:` 一节与设计文档 [sqlite-observability-design.md](./design/sqlite-observability-design.md)。

### 1. 打开方式，以及"关掉即回到原状"

```yaml
observability:
  enabled: true
  path: ./data/observe.sqlite
  flush_interval: 200ms     # 三个写入器共用的合并落盘周期
```

关闭时（默认）：不建库文件、不订阅事件总线、不注入任何写入器，两个事件端点仍读进程内缓冲。
实测对照：删掉整段 `observability` 后启动，`data/` 下不出现任何 `.sqlite*` 文件，
两个事件端点的响应与接入观测层之前的二进制逐字段一致（TASK-S07 §10.4 场景 1，
基线取的是 `c971f81` 构建出来的程序）。

回滚也是这一条：把 `enabled` 改回 `false` 即停止全部读写，表与既有行留在文件里不影响任何东西；
只关某一个子开关时，那张表照旧建出来但一行不写，对应的读端点给出 503 或退回内存缓冲。
8 种开关组合的逐格实测在 TASK-S07 §10.4 场景 2。

`observability.enabled: true` 而鉴权没配时**不会**像执行器那样报 error——台账与事件库里只会出现
`actor_kind=anonymous` 的行，没有越权风险，所以不额外拦一条启动检查。

### 2. 依赖与二进制体积

驱动是 `modernc.org/sqlite`（纯 Go，无 CGO、无需 C 工具链），版本钉在 `v1.46.0`：
更高版本要求整仓 `go` 指令抬到 1.25/1.26，会连带抬高 `examples/` 的 `go run` 门槛。
驱动只在 `store/sqlite` 这一个包里出现，`core`、`api`、`executor` 都不认识 SQLite
（`go list -deps ./api | grep -i sqlite` 必须为空，这条在 CI 里可查）。

体积代价实测（`windows/amd64`，不带 `-tags dashboard`）：接入观测层之前 32,773,120 B，
接入之后 38,583,808 B，当前 38,813,696 B ⇒ 约 **+5.8 MiB / +18.4%**。
设计阶段估的 +8~10 MB 落在实测之上，按实测记。

### 3. 文件、旁文件与备份

`observability.path` 指向一个单独的文件（默认口径 `./data/observe.sqlite`，与 `store.path` 同目录不同文件）。
WAL 模式会带来两个旁文件，运行期都在：

```
data/observe.sqlite      data/observe.sqlite-wal      data/observe.sqlite-shm
```

**运行中备份请用 `VACUUM INTO`，不要只拷单个 `.sqlite` 文件**——那一刻 `-wal` 里还有未合并的已提交事务，
只拷主文件会丢掉最近的行：

```bash
sqlite3 /var/lib/godelayq/observe.sqlite "VACUUM INTO '/backup/observe-$(date +%F).sqlite'"
```

（本仓库的容量实测就是这么量的：`VACUUM INTO` 一份只读副本再看文件大小，避免读到半成品。）
停服后备份整个数据目录即可，三个文件一起走。定期 `VACUUM` 不做：单连接、批量写、
条数有上界，碎片不是这里的瓶颈（设计文档 §13）。

### 4. 权限

库文件 `0640`、目录 `0750`，与产物文件同一取向；建目录与建文件都由启动装配顺手做掉。
`data/` 若与 `jobs.json` 同级，同一份属主与备份策略覆盖两者。

需要单独设权限的理由：`write_audit` 的行里含**账号名**与拒绝结论，`job_events` 含任务执行结论与输出预览，
这两类信息比任务快照更贴近"谁做了什么"。读取端点的档位也因此不低：`/admin/audit` 取 `ops`（`admin` 也不行），
`/jobs/:id/artifacts` 取 `viewer`（只有元信息，正文另有端点）。

⚠️ 权限位本身在 Windows 上没跑过（用例按平台跳过）。Linux 侧的 `0640/0750` 断言、
`-race` 实跑与优雅停服都还缺一次真机验证——与执行器系列同一限制。

### 5. 持久化强度

`synchronous` 默认 `normal`：WAL + `NORMAL` 下断电最多丢最后几个已提交事务，
与 `store.flush_interval`（JSON 存储的合并落盘周期）"崩溃最多丢一个周期"是同一量级的保证。
需要更强保证改 `full`，代价是每次提交都等磁盘同步、写入变慢。

实测的崩溃现场（`flush_interval: 500ms`，硬杀进程 5 轮）：
每轮丢的都是"最后一个未提交周期内"的行——3 轮丢 0 行、2 轮丢整批 5 行台账（事件侧同理）。
换句话说，**丢掉的上界是一个 `flush_interval` 内攒下的量**，不会出现半行或坏行；
重启后库照样打得开、已落的行一条不少（TASK-S07 §10.4 场景 6）。

### 6. 容量与保留：先看条数上界，再看天数

三张表都是"有界保留"：`retention_count` 按写入顺序淘汰最旧，`retention_age` 按时间淘汰（`0` 表示不按时间淘汰）。
默认值：事件 200,000 条 / 720 小时，台账 500,000 条 / 2160 小时，产物索引没有独立上界（跟着任务删除）。

每行的实际大小（本机实测，`VACUUM INTO` 之后）：500 行事件 + 500 行台账 = 1,000 行，
库文件 311,296 B ⇒ **约 311 B/行**（含八个索引与页开销）。按一次完整执行产生
3 行事件（`job.scheduled` / `job.started` / `job.completed`）+ 1 行台账（那次提交请求）算：

| 吞吐 | 每天新增行 | 每天大约多占 | 默认上界留得住多久 |
| --- | --- | --- | --- |
| 1 万次执行/天 | 4 万 | 12 MB | 事件 5 天 / 台账 50 天 |
| 10 万次执行/天 | 40 万 | 124 MB | **事件约 12 小时**、台账约 5 天 |

也就是说：**量大的部署先撞到条数上界，不是天数**。需要长历史时调 `retention_count`
（这正是它与 `store.history_limit` 的区别：那一个限的是终态快照条数）。
CI 里高频建任务的场景同理——`?action=job.create` 的历史会比预期短。

### 7. 丢弃计数怎么看（这一条与直觉不同）

写入是有界的：队列容量默认 4,096 条，队满时**丢新来的那一条、绝不阻塞请求或调度**。
丢了的条数记在写入器自己身上，但**当前实现没有在线出口**：

- `GET /api/v1/admin/runtime` 里**没有**观测层段落，`DB.Stats()` 也只有三张表行数与 `schema_version`；
- 唯一的现场是**关停时**的一条汇总 WARN，例如实测到的：

```
level=WARN msg="observability event writer dropped records" dropped=497 path=./data/observe.sqlite
```

所以运行一个长期不重启的进程时，你**看不到**它正在缺页；表看起来完整，实际可能少了几百行。
需要主动检查的部署，眼下只有两条路：定期重启看日志，或者自己按 `store/sqlite` 暴露的
`Dropped()` 方法接一层指标。补一个在线出口（`/admin/runtime` 加一段）已登记为缺陷，见设计文档 §15 第 10 条。

同一件事的另一半由响应自己说明：装配了事件库时 `note` 是
`persisted event store; newest entry may lag by the write flush interval`，
说的是"最新一批最多晚一个 `flush_interval`"，不是"丢了"。

### 8. 关掉即回到原状，不影响任何功能

关掉总开关后：事件端点退回内存缓冲、`GET /jobs/:id/artifacts` 与 `GET /api/v1/admin/audit` 各回 503
（503 而不是 404：路由存在，开关没开这件事本身要说清楚）、写请求照旧成功并落一行
`msg="write operation audited"` 的结构化日志。实测的 503 原文：

```json
{"code":503,"message":"write audit log is not configured","details":"start the server with observability.enabled and observability.audit.enabled to enable /api/v1/admin/audit"}
```

观测库文件损坏或被外部删掉也不会连累业务：库打不开时启动失败并给出原因（与档位配置非法同一口径，
不带"记不住"的状态上线）；库能开但某次写失败时只记日志，执行结果与任务状态都不受影响。

### 9. 与 `store.history_limit` / 产物 TTL 的关系

三处保留策略管的是三种不同的东西，**不要指望它们同步**（设计文档 D9）：

| 配置 | 保留的是 | 被淘汰后还能看到什么 |
| --- | --- | --- |
| `store.history_limit` / `history_ttl` | 任务的**终态快照**（`jobs.json` 里的历史条目） | 事件库与台账里仍有它的行，但 `GET /jobs?status=success` 不再给出这条 |
| `observability.events.retention_*` | 任务的**事件时间线** | 任务快照照旧，只是时间线从头开始 |
| `executors.output.ttl` | 输出**正文文件** | `artifact_index` 的行被删掉，`/result` 报 `found: false` |

典型后果：快照留痕被清掉之后，事件库里的时间线还在（这是有意设计，排障时更常回看历史事件）；
反过来"事件已淘汰但任务快照还在"也正常。真要对齐，就按上一节的表把三个上界一起调。

### 10. Systemd 与写权限清单

`docs/deployment.md` 下面那份 Systemd 单元里若配了 `ProtectSystem`、`ReadWritePaths` 或
`ProtectHome`，把 `observability.path` 所在的目录一并加进可写路径——与 `store.path`、
`executors.output.dir` 同一处理。默认值 `./data/observe.sqlite` 在 `WorkingDirectory` 之下，
只有你把它指到别处时才需要动这一项；配错的现场是启动直接失败并报
`sqlite: create dir for ...` 之类的可读原因（实测三种：目录不可写、路径非法、路径指向已存在的目录）。

## 配置热重载（可选）

`reload.enabled` 决定进程要不要盯着配置文件变化（默认 `false`）。**关闭时一个监听器都不建，
进程行为与本节不存在时一字不差**；`reload:` 整节不写就是这个默认。

打开它意味着什么：能在本机改这个文件的人，从此不必重启就能改变并发数、留痕策略与
`executors.commands` 里档位的内容。它把"改配置要重启"这条原本由重启窗口自带的审查环节
去掉了，所以是否打开应当是一次部署决定，而不是排障时的临时手段。

### 1. 三档归属：哪些改了立刻生效、哪些要重启、哪些会被拒绝

判定单位是**叶子键**。下表是本机真实进程逐条实测过的口径（场景表见
[TASK-R07 §10.4](./design/tasks/config-reload/task-r07-end-to-end-verification-and-docs.md)）。

| 改了立刻生效（下一个防抖窗口） | 改了接受但不应用（进 `ignored_keys`，重启才变） | 改了整次作废（进 `rejected_keys`，一项都不应用） |
| --- | --- | --- |
| `logging.level`<br>`scheduler.workers`（运行期扩缩，缩容不打断在跑任务）<br>`scheduler.max_retry_delay`<br>`store.history_limit` / `store.history_ttl`（下一次写入的修剪用新值）<br>`observability.events.retention_*` / `observability.audit.retention_*`（下一个批量周期的淘汰用新值）<br>`executors.commands`（档位内容与条目增删；同名以配置侧为准）<br>`reload.debounce`（下一次事件起用新窗口） | `server.port`、`server.cors.*`、`server.auth.jwt.access_ttl` / `refresh_ttl`<br>`scheduler.queue_capacity`、`scheduler.shutdown_timeout`<br>`store.type` / `store.path` / `store.groups_path` / `store.flush_interval`<br>`logging.format`<br>`observability` 的总开关、`path`、两个 `flush_interval`、`queue_capacity`、`busy_timeout`、`synchronous`、三个子开关<br>`executors` 的总开关、`required_role`、`workspace`、`runtime_allow`、`env_allow`、`concurrency`、`queue_capacity`、`default_timeout`、`max_timeout`、`restore_policy`、`loader_allow`、`web_enabled`、`profiles_path`、`output.*`<br>`reload.enabled`（这一项本身属于重启档） | `server.auth.token`<br>`server.auth.users`<br>`server.auth.jwt.secret`<br>档位条目内的执行许可字段：`kind`、`runtime`、`script`、`program`、`fixed_args`、`cwd`、`env`、`env_allow`、`method`、`url_template`、`allowed_hosts`、`headers`、`header_allow`、`deny_private_ranges`、`max_redirects` |

三条容易读错的口径：

- **拒绝档的代价是"整次作废"**，不是"那一项不生效"。一次存盘里同时改了一个热更键和一个凭据键，
  结果是 `rejected`，热更键也没有应用——旧凭据照旧可用，现网一套取值都不变。
- `rejected_keys` 列的是**当前与权威不一致的全部凭据类键**，不是"这次只改了哪一个"。
  先改 `server.auth.token` 再改 `server.auth.users`，第二次的清单会把两项都列出来
  （比较的对象是"现在生效的那份"，不是"上一次的文件"）。
- 档位那一条键在 `applied_keys` 里是**按条目摊开**的：加一条档位会给出一整套
  `executors.commands.<档位名>.<字段>`，删掉一条时额外再给出容器路径 `executors.commands`。
  读数里不会出现光秃秃的一条 `executors.commands`（新增/删除时）。

### 2. 怎么看上一次重载的结论

`GET /api/v1/admin/runtime`（ops 档）响应里的 `reload` 对象，十个字段：`enabled`、
`watched_path`、`last_attempt_at`、`last_applied_at`、`result`、`error`、
`applied_keys`、`ignored_keys`、`rejected_keys`、`watcher_error`。

- `result` 只有五个取值：`ok`、`unchanged`、`rejected`、`failed`、`degraded`。
  这段读数是**按次替换的快照**，只说最近一次尝试：一次 `ok` 之后再来一次 `rejected`，
  `last_applied_at` 与 `applied_keys` 都会消失，不是"从没应用成功过"。要历史翻日志。
  `unchanged` 是"存盘了但取值等价"，不算一次生效；`degraded` 只在**回滚自己又失败**时出现。
- `result` 是空串表示"开关开着，但从没尝试过"；整个 `reload` 键不出现表示这台部署没打开热重载。
- 时间字段是 RFC3339Nano 字符串，零值不给键。
- 一次失败的重载会在日志里留下**两条** error 级记录：重载链自己按失败原因记一条（带步骤与
  `hint`），监听器把返回的错误再记一条（带 `path`）。两条归因不同，按事件计数时请按
  `path` + 时间窗去重，不要按 error 行数告警。

本系列**没有新增任何端点**：`/admin/runtime` 仍是 ops 档，读端点仍不进写操作台账。

### 3. 运维提示

- **改配置前先在版本控制里留一份。** 坏文件（语法错、未知键、缩进错、被删）与**不再表达任何取值**的
  文件都会被拒绝，进程一直停在旧配置上跑——这是设计行为，但也意味着"你以为改了，其实没改"。
  修好文件后下一个窗口自动恢复，不需要重启。
  "不再表达任何取值"这一类要说全：只剩空白、只剩注释、只剩 `---` / `...` 文档标记
  （**带 BOM 或用 UTF-16 写出来的同一种也算**——PowerShell 的 `>` 默认就写带 BOM 的 UTF-16）、
  只有键名而没有值（`logging:` 后面空着、`scheduler:` 下面只有 `workers: null`、`logging: {}`、
  整份文件就是一个 `null`），以及把值写在第二份文档里（这一份配置只读第一份文档）。
  这样的文件读出来是一份合法的"全部取默认值"的配置，不拦的话一次误清空就会把整台机器静默换成默认值。
  反过来，**显式写出来的空列表**（比如 `executors.commands: []`）是"这一族清空"的正常取值，不会被拦；
  而文件里**没写的其他键**照样会退回默认值并生效——那是"整份文件是唯一真相"的语义，见下一条。
- **"把某个键从文件里删掉"不等于"让它空着"**：重载比较的是整份文件，没写的键会退回默认值并生效。
  要让某一项回到默认，请显式写出它的默认取值。
- `reload.enabled` 改了不生效（它是重启档），所以"运行期关掉监听"只能靠重启。
- 进程启动时没读到可监听的文件（不给 `-config` 且 `configs/` 下没有 `config.yaml`/`config.yml`）
  会记一条 warn 并照常服务，不建监听器。`core.LoadConfig` 认 `.json`/`.toml` 等多种拼写，
  而监听器只认 `.yaml`/`.yml`：用 `configs/config.json` 这类部署打开热重载，会得到
  "配置读到了、但没有文件可盯"的组合（warn + 不建监听器），改那个 `.json` 永远不会有反应。
- **没有配置文件时，`GODELAYQ_*` 环境变量也不会生效**（实测，R07 场景 4A）：`LoadConfig` 只在
  "读到配置文件"那一条分支上做精确解码，环境变量是绑在那次解码上的，所以一份文件都没有的进程
  按代码默认值跑，`GODELAYQ_RELOAD_ENABLED=true` 也换不来监听器。要让环境变量起作用，
  目录里得先有一份配置文件。
- 缩容 `scheduler.workers` 之后，`/admin/runtime` 的 `scheduler.workers` 立刻是新值，
  而真正在跑的协程要等任务重新流动才收敛；`scheduler.queue_capacity` 是启动期建通道时定下的，
  运行期扩缩都不换通道，读数不跟随 `workers`。

## Systemd 服务配置

```ini
# /etc/systemd/system/godelayq.service
[Unit]
Description=godelayq Delayed Task Scheduler
After=network.target

[Service]
Type=simple
User=godelayq
Group=godelayq
WorkingDirectory=/opt/godelayq
ExecStart=/opt/godelayq/godelayq-server -config=/etc/godelayq/config.yaml
# token 不落盘进 config.yaml，单独放 0600 权限的环境文件
EnvironmentFile=-/etc/godelayq/token.env
Restart=always
RestartSec=5

# 资源限制
LimitNOFILE=65535
MemoryLimit=2G

# 优雅关闭
TimeoutStopSec=30
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
```

收到 `SIGTERM` 后进程按序关停：停止接受新连接 → 取消所有请求上下文（SSE 长连接随即返回）→
关闭全部 WebSocket 客户端并等待读写协程退出 → 停调度器（取消在途任务的 `context`，等待 worker 返回）→
观测层收尾（撤事件订阅 → 把队列里剩下的行落成最后一批 → 关观测库，仅在启用时存在）→
最后一次落盘任务存储。

其中**只有 HTTP/长连接这一段受 `scheduler.shutdown_timeout`（默认 5s）约束**：超时后强制关闭残留连接并继续收尾。
`Scheduler.Stop()` 没有自己的超时——它依赖 Handler 检查传入的 `context`；
一个无视取消的处理器会把关停无限期挂住，此时由 systemd 的 `TimeoutStopSec` 发 `SIGKILL` 兜底，
因此 `TimeoutStopSec` 要留得比 `shutdown_timeout` 宽裕，并按最坏的 Handler 收尾时间设定。

启用服务：

```shell
sudo systemctl daemon-reload
sudo systemctl enable godelayq
sudo systemctl start godelayq
sudo systemctl status godelayq
```

## Nginx 反向代理（SSL）

```shell
upstream godelayq {
    server 127.0.0.1:8080;
    keepalive 32;
}

server {
    listen 443 ssl;
    http2 on;                       # nginx 1.25+；旧版仍写 listen 443 ssl http2
    server_name scheduler.example.com;

    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    # SSE：必须关掉代理缓冲，否则事件会攒在 nginx 里不往外发
    location /sse/ {
        proxy_pass http://godelayq;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding on;
        read_timeout 0;             # 长连接不主动掐断
        access_log off;             # 避免 ?token= 进访问日志
    }

    location / {
        proxy_pass http://godelayq;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 86400;   # WebSocket 长连接
        access_log off;             # /ws 同理：握手带 ?token= 时不要落日志
    }
}
```

服务端只看 `Origin` 头做握手来源校验，因此 `allow_origins` 要写**面板自己的 origin**
（如 `https://scheduler.example.com`），不要写成上游地址。