# TASK-R07　端到端场景实测、文档同步与系列收口

- 所属阶段：M3 收口
- 依赖任务：R01–R06 全部
- 涉及文件：`docs/design/config-reload-design.md`（加"落地位置"与偏离标注）、
  `docs/design/tasks/config-reload/README.md`（状态表与本卡记录）、
  `README.md`、`docs/deployment.md`、`docs/api.md`、`docs/example.md`、
  `web/src/content/job-template.md`、`configs/config.example.yaml`（只改注释）、
  `configs/config.yaml`（只改注释）、本目录七张卡的 §10 补齐核对
- 预计规模：中（**零生产代码改动**为目标；允许为补场景缺口动测试文件）

## 1. 任务目标

把 R01–R06 交付的东西在真实进程里跑一遍完整场景表，逐条实测并记录，
然后让使用者文档与设计文档说的是同一件事：哪些配置项改了立刻生效、哪些仍要重启、
哪些改动会被整次拒绝。收口时把"设计—实现—实测"三者之间的差异逐条标注，不留悬空说法。

## 2. 背景与当前问题

前五张卡的验收都是**包内**的：单元与集成用例证明零件正确，R06 的冒烟只跑了五行。
剩下的风险全部在"合起来跑"这一层：

- 监听链路端到端只有 R05/R06 各自的一段，中间隔着真实的文件系统事件、真实的编辑器写法。
- 文档里有六处旧说法现在只有一半对：
  `README.md:300`、`docs/api.md:515` 与 `:1618`、`docs/deployment.md:136`、
  `web/src/content/job-template.md:272` 都写着"改 `executors.commands` 要重启进程"。
  热重载打开时这句对配置侧不再成立；而 `docs/deployment.md:136` 的"增删账号需重启进程"
  仍然成立（拒绝档）。混在一起读，使用者会得出一句错的、一句对的都分不清。
- 设计文档 §12 那张风险表里的四条，只有两条被前面的卡实测过。

## 3. 要实现的功能

### 3.1 端到端场景表（本卡的交付物主体）

每条都要在一个独立目录里实跑（配置 + `data/` + 产物目录 + 独立端口），
判据一律取自日志、`/admin/runtime` 的 `reload` 对象与 REST 响应
（**卡面原来还写了 `/api/v1/pools`，本仓没有这个端点，实测 404——并发/队列读数一律改取
`/admin/runtime` 的 `scheduler` 对象，登记为 D-R0701**），
**不允许**用"看起来任务跑得快了"当判据。跑完删除临时目录。
（执行时的现场修正：`%TEMP%` 下的目录在本轮中途被系统回收，22 条全部在
`D:\codeproject\mygo\r07smoke\` 上重跑，见 §10.4 开头的说明。）

| # | 场景 | 判据 |
| --- | --- | --- |
| 1 | `reload.enabled` 缺省（不写这一节） | 进程起来后 `/admin/runtime` 无 `reload` 键、改文件没有任何反应（等三个窗口）。**goroutine 数那一半在真进程里读不到**（本仓没有 pprof、也没有计数端点，本卡不许加端点），登记为未观测，见 §10.6 乙 |
| 2 | `reload.enabled: true` + `debounce: 10ms` | **启动即失败**，错误文案含 `reload.debounce`（R01 的下界） |
| 3 | `enabled: true` 且 `-config` 指向不存在的文件 | 启动失败（显式指定的文件缺失是错误，既有口径未变），不产生"盯着空气"的监听器 |
| 4 | `enabled: true` 但走默认值部署（没有 `configs/config.yaml`，也不给 `-config`） | 拆成两条实测（D-R0706）：4A 没有文件时**环境变量整组不生效**（`LoadConfig` 只在读通文件那一支做 `UnmarshalExact`），所以"用环境变量打开热重载"这条路不存在；4B 有文件但拼写不是 `.yaml`/`.yml`（放 `configs/config.json`）→ 起服务、不建 watcher、日志里一条 warn 说明"用默认值运行，无文件可盯"（R06 §5.2 #13），`reload` 读数另带一条 `watcher_error` |
| 5 | 改 `logging.level: info→debug→error` | 两次都 `result==ok`；debug 期间能看到 debug 行、error 期间只剩 error；`applied_keys` 各含该键 |
| 6 | 改 `scheduler.workers: 4→12→2` | `/admin/runtime` 的 `scheduler.workers` 随两次改动变（原文写的 `/api/v1/pools` 不存在，D-R0701）；缩到 2 时在跑的普通任务一条都不失败（提交 6 条 `payment_check`，每条在跑 2s——`cmd/server/main.go:956` 的 `time.After(2 * time.Second)`，原文写的"各睡 1s"与代码不符），全部 `success` |
| 7 | 改 `scheduler.queue_capacity` | `result==ok`、`ignored_keys` 含该键、`/admin/runtime` 的队列容量读数**不变**（原文写的 `/pools` 同上）（下次重启才变） |
| 8 | 改 `store.history_limit: 1000→5` | 提交 8 条会很快终态的任务，`GET /api/v1/jobs?status=success` 的条数受新上限约束（`/stats` 的 completed 同步）；`applied_keys` 含该键 |
| 9 | 改 `observability.*.retention_*`（观测层启用时） | `result==ok`、`applied_keys` 含该键；下一个批量周期的淘汰按新值（构造一次超过新条数的事件量，查 `GET /api/v1/events` 的行数上界变化） |
| 10 | 观测层**未启用**时改同样两个键 | `result==ok`、两键进 `ignored_keys`、不失败（R06 §3.2 #5） |
| 11 | 改 `executors.commands` 增一条档位（执行器与鉴权都开着） | `GET /api/v1/executors` 多一行且 `source=="config"`、`GET /api/v1/job-types` 出现 `exec.<新名>`；不重启提交它跑成 `success`；`applied_keys` 含**这一族** `executors.commands.<新名>.<字段>`（原文写"含 `executors.commands`"，落地按叶子键摊平，D-R0704） |
| 12 | 再从 `commands` 里删掉那条 | 该键的处理函数被摘除：`/job-types` 不再列出；提交它 400；既有任务留痕不丢；`applied_keys` 同样给那一族摊平路径（D-R0704） |
| 13 | 执行器**关闭**时改 `executors.commands` | `result==ok` 且有一条 warn 说明"未启用，本次不生效"（R06 §3.1 第三条），`/executors` 不因此多出档位。注：`executors.web_enabled` 必须跟着关，否则 `Validate` 让进程启动即失败 |
| 14 | 改 `executors.commands[].script`（档位内的执行许可字段） | `result==rejected`、`rejected_keys` 含该键、生效表一字未动（`/executors` 与 `/job-types` 与改前逐字节相同） |
| 15 | 改 `server.auth.token` / 加一个 `users` 条目 / 改 `jwt.secret` 三种各一次 | 三次都 `rejected`，旧凭据仍可用（用旧 token 打一次 `GET /api/v1/jobs` 得 200）；`rejected_keys` 给的是**那一次里与生效那份不一致的全部拒绝档键**（累计，不是"刚改的那一条"，D-R0705） |
| 16 | 写坏 YAML（未知键 / 缩进错 / 空文件 / 删掉文件）四种 | 四种都 `rejected`，**当前生效值全部不变**（改前把 level 设成 debug、坏写入后仍是 debug）；修好后下一个窗口自动回 `ok`/`unchanged`。空文件那一支在 R06 落地里其实**没有**被判据覆盖（它是一份合法的"全默认"配置，会被读通并应用），本卡补了守卫并登记 D-R0702；另外补测一条 16E：完全没有凭据的部署（`token` 置空、`users` 与 `jwt.secret` 删掉）遇到空文件，修复前读出 `result=ok` + `workers 7→100`，修复后读出专门文案的 `rejected` 且读数停在 7 |
| 17 | 一次编辑器式原子存盘（写 `config.yaml.tmp` 再 rename 覆盖） | 只发生一次重载（`last_attempt_at` 一个窗口内只推进一次），后续事件仍能触发（`watcher` 没死） |
| 18 | 连续快速改三个键（同一个窗口内） | 合并成一次重载，`applied_keys` 一次给出三个键（防抖真的合并了） |
| 19 | 页面档位写入与文件重载交替（`web_enabled: true` + `reload.enabled: true`） | 交替各 5 次后：`/executors` 的来源标注、`/job-types` 的注册表与登记表三者自洽；无 5xx、`-race` 无关（真进程），日志里没有 panic |
| 20 | 优雅关闭（`SIGINT` / `Ctrl+C`）在重载进行中 | **部分未观测**（登记在 §10.6 乙）：这台机器上 Git Bash 起的 python 没有控制台窗口，`AttachConsole(目标 pid)` 与 `GenerateConsoleCtrlEvent` 都返回真但目标进程不退出，`taskkill` 不带 `/F` 返回 rc=1，所以"关闭日志顺序 watcher 停 → server 停 → scheduler 停"没能在真进程里量到。替代证据是 `go test ./cmd/server -run TestRun_CloseOrder -v` → `--- PASS`（它按调用流水判四步，不依赖信号） |
| 21 | 强杀（`taskkill /F`）后重启 | 崩溃恢复语义不变：执行器任务按 `restore_policy` 处理、被打断的普通任务回 `pending`（对照 `docs/deployment.md:281-288` 的既有现场描述） |
| 22 | `reload.debounce` 热更（200ms→800ms） | `applied_keys` 含该键；改后立刻连写两次（间隔 400ms）应合并成一次重载（旧窗口下会是两次）——判据取 `last_attempt_at` 的推进次数 |

场景 9、11、13、19 需要凭据与执行器开启，按 `docs/design/tasks/web-profile/README.md` 的冒烟体例
准备配置（bcrypt 哈希自造时要 `checkpw` 自检；残留的 `server.exe` 会锁住 `data/`，跑前确认没有）。
测不到的项一律标"未观测"，不许写"应该没问题"。

### 3.2 全量验证

```bash
go build ./...
go vet ./...
go test ./... -race -count=1
go test ./core ./executor ./cmd/server -race -count=5 -timeout 30m
go mod verify
go build -tags dashboard ./cmd/server
GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/server
GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/server
GOOS=windows GOARCH=386 go build -o /dev/null ./cmd/server
```

三条交叉构建的实际命令按 R07 落地时本机 shell 的写法给（本仓前两个系列用的是同样的三条，
出处见 `docs/design/tasks/web-profile/README.md` 的"状态"段），逐条抄输出进本卡 §10.3。

`-tags dashboard` 那条除了构建，还要按控制台的实测体例起一次内嵌形态的进程，
确认 `/admin/runtime` 的 JSON 里 `reload` 对象在浏览器侧的读取路径不受影响
（前端不消费这个键，所以判据只是"页面照常渲染 + 控制台无新增错误"）。

### 3.3 文档同步（逐处给"改成什么"）

1. **`README.md`**
   - 配置示例段（`:436` 起、`:471` 附近）加 `reload:` 一节，注释与 `config.example.yaml` 同一条口径。
   - `:300` 那句"改完要重启"改成条件式："`reload.enabled: true` 时改 `executors.commands` 里
     条目自身的可改字段（timeout、参数声明等）下一个防抖窗口就生效；
     改动条目内的执行许可字段会让整次重载被拒绝，那种改动连同 `executors.workspace`/
     `runtime_allow`/`env_allow` 仍须重启"。
   - `:331` 的"账号增删需重启进程"补一句"热重载不会代劳这件事：凭据项变了整次重载会被拒绝"。
   - 设计文档索引（`:538` 附近那条列表）加一行指向 `docs/design/config-reload-design.md`，
     状态写"已实现（R01–R07）"。
2. **`docs/deployment.md`**
   - `:136` 的"增删账号需重启进程"后面补一句拒绝档的说明。
   - 新增一节"配置热重载"：默认关闭、打开它意味着什么（**能在本机改这个文件的人从此不必重启
     就能改变并发数、留痕策略与档位内容**，设计文档 §9）、哪些键改了立刻生效 / 哪些要重启 /
     哪些会被拒绝（一张三列小表，内容取自设计文档 §6）、
     怎么看上次重载的结论（`GET /api/v1/admin/runtime` 的 `reload`）、
     以及一条运维提示：**改配置前先在版本控制里留一份，坏文件会让进程一直停在旧配置上**
     （场景 16 的事实）。
   - `:503-504` 那段"长期不重启看不到缺页"的既有说法与热重载无关，不改，
     但新节里提到 `uptime` 时不要重复那句话。
3. **`docs/api.md`**
   - `GET /api/v1/admin/runtime` 的响应示例与字段表加 `reload` 对象（七个字段 + 五个 `result` 取值，
     并写明"未启用热重载时这个键整个缺省"）。
   - `:515` 与 `:1618` 两处"改 `executors.commands` 要重启"改成与 README 同一句条件式表述。
   - 明确一句：本系列**没有新增端点**，`/admin/runtime` 仍是 ops 档、读端点仍不进台账（R06 §5.5）。
4. **`docs/example.md`**：如果那份里有配置示例或"重启"相关表述，同步成同一句；
   没有就不改，并在 §10.6 写"核对过、无需改动的具体位置"。
5. **`web/src/content/job-template.md`**：`:272` 那句是使用者读到的话，改成与 README 同一句条件式表述
   （档位在线管理系列 W08/W09 改过一次，本卡再改一次是同一处文案的第二次事实更新，
   记录时把新旧两句都抄进 §10.2，方便后续 grep）。
6. **`configs/config.example.yaml` 与 `configs/config.yaml`**：只改注释——
   在 `reload:` 一节里点出"三档"的去向与那一节自身属重启档，并逐条把
   `executors.web_enabled`（`:119` 附近）与 `server.auth.users`（`:33`）的注释
   加上"热重载不会代劳这件事"的半句。**两份都要改**，键集合由 `TestExampleConfigMatchesLocal` 守。
7. **`docs/design/config-reload-design.md`**
   - 状态行改成"已实施（TASK-R01…R07 落地）"，并在 §2 的决策表后面加一段"落地位置"
     （照 `docs/design/web-profile-design.md:43-78` 的体例：每条 R1–R10 一个 bullet，指向实际代码）。
   - 加一节"§ 与实现的偏离"，逐条列：待拍板 P1–P5 的实际落地答案（P2 已在本系列期间改过口径，
     见设计文档 §13 与 README P5）、`Registry.ApplyConfig` 的最终签名、
     `Diff` 的扁平化实现位置、场景 22 暴露出来的防抖语义差别（如果有）。
   - 设计文档 §3 那张"现状盘点"表里的行号是写卡时的位置，落地后整体下移——
     本卡负责逐条 grep 复核并把现行位置写进 §10.1（W 系列的同样问题记在其 W09 卡里）。

### 3.4 待做项登记（本期只登记，不在任何卡里做）

设计文档 §10 的 N1–N7 七条，本卡逐条核对成"确实没做 + 实测证据"，写进 §10.5 的表：

| 编号 | 核对方式 |
| --- | --- |
| N1 执行器池的运行期扩缩 | 场景 6 之外再做一次：改 `executors.concurrency` → 断言进 `ignored_keys` 且执行器池的读数不变（读数取自 `/admin/runtime` 的 `scheduler.exec_*`，原文写的 `/pools` 不存在，D-R0701） |
| N2 顶层 timeout 与 required_role | 改 `executors.default_timeout` 与 `executors.required_role` → 都进 `ignored_keys`，且既有档位的生效超时不变 |
| N3 三个 interval/capacity | 改 `store.flush_interval`、`observability.flush_interval`、`observability.queue_capacity` → 三条都在 `ignored_keys` |
| N4 凭据热更 | 场景 15 已覆盖 |
| N5 配置端点 | `GET /api/v1/config` 与 `POST .../config/reload` 都回 404；`web/` 里 grep 不到 reload 相关调用 |
| N6 变更台账 | 台账页/`GET /admin/audit` 里搜不到 `config.reload` 这类动作行（本系列没登记任何动作词） |
| N7 与目录加载器共用防抖 knob | 改 `reload.debounce` 后，任务目录加载器仍按 100ms（`core/load.go:94` 是常量）工作：往任务目录投两个文件验证 |

## 4. 实现步骤

1. 先跑 §3.2 的九条命令，把输出原样抄进 §10.3（**先建立基线**，改文档不影响它们，但场景实测会）。
2. 逐条跑 §3.1 的 22 个场景，每个场景记下：改前的 `reload` 状态、执行的写入动作（用
   `Write` 工具或 shell 写入的完整文件片段）、观察到的字段与日志行。
   判据不成立的当场登记为缺陷（§10.6），不要改判据去迁就实现。
3. 跑 §3.4 的 N1–N7 核对。
4. 按 §3.3 逐处改文档，每处改完 `grep` 复核旧说法没有残留（把 grep 命令与命中清单抄进 §10.6）。
5. 复核设计文档 §12 风险表四条的实测归属：事件形状（场景 17/18）、回滚链（R06 §5.1 #2 已有单测，
   本卡补一条真进程的回滚现场——用场景 14 的拒绝路径代替，因为真失败注入不好做，
   登记为"部分实测"）、与页面写入撞锁（场景 19）、读数与真值短期不一致（场景 6 缩容那段）。
6. 更新本目录 `README.md` 的状态表：七行都写"已完成（日期 + 偏离条数 + 关键证据一句话）"，
   并加一节"阶段进度与未验证项"，把受平台限制没跑的项目逐条列清
   （Linux/macOS 只有交叉构建，fsnotify 的真实事件形状只在 Windows 实测等）。
7. 全量重跑 §3.2，确认改文档没有动过代码（`git diff --stat` 只有文档、两份 YAML 注释与本目录卡片）。

## 5. 测试要求

本卡以实测为主。允许为补缺口新增测试，但只能加在本卡依赖的包里，
且新增用例要与原卡的测试要求对得上（例如场景 18 的合并行为若单测缺失，
应补在 `core/watch_test.go` 而不是新造一个文件）。

- `go test ./... -race -count=1` 必须全绿。
- 本卡若零生产代码改动，则 `git diff` 里 `*.go` 只应出现 `_test.go` 的可选新增。

## 6. 完成标准（DoD）

- [x] §3.1 的 22 条场景逐条有实录，判据不成立的登记为缺陷并写清处置（修 or 不修 + 归属卡）。
      实测是 22 条 + 四条补测 16E/16F/16G/16H，实录在 §10.4；§10.6 甲共十七行 D-R0701…D-R0717，每行都写了处置。
- [x] §3.2 那组命令（九条）全部跑过，输出抄在 §10.3；`TestExampleConfigMatchesLocal` 是 `--- PASS`
      而不是 SKIP（若本机缺 `config.yaml` 要写明）。§10.3 抄的是第五轮 01:48:26–01:55:01（判据重写之后的字节），
      每轮各占一个留档文件 `r07smoke/v_final_tests*.txt`；为什么只认第五轮见 D-R0713 与 D-R0716。
- [x] §3.3 七处文档全部改到位，旧说法 grep 无残留（命中清单与逐条处置在 §10.6）；
      `docs/api.md` 的 `reload` 字段表与设计文档 §5.4 的字段名逐个对得上。两份 YAML 与
      `web/src/content/job-template.md` 同步；七处之外另补 executor / web-console 两份设计文档的指向，
      清单在 §10.2 甲；两条 grep 的命中与逐条处置在 §10.6 丁（都不打 `no-stale-claim*`，原因同节）。
- [x] 设计文档状态行改为"已实施"，§2 后面有"落地位置"段，并有一节逐条列出实现与设计的偏离
      （含待拍板 P1–P5 的实际答案）。落地位置块是 R1–R10，偏离那节是新增的 §14
      （14.1 逐条答 P1–P5、14.2 七条形状偏移、14.3 七条读数口径、14.4 携带项处置、14.5 缺陷登记）。
- [x] §3.4 的 N1–N7 七条都有"未实施 + 实测证据"，不是只抄设计文档那段话。§10.5 七行都给了实测或代码事实，
      N1/N2/N3/N5/N6/N7 有真进程读数，N4 落在既有用例的形状上。
- [x] 本目录 README 的状态表七行齐全，"阶段进度与未验证项"里把平台受限项列清。
      另加一张 V1–V7 的"要闭合各需要什么"表。
- [x] `go test ./core ./executor ./cmd/server -race -count=5 -timeout 30m` 无 flake
      （本系列的用例大量等时间窗口，这条是防 flake 的主关口）。第五轮 core 109.387s /
      executor 132.911s / cmd/server 38.045s。
- [x] 若本卡动过任何 `*.go`（补缺口），改动逐条对应到某张前序卡的测试要求，并在 §10.2 写明。
      动了四处：`cmd/server/reload.go` 的"没有取值"守卫对应 R06 卡 §3.1 第 1 步"绝不退回默认值"、
      `cmd/server/reload_test.go` 是它的判据（七条用例、54 条可计数）、`go.mod` 因前者把 yaml.v3 提成直接依赖、
      `api/reload_state.go` 只改注释（D-R0605/D-R0705 口径）。逐条对应关系在 §10.2 乙。
- [x] 缺陷编号一律带系列名"配置热重载系列"（档位在线管理系列的 D-0902 就是重号问题，别再犯）。
      §10.6 甲的十七行每行都带"配置热重载系列"前缀；设计文档 §14.5 用的是裸编号，
      但编号本身已含系列号（`D-R07xx`），两处对同一编号的处置口径一致（这一点是 D-R0717 订正的）。

## 7. 验收方式

```bash
# 1) 确认零生产代码改动
git diff --stat
# 2) 全量验证
go build ./... && go vet ./...
go test ./... -race -count=1
go test ./core ./executor ./cmd/server -race -count=5 -timeout 30m
go mod verify
go build -tags dashboard ./cmd/server
# 3) 旧说法是否还有残留
grep -rn "改 \`executors.commands\` 需要重启" README.md docs web/src/content || echo "no-stale-claim"
grep -rn "页面上不能新增或编辑档位" README.md docs web/src/content || echo "no-stale-claim-2"
# 4) 守卫用例
go test ./core -run TestExampleConfigMatchesLocal -v
```

预期：第 1 条只有文档、两份 YAML 注释与本目录卡片；第 3 条两次都打 `no-stale-claim*`
（若仍有命中，逐条写进 §10.6 说明为什么保留）；第 4 条 `--- PASS`。

执行之后这一段的三条前提都要改：第 1 条的"只有文档"被 **D-R0702 证伪**——那条缺陷要求动生产代码，
本卡最终动了三个 `*.go` 加一个 `go.mod`（`cmd/server/reload.go` 的守卫、`cmd/server/reload_test.go` 的判据、
`api/reload_state.go` 的注释、`go.mod` 因守卫改用 YAML 解析器把 `gopkg.in/yaml.v3` 提成直接依赖），
对应的前一张卡的测试要求逐条写在 §10.2 乙；第 2 条那组命令在终稿里多了一条 `go mod verify`；
第 3 条两次**都不打** `no-stale-claim*`（第一轮 2 条命中、第二轮 6 条，全是 W 系列的历史引用与本卡
自己的两条 grep 命令行，逐条处置在 §10.6 丁）。第 2 条与第 4 条按原文成立。

## 8. 不在本任务范围

- 不做设计文档 §10 的 N1–N7 中任何一条（只核对与登记）。
- 不改前端（`web/` 里只有那份 markdown 文档 `web/src/content/job-template.md` 会动，
  `.vue` 与 `web/src/api/` 零改动）。
- 不新增端点、不动鉴权与角色档位。
- 不把 `reload.debounce` 与目录加载器的常量合并。
- 不做 Linux/macOS 真机实测（只交叉构建），并如实列进"未验证项"。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 场景判据太弱 | "改了 level 后看到 debug 行"可能来自另一条日志源 | 每条都断言 `reload.result` + `applied_keys`，日志现象只是佐证（README 的共同口径） |
| 冒烟污染仓库 | 本系列改的就是配置文件，误写 `configs/config.yaml` 会带走本机凭据 | 一律在 `%TEMP%` 下独立目录跑；跑前确认 `-config` 指向临时文件；结束后 `git status` 核对 |
| 临时目录被回收 | 本机清理器会收走 `%TEMP%` 里的冒烟目录（W08 踩过） | 现场重建后**全部步骤重跑**，不拼接 |
| 残留进程锁住 `data/` | `server.exe` 没退干净会让下一个场景启动失败 | 每个场景结束确认进程已退；`tasklist` 核对 |
| 文档旧说法只改了一半 | 六处表述分散在四份文档 | §7 第 3 条的两次 grep 是硬关口，命中清单逐条处置 |
| `-count=5` 缺 `-timeout` | api 包必然败在 600s 默认超时（W 系列 D-0901） | §3.2 的命令里已带 `-timeout 30m`，且只挑三个包重复跑，全量用 `-count=1` |

回滚：本卡是文档与本目录卡片的 §10 补齐，`git revert` 即可；
若为补缺口动了测试，回滚时保留那些用例（它们守住的是已交付行为）。

## 10. 实现记录（执行时补写）

### 10.1 设计文档 §3 行号的复核结果

复核方式：按**符号名**定位（行号只作辅助），每条都用 `grep -n "^func …"` 一类精确匹配确认。
下表左列是设计文档 §3 那张"现状盘点"表写卡时给的位置，右列是 2026-10-04 的现行位置。

| # | 卡面写的 | 现行位置 | 这条事实现在怎么说 |
| --- | --- | --- | --- |
| 1 | `cmd/server/main.go:313-338`、`:176-311` | `main.go:389-416 main`（`core.LoadConfig` 在 `:394`）、`main.go:225-387 defaultRuntimeDeps`（结构体 `:151-218`） | 仍然"按值传给构造闭包、无全局单例"。补一句：**运行期还会再读一次**——重载链在 `cmd/server/reload.go:329 reloader.Reload` 里调 `core.LoadConfig`，读到的那份只交给 `Diff` 与 setter，不回流到 `runtimeDeps` |
| 2 | `core/config.go:717-816`、`:819`、`:907` | `config.go:741-845 LoadConfig`（`UnmarshalExact` 的拒绝在 `:835-838`）、`config.go:848-935 Validate`、`config.go:948-1068 Normalized` | 一字未变，只是整体下移 |
| 3 | `core/config.go:734-794` | `config.go:758-823`（绑定循环 `:823-827`），`reload.enabled`/`reload.debounce` 在 `:821-822` | 键名表形状未变（本系列往里加了两项） |
| 4 | `core/config_test.go:434`、`:437-444` | `config_test.go:423-437 TestExampleConfigMatchesLocal`、`:440-450 configKeys` | 未变 |
| 5 | `core/scheduler.go:153-254` | `:182-198 SetConcurrency`、`:202-214 SetQueueCapacity`、`:224-236 SetExecConcurrency`、`:242-254 SetExecQueueCapacity`、`:273-285 SetEventPreviewLimit` | "running 时 warn 并忽略"对**这五个**仍成立；本系列新加的 `:292-297 SetRetryPolicy` 与 `:1232-1279 ResizeWorkers` 是运行期生效的例外，别把这句话说成全体 setter |
| 6 | `core/scheduler.go:1128-1196`、`:1206-1228` | `:1282-1335 Start`、`:1395-1416 Stop`（`wg.Wait` 在 `:1415`） | "通道在 Start 建好、Stop 靠 wg 等齐"未变；worker 数已可运行期改（`ResizeWorkers`），执行器池仍按启动值（`:1342-1366 worker`） |
| 7 | `core/scheduler.go:1327-1332`、`:1276-1282` | `:1508-1521 dispatch`（`stopCh` 分支 `:1515-1520`）、`:1529-1543 dispatchExec`、常量 `:1545-1548 execBlockedRetry`（用在 `:1469-1471 scheduleLoop`） | 未变 |
| 8 | `core/scheduler.go:1686` → `core/retry.go:41-48` | `:1875`（在 `:1860-1900 handleFailure` 里 `(*s.retryPolicy.Load()).NextRetry(job)`）→ `retry.go:44-53 ExponentialBackoffRetry.NextRetry` | "每次执行时才读"未变；`retryPolicy` 现在是 `atomic.Pointer`（`scheduler.go:41-44`），所以 R02 的 `SetRetryPolicy` 换得动它 |
| 9 | `core/store.go:161-189` | `:192-222 trimTerminalLocked`（读值在 `:194-195`），入口 `:160-164 trimAfterWriteLocked` ← `:150-157 Update` | 判定形状未变；两个字段已换成 `atomic.Int64`（`:64-65`），新增 `:134-138 SetHistoryRetention` |
| 10 | `core/store.go:204-222` | `:239-255 flushLoop`（ticker 在 `:242`） | 未变，`store.flush_interval` 仍是重启档 |
| 11 | `store/sqlite/events.go:62-63`、`:227-234`；`audit.go:73-74` | `events.go:65-66`（现为 `atomic.Int64`）/淘汰调用点 `:229` /函数体 `:248-276`；`audit.go:77-78` / `:222` / `:237-269`；新增 `:152-159 SetRetention`（events）与 `:153-159 SetRetention`（audit），读值收口在 `:140-149 eventRetentionValues`、`:141-150 auditRetentionValues` | "字段 + 批量周期淘汰"未变；不再是裸结构体字段，本系列换成原子值并加了对外的 setter |
| 12 | `core/logging.go:66-81` | `:67-73 NewLogger`（薄壳）→ `:81-99 NewLoggerWithLevelVar`（级别进 `*slog.LevelVar`，`:87-89`）；新增 `:105-116 SetLogLevel` | **这条已过时**：级别不再钉进 handler 常量，改级别不用换 handler。设计文档 §2 落地块 R10 记的就是这里 |
| 13 | `executor/registry.go:85`、`:143-153` | `:74-86`（`data atomic.Pointer[snapshot]`，字段在 `:85`）、`:154-211 ApplyStore`；新增 `:233-297 ApplyConfig` | "只覆盖 store 那一批"已不成立：config 那一批现在也能整表替换 |
| 14 | `executor/register.go:52-108` | 同范围，语义未变（键冲突返回错误），启动期仍只在 `main.go:911` 的 `registerHandlers`（`:895-913`，914 行往后是 `installRestoreGuard` 的注释）里跑一次 | 配置来源档位的运行期入口不是 `Register`，是 `Applier.ApplyConfig`（调用点 `cmd/server/reload.go:246`） |
| 15 | `executor/applier.go:106-203`、`:116-117` | `:173-227 Apply`（同一把写锁在 `:174-175`）、`:156-162 Validate`、`:240-276 syncHandlers`、`:91-115 NewApplier`、结构体 `:56-75`；新增 `:125-144 NewConfigApplier`、`:313-387 ApplyConfig`、账目字段 `:74 configCommands`、冻结值出口 `:83-87 executorsNow` | "读文件→校验/探测→整表替换→同步处理函数，全程一把写锁"未变；重载链与页面链共用同一把 `writeMu`（场景 19 实测交替 10 次无 5xx、无 panic） |

一句结论：§3 那 15 条里 12 条只是行号下移，3 条（第 5、12、13 行）的**说法**已被本系列改掉，
本表右列给了现行说法；设计文档 §2 末尾的"落地位置"块与 §14 是同一批事实的另一处记录。

### 10.2 文档改动清单（新旧句对照）与本卡动过的 `*.go`

#### 甲、文档改动（七处，逐处新旧对照）

| 处 | 位置 | 旧句（原文） | 新句（要旨） |
| --- | --- | --- | --- |
| 1 | `README.md:300-301` | "……与档位文件 `executors.profiles_path`（……）两份合并时同名以配置为准。"（前一行是"（改完要重启）"） | 删掉那句"（改完要重启）"，改为条件式：`reload.enabled: true` 时改条目自身的可改字段（超时、参数声明、条目增删）下一个防抖窗口就生效；条目内的执行许可字段（`runtime`/`script`/`program`/`env`/`url_template` 等十五项）会让整次重载被拒绝，那种改动连同 `executors.workspace`/`runtime_allow`/`env_allow` 仍须重启，并指向设计文档 §6 |
| 2 | `README.md:331` | "账号增删需重启进程。" | "账号增删需重启进程：`reload.enabled: true` 也不会代劳这件事——凭据项（`server.auth.token`、`server.auth.users`、`server.auth.jwt.secret`）一旦变化，整次重载会被拒绝，现网照旧按旧凭据跑"，指向部署文档 |
| 3 | `README.md:504 之后` | 配置示例里没有 `reload:` 一节 | 加 `reload: enabled: false / debounce: 500ms` 两行注释与一段指向（默认关闭，关闭时一个监听器都不建） |
| 4 | `README.md:541` | 设计文档索引只到观测层那行 | 加"配置热重载设计文档（状态：已实现 R01–R07）+ 任务卡 TASK-R01…R07"两行 |
| 5 | `docs/deployment.md:136` | "增删账号需重启进程" | 补半句：凭据项属拒绝档，热重载也不会生效 |
| 6 | `docs/deployment.md`（新增一节 `## 配置热重载（可选）`） | 全文没有热重载的运维口径 | 三小节：三档归属表（含三条易读错的地方：整次拒绝而非单键跳过、`rejected_keys` 是累计、档位键按字段摊平）、怎么看上一次重载的结论（十字段 + 五个 `result` + 一次失败两行 error + 快照按次替换）、运维提示（改前留版本控制副本、空文件被拒、删键等于退回默认、`reload.enabled` 自己是重启档、`.json` 拼写的盲区、没有配置文件时环境变量也不生效、缩容读数的短期不一致） |
| 7 | `docs/api.md:515` | "改 `executors.commands` 要重启进程"这类无条件说法 | 改成与 README 同一句条件式表述 |
| 8 | `docs/api.md`（`GET /admin/runtime` 处） | 响应示例与字段表没有 `reload` | 加 `reload` 对象示例 + 十字段口径表 + "本系列没有新增任何端点、读端点不进台账、凭据取值不在读数里" |
| 9 | `docs/example.md:184` | **核对过，无需改动**：该文只说"档位来自配置还是来自页面，加载器都拒绝 `exec.` 任务文件"，没有任何"要重启"的表述 | — |
| 10 | `web/src/content/job-template.md:272` | "在线档位（"档位"页）建完即生效、重启后仍在；改配置的 `executors.commands` 仍需重启进程" | 同一行改成条件式（`reload.enabled: true` 时条目自身可改字段下个防抖窗口生效；条目内执行许可字段连同工作目录与两份白名单仍须重启；默认关闭时一律要重启）。这是这处文案的第二次事实更新（第一次是 W08） |
| 11 | `configs/config.example.yaml` 与 `configs/config.yaml`（**只改注释**） | `reload:` 一节写着"监听器与重载链的接线在 R05/R06 落地，状态表收口（R07）之前打开本节不会有行为变化"；`executors.web_enabled`、`server.auth.users`、`executors.commands` 的注释是不条件式的 | `reload:` 换成三档去向 + 本节自身属重启档；`server.auth.users` 补"热重载不会代劳这件事"；`executors.commands` 改条件式；`executors.web_enabled` 补"本项自己是重启档"。两份同步，键集合由 `TestExampleConfigMatchesLocal` 守（见 §10.3 第 8 条 `--- PASS`） |
| 12 | `docs/design/config-reload-design.md` | 状态行是"设计已定稿，逐卡执行中"；§2 之后没有落地位置；§5.4 的三条约束、§8 的两条判据、§12 的最后一条与实现不符 | 状态行改"已实施（TASK-R01…R07 落地，2026-10-03 收口）"；§2 末尾加"落地位置"块（R1–R10 十个 bullet）；§5.4 补两条口径修正与"不含凭据取值"的收窄；§8 的 `/pools` 行改成 `/admin/runtime` 并补一条空文件判据；§12 的 `env_allow` 说法收窄（细节挪到新增的 §14）；新增 **§14 与实现的偏离**（14.1 P1–P5 答案 / 14.2 落地形状 / 14.3 读数语义 / 14.4 携带项处置 / 14.5 新登记缺陷） |
| 13 | `docs/design/executor-design.md:48、:54-55`（本系列之外的两份设计文档，§8 最后一条验收要求补指向） | "`executors.commands`（本文原样，改完仍要重启）"；"现在等价于 能改配置文件并重启的人 + 拿到一个 ops 档 JWT 的人" | 前者改条件式并指向本文 §6.4；后者补一句"打开 `reload.enabled` 之后前半句里的'并重启'不再成立"，并指向本文 §9 的兜底范围 |
| 14 | `docs/design/web-console-design.md:30-32` | 非目标两条："账号改动需重启"、"运行时调整 worker 数/队列容量（列为二期）" | 第一条补"热重载也不代劳（拒绝档）"并指向本文 §6.3；第二条改成"worker 数**已落地**（`ResizeWorkers`，`reload.enabled` 时改 `scheduler.workers` 即生效），`scheduler.queue_capacity` 仍是重启档"，指向本文 §6.2/§7.1 |

#### 乙、本卡动过的生产文件（四处，逐条对应前序卡的要求）

先划清边界：工作树里还有与本卡无关的本地改动（`api/api_test.go`、`api/shutdown_test.go`、`api/sse.go`、`web/vite.config.ts`、`data/jobs.json`，以及 `api/executor_profile_record_test.go` 与 `api/handlers_executor_profiles_test.go` 两个只有行尾差异的文件），本卡一条都没提交，也不列进下面这张表——它们不是本卡的改动。

| 文件 | 改了什么 | 对应的前序卡测试要求 |
| --- | --- | --- |
| `cmd/server/reload.go` | 新增 `configFileCarriesNoValues(path string) (bool, error)` 与它的 `yamlCarriesNoValue(doc any) bool`：**按 YAML 结构**判"这份文件还表达任何取值吗"——整份解析成 `any`（用的就是 `core.LoadConfig` 那一族解析器，所以 BOM、带 BOM 的 UTF-16LE/BE、文档标记与流式写法都不用自己还原），顶层是 nil、或映射里每个叶子都是 nil / 空映射，就算没表达；标量与序列（哪怕是 `commands: []`）算作者写下的取值；解析失败或文件读不出**都不判**，交回第 1 步那条统一口径。`Reload` 在第 1 步之前问这一句，命中就 `rejected` + 专门文案 + 一条 error 日志 + `hint`，`applied` 一字不动；`Reload` 的头注释从"七步"改成"八步"并列出第 0 步。第二、三版留下的 `configFileCarriesNoKeys` / `configFileText` / `cutDocumentMarker`（逐行扫文本那一套）**已删除** | R06 卡 §3.1 第 1 步"整份读通……**绝不退回默认值**（I1 的另一半）"与设计文档 §8"文件被删或读不到 → 视为一次失败的重载，绝不退回默认值"。R06 落地时这条只覆盖了"读失败"那一支：`f66472c` 先补了文本层的判据（缺陷 D-R0702，两轮复核又给它补了 D-R0709 的 BOM/UTF-16 与 D-R0711 的标记边界），第三轮复核证明**文本层修不到底**——`logging:`、`workers: null`、`logging: {}`、顶层 `null` 与"值写在第二份文档"这几族文本上有内容、语义上是全默认，照样退回默认值（新登记 D-R0715），于是整条判据换成按结构判。依据仍是同一条"绝不退回默认值" |
| `cmd/server/reload_test.go` | 七条用例（逐条清单与子用例数见 §10.3 的用例宇宙）：链上五条 `TestReload_EmptyFileKeepsApplied`（空文件 + 恢复，含日志与 `hint` 逐字）、`TestReload_CommentsOnlyFileIsSameConclusion`（8 格：空白 / 注释 / 文档标记 / 标记后跟注释 / UTF-8 BOM / UTF-16LE / UTF-16BE / 只有 BOM）、`TestReload_ValuelessFileIsSameConclusion`（7 格：只有键名 / 显式 null / 空流映射 / 顶层 null / 层层空键 / 值写在第二份文档 / 带 BOM 的只有键名，每格还先核 `LoadConfig` 真能读通它）、`TestReload_StepOneShapesKeepTheirOwnWording`（Tab 缩进 / `----` 开头 / 不带 BOM 的 UTF-16 / 文件被删，判"两种脸色不串门"）、`TestReload_FlowMappingWithMarkerKeepsApplied`（标记后紧跟流式取值不许误拒）；正向对照一条 `TestReload_LeadingBOMDoesNotLookEmpty`；单元层一条 `TestConfigFileCarriesNoValuesShapes`（28 个子用例：16 格该拒 + 12 格该放过，另加"文件不存在交回错误"那一支把 err 返回值也覆盖上） | 同上。判红点重跑成七条变异 M1–M7（见 §10.3，每条的原始输出留档在 `r07smoke/mut_final/`）；上一轮的 M1/M2/M3b/M5/M6 是文本判据的靶子，随那一套实现一起作废（D-R0716 里登记了它们为什么不可复算） |
| `go.mod` | `gopkg.in/yaml.v3 v3.0.1` 从 indirect 那一组提到直接依赖组（新增 import 的必然结果）；`go.sum` 一字未变，构建与验证全程离线跑过 | 与上一行同源：按结构判需要一个 YAML 解析器。选 `gopkg.in/yaml.v3` 是因为 `core.LoadConfig` 走的 viper 用的就是它，判据与第 1 步读的是同一族解析器才谈得上"同一个答案" |
| `api/reload_state.go` | 只改注释：`AppliedKeys` 那一位从"最近一次真的换上现网的键"改成本卡 §14.3 第 1、3 条的口径（处理过 ≠ 生效；`RejectedKeys` 是累计清单） | R06 登记的 D-R0605/D-R0705 的"文档同步"处置（R06 卡缺陷表明写归 R07）。行为一字未动 |

除此之外没有别的生产代码改动。动到的东西落在三条提交里：`f66472c`（文本层那一版守卫与它的判据）、
`ba0f9d5`（`api/reload_state.go` 的注释）、以及第三轮复核返工的那条 `fix(config-reload)` = `ee5a857`
（判据换成按 YAML 结构判，连带 `go.mod` 与这一族用例重写）。前两条已经入库，其中 `configFileText` /
`cutDocumentMarker` 那一套在第三条里被删除——这也是 §10.3 的变异名单必须在第三条之后的字节上整批重跑的理由。

### 10.3 验证证据（§3.2 那组命令的实际输出，本轮多加一条 `go mod verify`）

跑在**本卡终态字节**上（`cmd/server/reload.go` 813 行 sha256 `765fbac5…`、`cmd/server/reload_test.go` 1573 行
`d890ba56…`、`api/reload_state.go` 114 行 `5574c51b…`、`go.mod` 只动了 `gopkg.in/yaml.v3` 那一行的归组；
终态字节与变异前的备份 `r07smoke/reload.go.bak4` 逐字一致），一台机器、一次连跑，
中途没有并行改过被验证包的源文件（R05 的 D-R0515 口径）。
这台机器上这一组命令跑过五轮：00:11–00:17（第一轮复核之前，**这轮的读数没有留档**——脚本每轮截写同一个
输出文件，见 D-R0716 第一条）、00:40–00:46（第一轮修复后，`v_final_tests2.txt`）、00:59:38–01:05:59
（`v_final_tests.txt` 与 `v_final_tests3.txt` 是同一份，`v_final_tests.txt` 现在装的就是它）、
01:08:53–01:15:17（第二轮修复后，`v_final_tests4.txt`）、01:48:26–01:55:01（第三轮判据重写后）。
**下面抄的是第五轮**，前四轮只作历史、不当终态证据（第三轮那一份为什么不能当，见 D-R0713）。
工具链：Go 1.26.4 windows/amd64，Git Bash。

```
### go build ./...
exit=0 (wall from 01:48:26 to 01:48:28)

### go vet ./...
exit=0 (wall from 01:48:28 to 01:48:29)

### gofmt -l cmd/server/reload.go cmd/server/reload_test.go api/reload_state.go go.mod
go.mod:1:1: expected 'package', found module
exit=2 (wall from 01:48:29 to 01:48:29)
# 这一条 exit=2 是我把 go.mod 塞进 gofmt 参数里造成的，不是代码问题：gofmt 只管 .go。
# 三个 .go 文件在这一行里没有任何输出，也就是都已格式化。
# 口径提醒（本轮复核订正过一次）：这三个文件在工作树里是 **LF**，`gofmt -l` 直接跑原文件就算数；
# 本仓工作树里 CRLF 文件确实会让 gofmt 全量误报（项目记忆"验证口径"那条），但改动过的这三个不是那种。

### go mod verify
all modules verified
exit=0 (wall from 01:48:29 to 01:48:32)          ← 本轮起加的一条：判据改用 yaml.v3，把它是否离线可构建记下来

### go test ./... -race -count=1 -timeout 30m
ok      godelayq/api                240.160s
?       godelayq/cmd/gensecret      [no test files]
?       godelayq/cmd/hashpassword   [no test files]
ok      godelayq/cmd/server         8.203s
ok      godelayq/core               21.545s
?       godelayq/examples/demo1     [no test files]
?       godelayq/examples/demo2     [no test files]
ok      godelayq/executor           24.765s
ok      godelayq/store/sqlite       3.724s
?       godelayq/web                [no test files]
exit=0 (wall from 01:48:32 to 01:52:36)

### go test ./core ./executor ./cmd/server -race -count=5 -timeout 30m
ok      godelayq/core               109.387s
ok      godelayq/executor           132.911s
ok      godelayq/cmd/server         38.045s
exit=0 (wall from 01:52:36 to 01:54:51)          ← 三条都过，没有 flake（DoD 的主关口）

### go build -tags dashboard -o r07smoke/server-dash.exe ./cmd/server
exit=0 (wall from 01:54:52 to 01:54:54)

### GOOS=linux GOARCH=amd64 go build -o r07smoke/x-linux-amd64 ./cmd/server
exit=0 (wall from 01:54:54 to 01:54:56)

### GOOS=darwin GOARCH=arm64 go build -o r07smoke/x-darwin-arm64 ./cmd/server
exit=0 (wall from 01:54:56 to 01:54:58)

### GOOS=windows GOARCH=386 go build -o r07smoke/x-win-386.exe ./cmd/server
exit=0 (wall from 01:54:58 to 01:55:01)

### go test ./core -run TestExampleConfigMatchesLocal -v
=== RUN   TestExampleConfigMatchesLocal
--- PASS: TestExampleConfigMatchesLocal (0.00s)
PASS
ok      godelayq/core   (cached)
exit=0                                            ← 是 PASS 不是 SKIP（本机 configs/config.yaml 在场）

补一条本卡新增用例的点名读数（终态字节上单独跑的一次 `-v`：
`go test ./cmd/server -count=1 -v -timeout 5m -run '<七条用例的 or 式>'`，
`ok godelayq/cmd/server 0.359s`，全集见 `r07smoke/v_guard_tests2.txt`；54 条 = 7 个顶层 + 47 个子用例）：
--- PASS: TestReload_EmptyFileKeepsApplied (0.01s)
--- PASS: TestReload_CommentsOnlyFileIsSameConclusion (0.02s)
    --- PASS: .../只有注释、只有空白、只剩文档分隔符、分隔符后跟注释、UTF-8_BOM_加注释、
                UTF-16LE_加注释、UTF-16BE_加注释、只有_BOM  各 0.00–0.01s
--- PASS: TestReload_ValuelessFileIsSameConclusion (0.02s)
    --- PASS: .../只有键名、显式_null_叶子、空流映射、顶层_null、层层都是空键、
                值写在第二份文档、带_BOM_的只有键名  各 0.00–0.01s
--- PASS: TestReload_StepOneShapesKeepTheirOwnWording (0.01s)
    --- PASS: .../Tab_缩进的空白、四个横杠开头不是标记、不带_BOM_的_UTF-16_注释、文件被删
--- PASS: TestReload_LeadingBOMDoesNotLookEmpty (0.00s)
--- PASS: TestReload_FlowMappingWithMarkerKeepsApplied (0.00s)
--- PASS: TestConfigFileCarriesNoValuesShapes (0.08s)
    --- PASS: .../16 格"该拒" + 12 格"该放过" + 文件不存在交回错误  共 28 个子用例，全部 0.00s
```

内嵌形态（`-tags dashboard`）的一次真进程实测，判据按卡 §3.2 末段那句"页面照常渲染 + 控制台无新增错误"。
这一轮的**浏览器观测**用的是 00:46:27 那次构建的二进制，它不是终态字节（既没有第二轮的
`cutDocumentMarker`，也没有第三轮的按结构判）；第五轮在 01:54:52 用终态字节重建过同一个内嵌二进制，
但没有再做一次浏览器观测。为什么不补：这一节判的是 `api` 那份读数的形状与页面渲染，
`api/reload_state.go` 到终态只改过注释，而 `reload.go` 两版之间的差异只在"哪些文件被判成没取值"——
那一族在终态字节下的真进程证据是 16F / 16G / 16H（01:47 那一批，终态 `server.exe`），
下面那次 `logging.level` 改动是一份有取值的正常配置，走不到差异那一支。

```
现场：r07smoke\sdash\，端口 18400，server-dash.exe（含 web/dist 内嵌），reload.enabled=true / debounce=300ms
浏览器（in-app）实测：
  /login 渲染 → ops01 登录后落到 /admin（redirect 直接接上；网络里能看到 POST /api/v1/auth/ws-ticket 200）
  /admin（运维页）渲染完整：main 的标题是"运维"、正文 523 字，并发 worker=4、执行器池四块都在
  该页自己发的 GET /api/v1/admin/runtime = 200（网络面板）
  控制台消息：无（一条都没有）
  在页面上下文里带 Bearer 再读一次 /api/v1/admin/runtime，顶层键 =
      [uptime, started_at, scheduler, event_history, reload]
    reload（一次重载都没跑过时）=
      {"enabled": true, "result": "", "watched_path": "…\\sdash\\config.yaml"}
      ← 没有 last_applied_at / last_attempt_at，正是 R06 要求的那个形状（D-R0101/D-R0502）
  改文件（config.yaml 的 logging.level: info→debug）之后同一个读口：
    reload = {"applied_keys": ["logging.level"], "enabled": true,
              "last_applied_at": "2026-10-04T00:47:28.5681036+08:00",
              "last_attempt_at": "2026-10-04T00:47:28.5663878+08:00",
              "result": "ok", "watched_path": "…\\sdash\\config.yaml"}
    scheduler.workers 读数 4（没改它，跟着变才奇怪）
  两点如实记下：这一份部署里 executors 关着，所以 level=debug 之后日志里 DEBUG 行仍是 0
     （全仓的 debug 站点都要执行器/观测层参与，见 D-R0602）；级别换过去了的证据是上面那个读数。
前端不消费 reload 这个键，所以页面上一格新增的展示都没有——这是预期，不是漏做。
测完 taskkill /F /PID 8332 收掉，tasklist 里不再有 server.exe / server-dash.exe 残留。
⚠️ 生产构建会剥掉 Vue 的开发期警告，"控制台无消息"不能反证模板无问题（控制台系列的既有口径）。
```

D-R0702 / D-R0709 / D-R0711 / D-R0715 这几处判据代码的判红点。这一批是在**第三轮复核重写之后的判据**上跑的
（上一版按文本扫的那套判红点随之作废，理由见 §10.6 的 D-R0715/D-R0716）：跑在 §10.3 那轮全量验证**之后**，
每条变异之后按字节还原，七条的原始输出整批留档在 `r07smoke/mut_final/M<n>.txt`（上一轮只留了叙述、
没留原始输出，被复核判为不可复算，这次每条都带着当时的命令行）。

```
备份：cp cmd/server/reload.go r07smoke/reload.go.bak4
      终态 cmd/server/reload.go 813 行、sha256 765fbac5…；cmd/server/reload_test.go 1573 行、d890ba56…

用例宇宙（-run 里点名的七条 Test 函数；计数口径：go test -v 输出里的 `--- FAIL` / `--- PASS` 行数，
 顶层与子用例同算一条；复算方式 `grep -cE "^ *--- (FAIL|PASS)" r07smoke/mut_final/M*.txt`）：
   TestReload_EmptyFileKeepsApplied                1
   TestReload_CommentsOnlyFileIsSameConclusion     1 + 8   （空白 / 注释 / 文档标记 / 标记后跟注释 /
                                                              UTF-8 BOM / UTF-16LE / UTF-16BE / 只有 BOM）
   TestReload_ValuelessFileIsSameConclusion        1 + 7   （只有键名 / 显式 null 叶子 / 空流映射 /
                                                              顶层 null / 层层都是空键 / 值写在第二份文档 /
                                                              带 BOM 的只有键名）
   TestReload_StepOneShapesKeepTheirOwnWording     1 + 4   （Tab 缩进 / `----` 开头 / 不带 BOM 的 UTF-16 /
                                                              文件被删；这一条判"两种脸色不串门"）
   TestReload_LeadingBOMDoesNotLookEmpty           1
   TestReload_FlowMappingWithMarkerKeepsApplied    1
   TestConfigFileCarriesNoValuesShapes             1 + 28  （判据本身：16 格该拒 + 12 格该放过，再加一格"文件不存在交回错误"）
 合计 54 条（7 个顶层 + 47 个子用例）。

M1 判据永不成立（解析出来的文档一律算有取值）
   红 35 条 / 绿 19 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestReload_EmptyFileKeepsApplied、TestReload_CommentsOnlyFileIsSameConclusion、TestReload_ValuelessFileIsSameConclusion、TestConfigFileCarriesNoValuesShapes
   红的子用例 31 格：只有注释、只有空白、只剩文档分隔符、分隔符后跟注释、UTF-8_BOM_加注释、UTF-16LE_加注释、UTF-16BE_加注释、只有_BOM、只有键名、显式_null_叶子、空流映射、顶层_null、层层都是空键、值写在第二份文档、带_BOM_的只有键名、空文件、只有换行与空格、只有注释、注释里带_#!_之类的前缀、只有文档标记、标记后跟注释、UTF-8_BOM_独存、UTF-8_BOM_加注释、UTF-16LE_加注释、只有键名、显式_null_叶子、空流映射、顶层_null、层层都是空键、非字符串键的空值、值写在第二份文档
   首个红消息：reload_test.go:789: 交回 result="ok" err=<nil>，应当是 rejected
   这条在量什么：D-R0702 那一族整批弹回去：链上与单元层的"该拒"格子全红

M2 解析失败也当成空文件
   红 7 条 / 绿 47 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestReload_StepOneShapesKeepTheirOwnWording、TestConfigFileCarriesNoValuesShapes
   红的子用例 5 格：Tab_缩进的空白、四个横杠开头不是标记、不带_BOM_的_UTF-16_注释、Tab_缩进的空白读不懂不归这里判、不带_BOM_的_UTF-16_读不懂不归这里判
   首个红消息：reload_test.go:996: configFileCarriesNoValues = true, want false（原文 "\n \n\t\n"）
   这条在量什么：判的正是"两种脸色不串门"——读不懂的文件要说成读不懂，不许冒充空文件

M3 顶层是 null、只有空白或注释那一支不算空
   红 33 条 / 绿 21 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestReload_EmptyFileKeepsApplied、TestReload_CommentsOnlyFileIsSameConclusion、TestReload_ValuelessFileIsSameConclusion、TestConfigFileCarriesNoValuesShapes
   红的子用例 29 格：只有注释、只有空白、只剩文档分隔符、分隔符后跟注释、UTF-8_BOM_加注释、UTF-16LE_加注释、UTF-16BE_加注释、只有_BOM、只有键名、显式_null_叶子、顶层_null、层层都是空键、值写在第二份文档、带_BOM_的只有键名、空文件、只有换行与空格、只有注释、注释里带_#!_之类的前缀、只有文档标记、标记后跟注释、UTF-8_BOM_独存、UTF-8_BOM_加注释、UTF-16LE_加注释、只有键名、显式_null_叶子、顶层_null、层层都是空键、非字符串键的空值、值写在第二份文档
   首个红消息：reload_test.go:789: 交回 result="ok" err=<nil>，应当是 rejected
   这条在量什么：yaml 把这些都解析成 nil，去掉 nil 那一支就放过

M4 映射里只要有任何键就算表达了取值
   红 11 条 / 绿 43 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestReload_ValuelessFileIsSameConclusion、TestConfigFileCarriesNoValuesShapes
   红的子用例 9 格：只有键名、显式_null_叶子、空流映射、层层都是空键、带_BOM_的只有键名、只有键名、显式_null_叶子、空流映射、层层都是空键
   首个红消息：reload_test.go:996: configFileCarriesNoValues = false, want true（原文 "logging:\n  level:\n"）
   这条在量什么：D-R0715 的靶子：把实现退回"有键就算有内容"，第三轮复核提的那一族（只有键名 / null / 空流映射）立刻判红

M5 标量与序列也判成空（过头实现）
   红 13 条 / 绿 41 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestReload_EmptyFileKeepsApplied、TestReload_LeadingBOMDoesNotLookEmpty、TestReload_FlowMappingWithMarkerKeepsApplied、TestConfigFileCarriesNoValuesShapes
   红的子用例 9 格：一行真取值、流式映射跟在标记后面、流式映射单独一行、带_BOM_的真取值、显式空序列是作者意图、空字符串也算写下来的取值、顶层是标量、四个横杠不是文档标记、三个点后面紧跟键
   首个红消息：（见原始输出）
   这条在量什么：正向对照的牙在这一条上：真取值、显式空序列、标记后面的流式映射全被误拒

M6 非字符串键的映射不回看叶子（去掉 map[any]any 那一支）
   红 2 条 / 绿 52 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestConfigFileCarriesNoValuesShapes
   红的子用例 1 格：非字符串键的空值
   首个红消息：（见原始输出）
   这条在量什么：这一格是为这条变异补上去的判据（`1:` / `2:` 这种数字键的空值）

M7 守卫调用点短路（判据算了但没人问）
   红 18 条 / 绿 36 条（合计 54，等于用例宇宙 54 条）
   红的顶层用例：TestReload_EmptyFileKeepsApplied、TestReload_CommentsOnlyFileIsSameConclusion、TestReload_ValuelessFileIsSameConclusion
   红的子用例 15 格：只有注释、只有空白、只剩文档分隔符、分隔符后跟注释、UTF-8_BOM_加注释、UTF-16LE_加注释、UTF-16BE_加注释、只有_BOM、只有键名、显式_null_叶子、空流映射、顶层_null、层层都是空键、值写在第二份文档、带_BOM_的只有键名
   首个红消息：reload_test.go:789: 交回 result="ok" err=<nil>，应当是 rejected
   这条在量什么：链上三条用例全红、单元层那 29 条全绿——这就是"两层判据都要有"的证据：只留单元测试会漏掉接线，只留链上用例查不出形状边界

七条变异全部判红，这一批没有等价变异（上一轮的 M6 等价变异是因为它变的正是文本判据里那个
"剥掉标记后再判断"的分支，而那一整套分支已经随着判据换掉而删掉了）。
每条之后 cp 回原字节：终态 `cmd/server/reload.go` 与 `r07smoke/reload.go.bak4` sha256 逐字一致（脚本核过），
还原之后 `go vet ./cmd/server` 干净、`gofmt -l` 无输出、`go test ./cmd/server -count=1` 回到 ok。

判据重写发生在第四轮之后，所以 §3.2 那组命令在终态字节上又整轮跑了一次（第五轮，01:48:26–01:55:01）。
上面正文抄的就是第五轮；第三轮那一份为什么不能当终态证据见 D-R0713，最早一轮的读数为什么没有留档见 D-R0716。

### 10.4 场景实测记录（22 条，含每条的写入片段与观察到的字段）

现场与工具（先说清，后面的读数才站得住）：

- 每个场景一个独立目录、一个真实进程、一个自己挑的空闲端口（IPv4 与 IPv6 都探过），
  配置、`data/`、产物目录、`server.log` 全在场景目录里；仓库的 `configs/config.yaml` 与 `data/` 一个字节都没写。
- 目录原定 `%TEMP%\r07\<场景>`，**实测中途被系统回收**（前一轮的记录全在回收里丢了），
  所以本节的每一条都是在 `D:\codeproject\mygo\r07smoke\s<编号>\` 上**重跑**出来的。
  公共件 `r07lib.py` 与 `s*.py` 留在同一个目录里，写入动作全部由 `Proc.patch`（点分键改值）
  或 `Proc.patch(raw=…)`（整份覆盖，`atomic=True` 时先写 `config.yaml.tmp` 再 `os.replace`）发出。
- **二进制的新旧要说清**（这一条本身登记为 D-R0714）：表里 1–22 与 16E 那一批跑在 00:44 那次
  `go build` 的 `server.exe` 上，那份字节**已含** D-R0702 的守卫与 D-R0709 的 BOM/UTF-16 还原，
  **还没有**第二轮的 `cutDocumentMarker`；第二轮的改动只影响"标记后紧跟流式取值"这一族会不会被误拒，
  那一批里没有一格走那条路径。之后判据又被第三轮复核整体重写（D-R0715：改成按 YAML 结构判），
  所以 01:45 用终态字节（`reload.go` sha256 `765fbac5…`）重建了一次 `server.exe`，
  并把与这条判据相关的三条在**终态字节**上重跑/补跑：16F（01:47:48）、16G（01:47:53）、
  新增的 16H（01:47:35，专测第三轮那一族"有键名没取值"的形状）。
  1–22 与 16E 没有在终态字节上重跑——重写只可能改变"哪些文件被判成没取值"，
  而那一批里除 16/16E/16F/16G/16H 之外没有一格走这条判据；这一点如实写在这里，不写成"全部重测过"。
  另外 01:18/01:19 那一次 16F/16G 是在第二轮修复后的字节上跑的（`12d972b1…`），留档不删、只作历史。
- 凭据是本组自造的假值：机器 token `r07-machine-token-2k9x`、账号 `ops01` / `r07pass-ops`
  （哈希由 `cmd/hashpassword` 产出并用 `bcrypt.checkpw` 自检过 True/False 两侧），
  JWT secret 是 `crypto/rand` 风格的随机串。判据一律取自日志、`/admin/runtime` 与 REST 响应。
- 卡面写的 `GET /api/v1/pools` 本仓不存在（实测 404，D-R0701），凡是要看并发/队列读数的判据
  都改取 `GET /api/v1/admin/runtime` 的 `scheduler` 对象——同一份 `RuntimeStats`。
- 读数里的时间戳来自 2026-10-04 本机时钟；同一次记录里的先后顺序按 `last_attempt_at` 比较，不按人眼。

逐条结论（"达标"= 卡面判据成立；每条下面的现场是原样抄的读数行）：

| # | 场景 | 结论 |
| --- | --- | --- |
| 1 | 不写 `reload` 这一节 | 达标（监听器一条都不建、`reload` 键不出现、改文件无反应）。goroutine 数那一半未观测，见 §10.6 |
| 2 | `debounce: 10ms` | 达标：启动即失败，文案含 `reload.debounce`、下界与默认值 |
| 3 | `-config` 指不存在的文件 | 达标：启动即失败，不建监听器 |
| 4A | 没有配置文件、只给环境变量 | 达标（并暴露 D-R0706：环境变量在这条路上整组不生效） |
| 4B | 目录里只有 `config.json`、不给 `-config` | 达标：走到 R06 §5.2 #13 那条 warn，`reload` 读数带 `watcher_error` |
| 5 | `logging.level` info→debug→error | 达标（debug 侧判据按 D-R0602 的可判形状写） |
| 6 | `scheduler.workers` 4→12→2 | 达标：两次读数都跟随，缩容期间 6 条任务全 `success` |
| 7 | `scheduler.queue_capacity` | 达标：`ignored_keys` 含该键，读数 4→4 不变 |
| 8 | `store.history_limit` 1000→5 | 达标：留痕 8→5，`/stats.completed` 同步 |
| 9 | 观测层启用时改两个 retention | 达标：事件 `count` 40、台账 `count/total` 15 |
| 10 | 观测层关闭时改同样两个键 | 达标：两键进 `ignored_keys`、`result=ok`、两条 warn |
| 11 | 加一条档位 | 达标（`applied_keys` 的形状是摊平后的逐字段路径，判据按前缀族改，D-R0704） |
| 12 | 删掉那条档位 | 达标：`/job-types` 不再列出、再提交 400、既有留痕不丢 |
| 13 | 执行器关闭时改档位 | 达标：`result=ok` + 键记入 `applied_keys` + 一条 warn（D-R0605 的形状） |
| 14 | 改档位内的 `script` | 达标：`rejected`、生效表逐字节相同 |
| 15 | 三种凭据改动各一次 | 达标（`rejected_keys` 是累计清单，判据按此改写，D-R0705） |
| 16 | 四种坏写法 | 达标：未知键/缩进错/删文件都 `rejected`，**空文件也 `rejected`**——这一条是本卡修出来的（D-R0702） |
| 16E | 无凭据部署遇空文件 | 达标（修复前后对照见下面第 16 条） |
| 16F | 带 BOM / UTF-16 写的空文件（第一轮复核后补测） | 达标：四种形态全 `rejected`、读数停在 7（D-R0709 的真进程证据；终态字节上 01:47:48 又跑了一遍） |
| 16G | 文档标记后紧跟流式取值的配置文件（第二轮复核后补测） | 达标：三种"有内容"的形状都不走这条判据，读数照改（D-R0711 的真进程证据；终态字节上 01:47:53 重跑） |
| 16H | 有键名但没有取值的文件：`logging:`、`workers: null`、`logging: {}`、顶层 `null`、值写在第二份文档、带 BOM 的 UTF-16 只有键名（第三轮复核后补测） | 达标：六格全 `rejected`、给的都是新文案，读数停在 7；恢复写入 `workers: 9` 之后 `result=ok`（D-R0715 的真进程证据，01:47:35） |
| 17 | 原子存盘（`.tmp` + rename） | 达标：一个窗口一次重载，后续事件仍能触发 |
| 18 | 同窗口内三连改 | 达标：合并成一次，`applied_keys` 一次给三个键 |
| 19 | 页面写与文件重载交替 | 达标：三者自洽、无 5xx、无 panic、无 error 行 |
| 20 | 重载进行中优雅关闭（Ctrl+C） | **部分未观测**：投递 API 调用成功但进程不退出；替代证据是 `TestRun_CloseOrder` |
| 21 | 强杀后重启 | 达标：exec 任务落 `paused` 且带 `restore_after_crash`，普通任务重跑至 `success` |
| 22 | `reload.debounce` 200ms→800ms | 达标：新窗口把间隔 400ms 的两次写入合并成一次，旧窗口给两次 |

下面把每条的写入片段与观察到的字段按场景号抄一遍。

**场景 1**（起点：`BASE_CFG` 去掉 `reload` 整节）
```
端口=18400 health_up=True ；runtime 状态码=200
顶层键=['event_history', 'scheduler', 'started_at', 'uptime']        ← 没有 reload
响应文本里出现 "reload" 子串？False
日志命中：hot reload=0 watcher=0 reload=0
改文件后：建任务状态码=201；INFO 行数 7 -> 9（没有监听器，级别不会跟着变）
磁盘上的那一行=['L30:   level: error']                                ← 写了但不可能生效
```

**场景 2**（写入 `reload.debounce: 10ms`）
```
退出码=1（启动即失败）
LOG: load config failed: reload.debounce 10ms is too small, a merge window below 50ms means
     re-reading the config file several times per save (omit the key to use 500ms)
含 reload.debounce 的行数=1；是否起了监听器=0
```

**场景 3**（`-config` 指向 `s03\nope\config.yaml`，该路径不存在）
```
退出码=1
LOG: load config failed: read config failed: open …\s03\nope\config.yaml:
     The system cannot find the path specified.
监听器建了没（hot reload 命中）=0；服务在听吗 health=-1
```

**场景 4A**（`no_config_flag=True` + `cwd` 里没有 `configs/`，环境变量给 `GODELAYQ_RELOAD_ENABLED=true`、
`GODELAYQ_SERVER_PORT=18400`、`GODELAYQ_LOGGING_LEVEL=warn`）
```
进程在跑？True
实际监听端口（日志行）：msg="http api server listening" addr=[::]:8080    ← 不是 18400
监听器起了吗（hot reload 命中）=0
/admin/runtime 状态码=-1（探的是 18400，进程不在那儿）
生效级别：INFO 行=1（GODELAYQ_LOGGING_LEVEL=warn 若生效应为 0）
生效并发数 workers 读数=None（默认值 100；GODELAYQ_SCHEDULER_WORKERS 没绑）
```
→ 卡面"用环境变量打开热重载"这条路走不通：`LoadConfig` 只在 `ReadInConfig()` 成功那一支做
`UnmarshalExact`，一份文件都没有时整组环境变量都不生效（登记 D-R0706，属既有行为、不归本系列改）。
所以"起服务、不建 watcher、日志里一条 warn"这条判据只能在**有文件**的形状上判，见 4B。

**场景 4B**（`cwd/configs/config.json` 存在且 `reload.enabled: true`，不给 `-config`）
```
进程在跑？True；端口=18400
WARN: msg="config hot reload is enabled but no config file was loaded at startup"
      hint="用 -config 指一个真实存在的文件，或放置 configs/config.yaml 后重启；
            在此之前进程按当前生效的取值一直跑下去，改文件不会有任何反应"
/admin/runtime 状态码=200
reload 对象={"enabled": true, "result": "",
             "watcher_error": "reload.enabled=true，但进程启动时没有读到任何配置文件，监听器没有建立"}
读到的配置是否生效：workers=100（代码默认 100）        ← .json 那份没被读
改 .json 之后 last_attempt_at None -> None；改后 INFO 行数=3
```

**场景 5**（`logging.level` info→debug→error，两次单独写入，间隔足够跨过防抖窗口）
```
起点 reload={"enabled": true, "watched_path": "…\s05\config.yaml", "result": ""}
[level=debug] result=ok applied=['logging.level'] attempt=23:55:33.4254 applied_at=23:55:33.4261
  提交 exec.sleep2 三条（201/201/201）→ DEBUG 行（waiting for an executor slot）命中=4
  样例：level=DEBUG msg="due job is waiting for an executor slot" heap_size=1 exec_queue_capacity=1
[level=error] result=ok applied=['logging.level'] attempt=23:55:36.7770
  error 期间：INFO 行 17 -> 17，DEBUG 行 5 -> 5（都不增长）；建任务 201 后新增 INFO 行=0
```

**场景 6**（`scheduler.workers` 4→12→2）
```
起点 sched：workers=4 queue_capacity=4
[workers=12] result=ok applied=['scheduler.workers'] → 读数 workers=12 queue_capacity=4
提交 6 条 payment_check（每条在跑 2s）→ 缩容前 running=6 heap=0 queue_length=0
[workers=2]  result=ok applied=['scheduler.workers'] → 读数 workers=2 queue_capacity=4
6 条任务终态分布={'success': 6}；缩容后读数复核：workers=2
```

**场景 7**（`scheduler.queue_capacity` 4→8）
```
result=ok ignored=['scheduler.queue_capacity'] applied=None error=
队列容量读数 4 -> 4（改了不该变）
```

**场景 8**（`store.history_limit` 1000→5；改前造 8 条终态任务）
```
改前：success 留痕=8，/stats={"completed": 8, …}
result=ok applied=['store.history_limit']
改后：success 留痕=5（受新上限约束），/stats.completed=5；pending=0（待执行任务不受影响）
```

**场景 9**（观测层开着，`observability.{events,audit}.retention_count` 3000→40/15；改前造 120 条任务）
```
改前：/events count=360（total 一位没有）；/admin/audit count=120 total=120
result=ok applied=['observability.audit.retention_count', 'observability.events.retention_count']
再造一条任务推过一个批量周期 → /events count=40；/admin/audit count=15 total=15
事件行数受新上界约束=True；台账行数受新上界约束=True
注：/api/v1/events 的形状是 {count, items, note}，没有 total，所以事件那一侧按 count 判
```
（本轮第一版把这条判据写成 `total <= 40`，读出来是 False——那是**测量脚本的 bug**，
不是产品行为；改正判据后重跑，上面两行是真读数。）

**场景 10**（`observability.enabled=false`，改同样两个键）
```
起点：/admin/audit 状态码=503；观测库文件存在？False
result=ok ignored=['observability.audit.retention_count', 'observability.events.retention_count']
        applied=None（没生效，也不算失败）
与"本节未启用"相关的 warn 行=2
WARN: msg="config reload skipped a disabled section" step="observability.events 保留策略"
      keys=observability.events.retention_count hint=那一节的总开关没打开…
```

**场景 11**（`executors.commands` 追加一条 `quick`（http 形态））
```
改前 /job-types={"types":["data_sync","email_send","exec.sleep2","payment_check","report_generate"]}
result=ok applied=['executors.commands.quick.allowed_hosts', …, 'executors.commands.quick.url_template']
        （26 条逐字段路径，裸键 executors.commands 不在里面 —— D-R0704）
/executors 行数=2 名称=['quick','sleep2']；quick 那行 source=config runtime_ok=True degraded=False
/job-types 含 exec.quick=True；不重启提交 exec.quick：201 → 终态 success；产物读取 200
```

**场景 12**（把那条 `quick` 从 `commands` 里删掉）
```
result=ok applied=[…同一族 quick.* 路径…（整列表改动时容器路径也在，见场景 12 的完整清单）]
删后 /job-types={"types":["data_sync","email_send","exec.sleep2","payment_check","report_generate"]}
删后再提交 exec.quick：400 {"message":"unknown job type","details":"job type 'exec.quick' not registered"}
既有 quick 任务的留痕条数=1（不丢）；/executors 剩余档位=['sleep2']
```

**场景 13**（`executors.enabled=false` + `executors.web_enabled=false`，改 `commands` 加一条）
```
起点 /executors={"enabled": false, "profiles": [], "web_enabled": false, "runtime_allow": ["node"]}
result=ok applied=['executors.commands.quick.*' 26 条] ignored=None rejected=None error=
WARN（命中=1）: msg="executors are disabled: executors.commands was recorded but is not in effect"
               keys=executors.commands.quick.allowed_hosts,…
改后 /executors 仍不含新档位=True（行数=0）；/job-types 里出现 exec.quick？False
last_applied_at 有值（这一条就是 D-R0605 说的那个形状：记为已处理、未生效）
```
（注：`executors.enabled=false` 时 `web_enabled` 必须一起关掉，否则 `Validate` 让进程启动即失败——
`executors.web_enabled requires executors.enabled to be true`。卡面没写这条，第一次跑就撞上了。）

**场景 14**（改档位内的 `script`，即执行许可字段）
```
result=rejected rejected=['executors.commands.sleep2.script']
error=改动触碰了拒绝档（凭据与执行许可字段），整次作废、一项都没有应用：
      executors.commands.sleep2.script；这类改动只能改完文件再重启进程
改前/改后 /executors 摘要=(200,'263a54e94f1afa68')，/job-types 摘要=(200,'5a957e290a142f58')
生效表逐字节相同=True；/executors 第一行的 path_display 仍是 scripts/sleep2.mjs
上一步热更出来的 scheduler.workers 读数仍是 6（拒绝那次一个落点都没动）
```

**场景 15**（三种凭据各改一次；先热更一次 `logging.level=debug` 作对照锚点）
```
锚点：result=ok applied=['logging.level']
① server.auth.token 改新值 → result=rejected rejected_keys=['server.auth.token']
   旧 token 打 GET /api/v1/jobs=200；用新 token 打=401
② server.auth.users 加一条  → rejected_keys=['server.auth.token', 'server.auth.users']
③ server.auth.jwt.secret 改 → rejected_keys=['server.auth.jwt.secret', 'server.auth.token', 'server.auth.users']
每一次都：旧凭据仍可用（200）、workers 读数不变（4）、error 文案给出对应键名列表
收尾读数：applied=None ignored=None last_applied_at=None    ← 见 D-R0707
最后一次失败的 error 行（监听器那一半，带 path）：
  level=ERROR msg="config reload returned error" error=… path=…\s15\config.yaml
```
①里 `rejected_keys` 恰好只有一条，是因为那一轮只有 token 与现网不同；②③把它带进清单，
说明这份清单是"本次与生效那份不一致的全部拒绝档键"（累计），不是"你刚改的那一条"（D-R0705）。
收尾那行同时说明 `last_applied_at` 属于"那一次尝试"，失败的重载会把它带走（D-R0707）。

**场景 16**（现网先设成 `level=debug` + `workers=6`，然后四种坏写法，最后修好）
```
未知键：result=rejected error=重新读取配置文件失败，本次重载作废、现网继续按当前生效的取值运行（…）：
        parse config failed: decoding failed due to the following error(s): 'logging' has invalid keys: level_not_a_key
缩进错：result=rejected error=…：read config failed: While parsing config: yaml: line 2: did not find expected key
空文件：result=rejected error=配置文件里一个键都没有（空文件或只有注释），本次重载作废、
        现网继续按当前生效的取值运行（…）：空文件会被读成一份全部取默认值的配置，
        要把取值退回默认请显式写出每一项并重启进程
        applied=None ignored=None rejected=None
        ← 这一句是**当时那份二进制**（00:44 构建）的文案。判据在第三轮复核重写之后，
          同一个形状交回的是"配置文件没有表达任何取值（只有空白、注释、只有键名或 null）…"，
          终态字节下的实读见下面 16F / 16H
删文件：result=rejected error=…：read config failed: open …: The system cannot find the file specified.
四种写法之后：workers 读数一直是 6（代码默认是 100），级别侧的读数证据见 16E 的同一说明
修好（整份覆盖回原内容）：result=unchanged applied=None error=None；workers=6
```

**场景 16E**（补测：一份**完全没有凭据**的部署——`server.auth.token` 置空、`server.auth.users` 与
`server.auth.jwt.secret` 从文件里删掉——遇到空文件）
```
起点：workers 读数=7，这份部署没有鉴权（/api/v1/stats 带不带凭据都 200，/admin/runtime 匿名可读）
写空文件之后：result=rejected
   error=配置文件里一个键都没有（空文件或只有注释），本次重载作废…（同上那条专门文案，
   终态字节下这句已经换成"没有表达任何取值"那一条，见 16F/16H）
现网 workers 读数=7（代码默认 100 —— 这一条就是"没换成默认值"的证据）
级别这一维在这份部署里没有可读判据点（executors 关闭时全仓 debug 站点都不打，
   而 level=debug 下 INFO 行本来就在长），所以不拿 INFO 行数当"退回 info"的证据
整份重写一份正常配置：result=ok applied=['logging.level','scheduler.workers']
   → workers 读数=9；随后 level=warn 下 INFO 行 16 -> 16 不再增长（监听器活着且这次真换上了）
```
修复前的同一场景（本轮第一次跑，守卫还没加）读出的是：`result=ok`、`workers 7 -> 100`、
级别从 debug 回到 info、档位表被清空。这正是 D-R0702。**这份"修复前"的读数没有原样留档**——
它出自 %TEMP% 目录被回收之前的那一次现场记录，回收后只剩本节这段转述（D-R0716 第三条把它登记在案）。

**场景 16F**（第一轮复核后补测：带 BOM / UTF-16 写出来的"空文件"，同一份无凭据部署）
```
起点：workers 读数=7
--- UTF-8 BOM + 只有注释（49 字节）      result=rejected，workers 读数=7
--- UTF-16LE(带 BOM) + 只有注释（42 字节） result=rejected，workers 读数=7
--- UTF-16BE(带 BOM) + 只有注释（42 字节） result=rejected，workers 读数=7
--- 只有 UTF-8 BOM（3 字节）              result=rejected，workers 读数=7
    四次的 error 都是那条专门文案（终稿这一句是"配置文件没有表达任何取值（只有空白、注释、
    只有键名或 null）…要把取值退回默认请显式写出每一项并重启进程"，
    第一轮复核时抄的是旧句"配置文件里一个键都没有…"——判据重写之后 01:47:48 整条重跑，读数量级不变）
恢复：整份重写 workers=9 → result=ok applied=['scheduler.workers']，读数=9
```
这四格是 D-R0709 的真进程侧证据。修复前它们会走多远，由当时的变异量出：不还原 BOM/UTF-16 时，
同一个形态在单元用例里交回的是 `result="ok"`（用例台跑的正是真的 `core.LoadConfig` + 真文件），
也就是说 viper/yaml 认得带 BOM 的 UTF-16，"只剩注释"被读成一份全默认配置。
判据重写成按结构判之后，这一族不再需要自己还原字节（解析器认这些编码），
终态字节上的复测就是上面这份。

**场景 16G**（第二轮复核后补测：文档标记后面紧跟流式取值，同一份无凭据部署，workers 起点 7）
```
--- {scheduler: {workers: 8}, logging: {level: debug}}          result=ok  applied 含 scheduler.workers，读数 7→8
{scheduler: {workers: 9}, logging: {level: debug}}              result=ok  applied=['scheduler.workers']，读数→9
--- 换行之后接块式两节（logging.level=info、scheduler.workers=11）  result=ok
      applied=['logging.level','scheduler.workers']，读数→11
---- {scheduler: {workers: 10}, …}（四个横杠，不是文档标记）      result=rejected，但 error 是
      "重新读取配置文件失败…read config failed: While parsing config"——走的是第 1 步的读失败口径，
      不是"没有表达任何取值"那条；workers 读数停在 9（这一格证明两个判据互不串门）
```
这四格是 D-R0711 的真进程侧证据：前三种都是**有内容的文件**，第一轮那版裸前缀匹配会把第一种
（`--- ` 后面直接跟流式映射）整行跳掉、从而误判成空文件；现在的判据按结构读，它解析得出 `workers: 8`，
于是读成 `ok`。最后一格反过来证明"误拒合法配置"与"读失败"这两种脸色没有混用。
（现场读数原样抄自 `r07smoke/out_s16g2.txt`，01:47:53，终态字节；第一种形状的 `applied_keys` 很长
 是因为整份覆盖把 `executors.commands` 那一族按条目摊平了，判据只看 `result` 与 workers 读数。）

**场景 16H**（第三轮复核后补测：文件里有键名、结构也在，但一个取值都没有，同一份无凭据部署）
```
起点：workers 读数=7
--- 只有键名（40 字节，logging: / level: / scheduler: / workers:）  result=rejected，读数=7
--- 显式 null 叶子（50 字节，workers: null / level: null）          result=rejected，读数=7
--- 空流映射（12 字节，logging: {}）                               result=rejected，读数=7
--- 顶层 null（5 字节）                                            result=rejected，读数=7
--- 值写在第二份文档（53 字节，--- / 注释 / --- / scheduler.workers=9）result=rejected，读数=7
--- UTF-16LE(带 BOM) 的只有键名（46 字节）                          result=rejected，读数=7
    六次的 error 全是同一条专门文案，"新文案命中=True 旧文案命中=False"逐条打在原始输出里
恢复：整份重写 workers=9 → result=ok applied=['scheduler.workers']，读数=9
```
这一族是 D-R0715 的真进程侧证据：上一版按文本扫的判据对这六格全部答"文件有内容"，
而 `core.LoadConfig` 把它们都读成一份全默认配置（第五格还额外证明 viper 只读第一份文档）。
现在这六格走的是与空文件同一条拒绝判据，现网取值一格都没动。
（原样读数在 `r07smoke/out_s16h.txt`，01:47:35；这一族在修复前的走多远由变异 M4 量出。）

**场景 17**（`config.yaml.tmp` 写入后 `os.replace` 覆盖，窗口 200ms）
```
写入的片段（相对上一份只改一处）=['level: warn']
result=ok applied=['logging.level']
窗口内 last_attempt_at 的新取值=['…00:00:45.1078742+08:00']（推进次数=1，判据 1）
临时文件残留=False
rename 之后再来一次普通写入：result=ok applied=['logging.level']（监听器没死）
```

**场景 18**（一次写入里同时改三个键：`logging.level`、`scheduler.workers`、`scheduler.max_retry_delay`）
```
result=ok applied=['logging.level','scheduler.max_retry_delay','scheduler.workers']
三次写入带来的重载次数=1（判据 1）；applied_keys 一次给出三个键=True
生效读数：workers=5
```

**场景 19**（`web_enabled: true` + `reload.enabled: true`，页面档位写与文件重载交替 10 步）
```
[1 页面建 wprof_a] 201 ；[2 文件加 quick] 文件侧 result=ok（applied 是 quick.* 那一族）
[3 页面改 wprof_a 超时] 200（timeout 45s）
[4 页面建与配置同名的 sleep2] 409 {"message":"profile name belongs to a configuration profile"}
[5 文件回到只有 sleep2] result=ok ；[6 页面删 wprof_a] 200
[8 页面建 wprof_b] 201 ；[9 文件加 quick 并改 sleep2 超时] result=ok
[10 页面删 sleep2] 409（同名那条归配置，页面删不动）
交替之后：/executors 行=[('quick','config'),('sleep2','config'),('wprof_b','store')]
          /job-types 的 exec.* =['exec.quick','exec.sleep2','exec.wprof_b']
          档位文件里的记录=[{"name":"wprof_b",…}]        ← store 侧只有一条，与 /executors 一致
三者自洽=True；状态码里出现 5xx=False；日志里的 panic 行=0、error 行=0
提交 exec.wprof_b：201 → 终态 success
交替次数：页面写 6 次、文件重载 4 次（同一把 writeMu，见 §10.1 第 15 行）
```

**场景 20**（重载进行中投 Ctrl+C）—— 部分未观测
```
以 CREATE_NEW_CONSOLE 启动（pid=28212），reload.debounce=1200ms 把窗口拉长，写入 workers=9
控制台现场：GetConsoleWindow=0  AttachConsole(目标 pid)=1  GenerateConsoleCtrlEvent=1
退出码=None（10s 内没退）；日志位置：shutting down server=[] server exited=[]
期间唯一与重载有关的行：level=INFO msg="config reload applied" applied_keys=scheduler.workers
taskkill /PID（不带 /F）也试过：rc=1，目标不响应（那是 WM_CLOSE，Go 不把它当信号）
替代证据：go test ./cmd/server -run TestRun_CloseOrder -v → --- PASS (0.02s)
```
所以四条关闭顺序（`watcher.Close → chain.Stop → server.Stop → scheduler.Stop`）只有单元测试这一层
的证据（它判的是调用流水，不依赖信号），真进程侧的"关闭落在重载之中"这一格没跑到，如实标未观测。

**场景 21**（跑着一条 20s 的执行器任务与一条 10s 的普通任务，`taskkill /F` 之后原目录重启）
```
强杀前：running=1 exec_running=1；两条任务都是 running
taskkill /F 返回=0，进程退出码=1
落盘状态（直接读 data/jobs.json，整数按 core/job.go 的 iota 翻回名字）：
   exec=running  普通=running        ← 强杀来不及写结论，所以盘上是 running 快照
重启后：exec 任务状态=paused（restore_policy=pause），普通任务=running → 后续终态 success
事件里的 metadata.reason=['restore_after_crash']
汇总日志：level=INFO msg="paused executor jobs after crash" count=1 reason=restore_after_crash
重启这轮的 error 行=[]
```
（`docs/deployment.md:281-288` 的既有描述说的是"优雅关闭把被打断的任务落成 pending"，
本场景走的是崩溃那一支：只有强杀才留 `running` 快照、才会被 restore guard 停住——
代码里 `core/scheduler.go` 的注释明确区分这两支，实测与之一致。）

**场景 22**（`reload.debounce` 200ms→800ms，然后用新窗口判合并）
```
result=ok applied=['reload.debounce']
启动时那行读数是旧值：msg="config hot reload enabled" … debounce=200ms
应用后没有第二行说新窗口：只有 applied_keys=reload.debounce（下一次排期起用新值）
[800ms 窗口内、间隔 400ms 两次写入] → result=ok，重载次数=1，applied=['logging.level','scheduler.workers']
对照（改回 200ms、同样 400ms 间隔两次写入）→ 重载次数=2
判据取的是 last_attempt_at 的推进次数；复位那一次 applied 含 reload.debounce
```

### 10.5 待做项 N1–N7 的实测核对

七条都在真进程里量过，结论都是"确实没做"。工具是 `r07smoke\sn.py`（三种参数：`13` 打 N1–N3、
`56` 打 N5–N6、`7` 打 N7），起点部署是 `executors.enabled=true` + `concurrency=2` +
`queue_capacity=2` + `workers=3`，档位两条（`sleep2` 带自己的 timeout、`notimeout` 不带）。

| # | 卡面核对方式 | 实测读数（原样） | 结论 |
| --- | --- | --- | --- |
| N1 执行器池的运行期扩缩 | 改 `executors.concurrency` → 进 `ignored_keys`，且执行器池读数不变 | 起点 `exec_workers=2 exec_queue_capacity=2`；`2→5` 之后 `result=ok`、`ignored_keys=['executors.concurrency']`、`applied_keys=[]`，改后 `exec_workers=2 exec_queue_capacity=2`。卡面写的 `/pools` 不存在（D-R0701），读数取自 `/admin/runtime` 的 `scheduler` 对象 | 没做：读数一字不动，键只进重启档清单 |
| N2 顶层 timeout 与 `required_role` | 改 `executors.default_timeout` 与 `required_role` → 都进 `ignored_keys`，既有档位的生效超时不变 | `result=ok`、`ignored_keys=['executors.default_timeout','executors.required_role']`；档位生效超时改前改后都是 `sleep2=30s notimeout=30s`，`/executors` 的 `required_role=admin`、`max_timeout=5m0s` 一字未变 | 没做 |
| N3 三个 interval/capacity | 改 `store.flush_interval`、`observability.flush_interval`、`observability.queue_capacity` → 三条都在 `ignored_keys` | `result=ok`、`ignored_keys=['observability.flush_interval','observability.queue_capacity','store.flush_interval']`、`applied_keys=[]`；这三次改动之后监听器仍活着（随后一次正常热更 `result=ok`） | 没做 |
| N4 凭据热更 | 场景 15 已覆盖 | 见 §10.4 场景 15：三种凭据各改一次都 `rejected`，旧 token 打 `GET /api/v1/jobs` 恒 200、新 token 401 | 没做（而且是拒绝档，不是"接受但不应用"） |
| N5 配置端点 | `GET /api/v1/config` 与 `POST …/config/reload` 都回 404；`web/` 里 grep 不到 reload 相关调用 | 四个候选全部 404：`/api/v1/config`、`/api/v1/config/reload`、`/api/v1/admin/config/reload`、`/api/v1/admin/config`。`grep -rn "reload" web/src` 只命中 `location.reload` 一类浏览器 API，没有任何指向后端配置端点的调用 | 没做：一个端点都没加，鉴权面因此没有变大 |
| N6 配置变更台账 | 台账里搜不到 `config.reload` 这类动作行 | `GET /api/v1/admin/audit` 在这轮现场里的动作词集合只有 `auth.login` 与 `unmatched`，总行数 3；本次会话产生的重载日志行有 2 条，它们在日志与 `/admin/runtime` 里，台账里一格都没多 | 没做：重载不进 `write_audit`（口径与 `sqlite-observability-design.md` §6.3 一致） |
| N7 与目录加载器共用防抖 knob | 改 `reload.debounce` 后目录加载器仍按 100ms 工作：往任务目录投两个文件验证 | `debounce 2s→200ms` 热更 `result=ok applied=['reload.debounce']`；但**服务端二进制里根本没有目录加载器**——`/job-types` 200 且没有 loaded-from-dir 的痕迹，场景目录里也没有加载器建的子目录（现场只有 `config.yaml data exec-workspace jobs server.log watchdir`）。代码事实：`core/load.go:94` 的 `loaderDebounceInterval = 100 * time.Millisecond` 是**常量**，`cmd/server` 对 `DirectoryLoader` 的引用数是 0 | 没做，而且这一条在真进程里**测不到**：目录加载器只作为库存在，装配方（`cmd/server`）从不建它。所以"两个 knob 没合并"的证据只能取代码事实：`reload.debounce` 走 `ConfigWatcher.SetDebounce`（可变），加载器的窗口是常量（不可变）——登记为部分实测 |

一句补充：N1–N3 的三次改动都落在 `ignored_keys` 而不是 `rejected_keys`，这正好是本系列
"接受但不应用"与"整次作废"的分界；每次改完都还能继续热更（最后一行 `result=ok`），
说明重启档的键不会把链卡住。

### 10.6 缺陷登记与 grep 命中清单的逐条处置

编号一律带系列名"配置热重载系列"（DoD 最后一条；档位在线管理系列的 D-0902 是重号教训的出处）。
同一批编号在设计文档 §14.4/§14.5 里有对应的处置说明，两处不重复展开。

#### 甲、缺陷登记表

| 编号 | 严重度 | 现场 | 处置 |
| --- | --- | --- | --- |
| 配置热重载系列 **D-R0701** | Minor（卡面前提错） | §3.1 与 §3.4 把并发/队列读数的判据写在 `GET /api/v1/pools` 上，本仓从来没有这个端点（实测 404） | **卡面就地改正**：判据改取 `GET /api/v1/admin/runtime` 的 `scheduler` 对象；§3.1 表头那句"判据一律取自……`/api/v1/pools`"同步改掉，设计文档 §8 那一行也改（§14.2 第 7 条）。不是产品缺陷 |
| 配置热重载系列 **D-R0702** | **Important（真实缺陷，已修，但修了三轮才修到底）** | 空文件/纯注释文件被 `core.LoadConfig` 读成一份合法的"全默认"配置，重载链照走、`result=ok`，现网取值换成代码默认（场景 16E 量到：`workers 7→100`、档位表清空、级别 debug 回 info）。带凭据的部署只是碰巧被拒绝档挡住 | **本卡已修**：终态形态是 `cmd/server/reload.go` 的 `configFileCarriesNoValues` + `yamlCarriesNoValue`，在链的第 1 步之前按 **YAML 结构**判"这份文件还表达任何取值吗"。过程要如实记：`f66472c` 那一版按文本逐行扫，第一轮复核抓到 BOM/UTF-16 放过（D-R0709）、第二轮抓到标记前缀误拒（D-R0711）、第三轮才抓到**文本层根本判不准**（D-R0715：`logging:`、`workers: null`、`{}`、多文档都读成全默认）。判红点 M1/M3/M4/M7（§10.3，原始输出留档 `r07smoke/mut_final/`），真进程证据是场景 16 / 16E / 16F / 16H。依据：R06 卡 §3.1 第 1 步"绝不退回默认值" |
| 配置热重载系列 **D-R0709** | Important（D-R0702 第一版的漏口，第一轮 fresh-context 复核发现） | 第一版守卫直接 `string(data)` 逐行扫，而 `strings.TrimSpace` 不认 U+FEFF，于是"带 BOM 的只有注释"与 PowerShell 5.1 重定向默认写出的带 BOM UTF-16LE 空文件被判成"有内容"放过——守卫注释自己举的那个现场从它脚边漏过去（当时的变异 M3b 读出 `result="ok"`） | **本卡已修，但修法被第三轮的重写吸收**：当时补的是 `configFileText`（自己还原三种 BOM/UTF-16 前缀），终态里这个函数**已删除**——判据改用 YAML 解析器之后，解析器自己认得 BOM 与带 BOM 的 UTF-16，还原字节这一步不再存在。这一族的真进程复测留在场景 16F（终态字节 01:47:48 那一份），单元层保留 UTF-8 BOM / UTF-16LE / UTF-16BE / 只有 BOM 四格与"不带 BOM 的 UTF-16 走第 1 步"那一格 |
| 配置热重载系列 **D-R0710** | Important（注释过度承诺，fresh-context 第二轮复核发现） | `configFileCarriesNoKeys` 那段注释的第一版写着"这一判据与 `core.LoadConfig` 读的是同一次存盘的两次读……窗口是微秒级，且真被换成默认取值的那一侧由第 1 步之后的 Diff 与拒绝档兜住，这里不额外加锁"。复核读码指出两点：① 那是两次独立的 `os.ReadFile`，中间被截断写入时本判据答的是截断前那一份；② "由 Diff 与拒绝档兜住"对这一形状并不成立——空文件读出来的全默认配置与生效那份必然不一致，`Diff` 会把它判成一批热更键、拒绝档里也没有它，于是照走应用，正是 D-R0702 那件事在竞态窗口里复活。注释描述了实现没有提供的保证 | **本卡已修（注释）**：改成如实陈述——窗口是微秒级、要靠紧接着的下一次存盘才可能撞上，撞上那一次本判据补不回来，下一次事件会再判一次。真要收口得让 `core.LoadConfig` 接受内容入参（那是 `core` 的接口改动，超出本卡"零生产代码变化"的目标），代码不动、不加锁、也不补第二次校验，代价写在同一段注释里 （第三轮把判据整体换成按 YAML 结构判之后，这段注释重写过一次，留下的仍是同一句如实陈述：竞态窗口没法在这道问里补判，收口需要 `LoadConfig` 接受内容入参） |
| 配置热重载系列 **D-R0711** | Important（D-R0709 修复自身引入的过头判据，第二轮复核发现） | 分隔符那一跳从"逐字相等"改成"前缀匹配"之后，`strings.HasPrefix(trimmed, "---")` 会把 `--- {logging: {level: debug}}` 这种"标记后紧跟真取值"的整行连内容一起跳掉，于是**一份有内容的单行流式配置**被判成"一个键都没有"而 `rejected`——守卫反过来误伤合法配置 | **本卡已修，同样被第三轮的重写吸收**：当时补的是 `cutDocumentMarker`（剥掉标记后再看剩下的是什么），终态里这个函数**已删除**——标记、流式写法、多文档这些边界本来就该由结构解析回答。判据侧保留的形状（`流式映射跟在标记后面`、`四个横杠不是文档标记`、`三个点后面紧跟键`）搬进了 `TestConfigFileCarriesNoValuesShapes`，真进程侧是场景 16G（终态字节 01:47:53）与 16H |
| 配置热重载系列 **D-R0712** | Minor（记录侧三条，不改行为） | ① §10.3 的变异编号在终态字节上重跑时换了写法（第一轮在 `reload.go.bak2` 字节上的 M3、M4 到终态字节分别记作 M3b、M5，同一形状），正文没写这层映射，读者会以为漏跑两条；② §10.6 丁 抄的验收 grep 命中清单按行号引用，而本卡每编辑一次行号就漂一次，第一版还漏记了本卡自己的两处自命中；③ §10.3 末尾那句"上面正文抄的是最后一轮（00:53–01:0x）"抄写时还是个占位 | **本卡已修（记录）**：② 丁 两段改成按内容引用、自命中逐条列出并说明它为什么消失；③ 占位换成真实读数。① 这一条随判据重写**作废**：文本判据那套变异（M1/M2/M3b/M5/M6）整体不再有意义，终态的变异名单是 §10.3 重写后的 M1–M7，且每条都留了原始输出（新登记 D-R0716 记的就是这件事） |
| 配置热重载系列 **D-R0713** | Minor（流程侧，本卡自己重犯 R05 的 D-R0515） | 第三轮 §3.2 全量验证（00:59:38 起跑）与 M6 那条变异的还原过程重叠，没法证明那一轮编译读的是终态字节；另外 `run_final_tests.sh` 把输出固定写到同一个文件名并 `: > "$OUT"` 截断，于是 00:11 那一轮的读数被 00:59 那一轮覆盖、**四轮里只留下三份留档**（复核第一条 Important 量到的就是这两件事） | **本卡已修**：判据重写之后在"已还原并核过 `sha256`"的字节上整轮再跑一遍（第五轮，`r07smoke/v_final_tests5.txt`），§10.3 正文与 README 状态行只抄这一轮；每一轮另起一个文件名、不再截断复用。口径同 R05：**变异与全量验证必须串行**，并行时任何一轮都不能当终态证据 |
| 配置热重载系列 **D-R0714** | Minor（记录/覆盖范围） | 真进程的 22 条场景与 16E 是在 00:44 那次 `go build` 的 `server.exe` 上测的，而第二、三轮都又改过 `cmd/server/reload.go`，所以那一批读数字面上不是"终态字节"下的读数；§10.4 一开始还写着"用的二进制是本卡终态字节" | **本卡已修（记录 + 补测）**：§10.4 现场段改成如实说明新旧边界，并在 01:45 用终态字节（`reload.go` sha256 `765fbac5…`）重建 `server.exe` 后重跑与这条判据相关的三格——16F（01:47:48）、16G（01:47:53）与新增的 16H（01:47:35，专测第三轮那一族"有键名没取值"）。1–22 与 16E 没有全批重跑，理由写在同一节：重写只改变"哪些文件被判成没取值"，那一批里只有 16 那一族走这条判据 |
| 配置热重载系列 **D-R0715** | **Important（真实缺陷，已修；第三轮 fresh-context 复核发现）** | 前两版的守卫都在**文本层**判空，而"这份文件表达了什么"是结构问题。`logging:` + `level:`（只有键名）、`scheduler: {workers: null}`、`logging: {}`、顶层 `null`、层层都是空键、以及"值写在第二份文档里"（viper 只读第一份）这六族文本上"有内容"，`core.LoadConfig` 一律读出**一份全默认配置**，链照走、`result=ok`、现网被换成代码默认——D-R0702 原样复现。复现方式：把临时判据用 `go test -overlay` 挂进 `cmd/server`（不落仓库文件），同时问判据、`LoadConfig` 与整条链，六族的答复是 `noKeys=false / 全默认=true / result="ok" / workers 4→100`。判据侧同样有洞：`configFileCarriesNoKeys` 返回 error 那一支零覆盖，日志与 `hint` 两行没有逐字断言 | **本卡已修**：判据换成按 YAML 结构判（`configFileCarriesNoValues` + `yamlCarriesNoValue`，用的就是 viper 那一族解析器，所以两边对"表达了什么"的答案同轴），`configFileText` / `cutDocumentMarker` 一并删除；`gopkg.in/yaml.v3` 因此从 indirect 提到直接依赖。判据侧补：`TestReload_ValuelessFileIsSameConclusion`（7 格，每格先核 `LoadConfig` 真能读通）、`TestReload_StepOneShapesKeepTheirOwnWording`（Tab 缩进 / `----` 开头 / 不带 BOM 的 UTF-16 / 文件被删，判"两种脸色不串门"）、`TestConfigFileCarriesNoValuesShapes` 里"文件不存在交回错误"那一格、以及 `TestReload_EmptyFileKeepsApplied` 对日志与 `hint` 的逐字断言。真进程复测见场景 16H，判红点见 §10.3 的 M1/M3/M4/M7 |
| 配置热重载系列 **D-R0716** | Minor（记录不可复算，本轮复核发现并已修） | 三类假账：① §10.3 说"这台机器上跑了四轮、读数都在四个文件里"，实际脚本每次截断同一个 `v_final_tests.txt`，00:11 那一轮的读数早已被覆盖，`v_final_tests.txt` 与 `v_final_tests3.txt` 是同一轮（md5 相同）；② 变异名单写"脚本在 `mut5.py`（前四条）与 `mut6.py`（M6）"，而 `mut5.py` 的锚点 `strings.HasPrefix(trimmed, "#")` 在终态字节里根本不存在（它跑的是第一轮 `bak2` 那份），`mut6.py` 的循环是 `MUTS[1:]`、M1 从没被它跑过，且七条变异**都没有留原始输出**；③ 几处小口径：`ok godelayq/cmd/server 0.333s` 与留档的 0.258s 对不上、gofmt 那条注释把"本仓工作树是 CRLF"用在这三个 LF 文件上、D-R0702 的"修复前 workers 7→100"没有留档（`out_s16e.txt` 是修后那份）、丁 第三条 grep 只举了三处命中而实际二十来行 | **本卡已修（记录）**：① §10.3 只把有留档的那一轮当终态证据，并如实写明 00:11 那一轮无留档；验证脚本每轮一个新文件名、不再复用。② 变异在终态字节上整批重跑（M1–M7），每条的原始输出留档在 `r07smoke/mut_final/M<n>.txt`，名单里的红/绿数与首个红消息都由脚本从原始输出算出来；旧的那套文本判据变异随实现一起作废。③ 三处口径逐条改成与留档一致（0.359s 与 54 条宇宙、LF/CRLF 那句改成"这三个文件是 LF，`gofmt -l` 直接跑原文件即有效"、16E 修复前的读数标明"来自当时的现场记录，无原样留档"、丁 的第三条 grep 改成给命中数并说明为什么只列代表行） |
| 配置热重载系列 **D-R0717** | Minor（跨文档矛盾，本轮复核发现并已修） | 同一件事在四处说法不一致：设计文档 §2 落地位置 R3 仍写 `reloader.Reload`"七步固定顺序"（代码头注释是八步）、§14.3 与 §14.5 写"三条用例 + 四个变异体"（终态是七条用例 + 七条变异）、DoD 里"设计文档 §14.5 每行都带前缀"不成立（那张表的编号列本来就是裸 `D-R07xx`，前缀规则管的是卡内缺陷表）、本目录 README 的 R07 行写"另加两条补测 16E、16F"（终稿是三条，含 16H） | **本卡已修（文档）**：四处逐条对齐到终态（七步→八步、计数改成实际的七条用例与七条变异、DoD 那句改成"卡内缺陷表每行带前缀，设计文档 §14.5 的编号本身已含系列号"、README 行补 16H）；`docs/deployment.md` 与 `docs/api.md` 里凡引用旧文案"一个键都没有"的地方一起换成终态那句 |
| 配置热重载系列 **D-R0703** | Minor（语义固有） | 只写一部分键的文件会把没写的键退回默认值并生效 | **登记不修**：这是"整份文件是唯一真相" + R04 整表替换的必然结果，守卫只认"一个键都没有"。处置在文档：`docs/deployment.md` 运维提示"删键不等于让它空着"，两份 YAML 的 `reload:` 注释同一条口径 |
| 配置热重载系列 **D-R0704** | Minor（卡面前提错） | 场景 11/12/13 的判据写"`applied_keys` 含 `executors.commands`"，实测给的是一族 `executors.commands.<名>.<字段>`（26 条），裸键只在整列表清空时才出现 | **卡面判据改按前缀族**；口径写进设计文档 §14.3 第 2 条与 `docs/api.md` 的字段表。不是产品缺陷：§6 开头一句"判定单位是叶子键"就是这个意思 |
| 配置热重载系列 **D-R0705** | Minor（卡面前提错） | 场景 15 的判据"`rejected_keys` 各自给出对应键名"，实测是累计清单（第二次给两条、第三次给三条） | **卡面判据改正** + `docs/api.md` 字段表与设计文档 §14.3 第 3 条写明"它是本次与生效那份不一致的全部拒绝档键"。不是产品缺陷 |
| 配置热重载系列 **D-R0706** | Minor（既有行为，非本系列） | 卡面场景 4 想用环境变量打开热重载，实测没有配置文件时整组 `GODELAYQ_*` 都不生效（`LoadConfig` 只在读通文件那一支做 `UnmarshalExact`） | **登记不修**：改它等于改 `LoadConfig` 的来源优先级，超出本系列范围。场景拆成 4A/4B 两条如实记录，`docs/deployment.md` 运维提示补一条 |
| 配置热重载系列 **D-R0707** | Minor（文档说法偏松） | `docs/api.md` 的 `last_applied_at` 写成"最近一次应用成功的时刻；只在 `result=ok` 时推进"，读起来像跨尝试的历史值；场景 15 收尾实测：一次失败的重载之后这个键消失 | **文档已改**：`docs/api.md` 字段表与 `docs/deployment.md` 读数小节都写明"按次替换的快照，只说最近一次尝试，要历史翻日志"。代码不改（按次替换是 R05/R06 定下的形状） |
| 配置热重载系列 **D-R0708** | Minor（测量工具的错，登记以免后人误读） | 三处工具侧的错：① 场景 9 第一版判据把事件一侧写成 `total <= 40`，而 `/api/v1/events` 的响应形状是 `{count, items, note}`、根本没有 `total`，于是读出 `False`；② 场景 20 的 harness 调的是 `SetConsoleCtrlEvent`——Win32 里投递 Ctrl+C 的那个 API 叫 `GenerateConsoleCtrlEvent`，`SetConsoleCtrlEvent` 是 pywin32 的包装名，ctypes 的 `windll.kernel32` 上取不到它（`AttributeError: function 'SetConsoleCtrlEvent' not found`）；③ 场景 21 用 API 去读"强杀之后盘上的状态"，而那时进程已经没了 | **改判据重跑**：①事件按 `count` 判、台账按 `total` 判，结论 True；②换成 `GenerateConsoleCtrlEvent` 并打出 `GetConsoleWindow/AttachConsole/sent` 三个现场值（结论见场景 20）；③直接读 `data/jobs.json` 并把整数状态按 `core/job.go` 的 iota 翻回名字。三条都是测量工具的错，不是产品行为 |

#### 乙、未观测项（一律如实标出，不写"应该没问题"）

| 项 | 为什么没测到 | 现有的最近证据 |
| --- | --- | --- |
| 22 条真进程场景在**终态字节**下的全批重测 | 判据在第三轮复核之后整体重写（D-R0715），真跑一遍 22 条要一整轮 harness；而这一轮的差异只可能改变“哪些文件被判成没取值” | 与这条判据相关的三格都在终态字节上量过：16F（01:47:48）、16G（01:47:53）、新增的 16H（01:47:35）；其余十九格与这条判据无关（读数取自 `/admin/runtime`、REST 与日志，走的都是正常取值的写入），边界登记为 D-R0714 |
| 内嵌形态（`-tags dashboard`）在终态字节下的浏览器实测 | 浏览器那一轮跑在 00:46 那份构建上，第三轮之后没有再开一次浏览器 | 终态字节重建过同一个内嵌二进制（01:54:52，exit=0），`api/reload_state.go` 两版之间只改过注释；`reload` 读数形状的真进程证据由 16F / 16G / 16H 与 §10.3 那份读数承担 |
| 场景 1 的"goroutine 数与 R06 之前同量级" | 真进程没有可读的 goroutine 计数端点（`/admin/runtime` 与 `/stats` 都不给），而本卡不许加端点；pprof 在本仓没装配 | 同一场景里可判的三半都过了：`reload` 键不出现、日志 `hot reload=0 watcher=0 reload=0`、改文件后 INFO 行继续长（说明没监听器）。`reload.enabled=false` 时"一个 goroutine 都不建"由 `cmd/server` 的装配用例守（`cfg.Reload.Enabled` 那一支整个不进） |
| 场景 20 的真进程优雅关闭 | 这一台 shell 里 Ctrl+C 投不进去：`GetConsoleWindow=0`，`AttachConsole(目标 pid)` 与 `GenerateConsoleCtrlEvent` 都返回真，但目标 10s 内不退出、日志里没有 `shutting down server`；`taskkill` 不带 `/F` 返回 rc=1 | `go test ./cmd/server -run TestRun_CloseOrder -v` → `--- PASS (0.02s)`，它判的是四步调用的流水次序（`watcher.Close → chain.Stop → server.Stop → scheduler.Stop`），不依赖信号投递 |
| Linux / macOS 的行为 | 本卡只做交叉构建（三条都 exit=0），没有真机 | 交叉构建 + `executor/proc_unix.go` 既有单测；fsnotify 的事件形状只在 Windows 量过（场景 17/18/22 的合并行为是 Windows 现场） |
| N7 的"投两个文件看加载器仍按 100ms" | 服务端二进制里不装目录加载器（`cmd/server` 对 `DirectoryLoader` 引用数 0） | 代码事实两条：`core/load.go:94` 是常量、`reload.debounce` 走 `ConfigWatcher.SetDebounce` 可变（§10.5 第 N7 行） |
| R06 交接给 R07 的"Linux 信号那一轮" | 见上一条 Linux 行——这一台机器上做不了 | 这条交接**没闭合**，写进本目录 README 的"阶段进度与未验证项"，不留空白 |

#### 丙、设计文档 §12 风险表的实测归属（§4 第 5 步）

| 风险行 | 实测归属 |
| --- | --- |
| Windows 事件形状不可预测 | 场景 17（原子存盘：一个窗口只推进一次 `last_attempt_at`，之后仍能触发）与场景 18（三连改合并成一次）；等价内容由 `Diff` 判回 `unchanged` 的那一半在场景 16 末尾"修好文件"那一步量到（`result=unchanged`） |
| 回滚链比应用长 | 单测面是 R06 卡 §5.1 #2（逐键断言回滚）；真进程面只有**部分**现场——场景 14 与场景 15 证明的是"拒绝发生在一个落点都没动之前"，真正的"应用中途失败 → 逆序重放"这一格在真进程里没法自然造出来（要造得注入故障，而本卡不许加故障注入口）。按卡 §4 第 5 步的口径登记为**部分实测** |
| 重载与页面档位写入撞上 | 场景 19：交替 10 步（页面写 6 次、文件重载 4 次），三者自洽、无 5xx、无 panic、无 error 行；共用一把锁的代码事实在 §10.1 第 15 行 |
| 读数与真值短期不一致 | 场景 6 缩容那段：`workers` 读数立刻是 2，而 6 条在跑任务全部 `success`（`running` 高于 `Workers` 的窗口在日志里，不在读数里）；`RuntimeStats.Workers` 的注释口径"期望并发 vs 瞬时值"由 R03 卡守 |
| 环境变量与文件谁赢 | 场景 4A 反而量到更强的事实：没有文件时环境变量整组不生效（D-R0706），所以本期"只在文案提示"这条对策暂时不需要新增来源判定 |
| 免重启新增档位是"改文件的人"绕开重启的一条口 | 场景 11 免重启新增并跑成 `success`；边界那一半（越出冻结许可即整次失败）由 R04 卡 §5.2 的用例守，本卡没重跑，且 `env_allow` 那一半的说法已按 D-R0404 收窄 |

#### 丁、验收 grep 的命中清单与为什么保留

口径先说清（D-R0712 第二条）：这两条 grep 的搜索面是 `README.md docs web/src/content`，而本卡自己就在
`docs/` 底下，所以本卡会自命中——第一轮那两行自命中是因为这一节逐字抄了命中原文，改成"按内容描述、
把 `文件:行号` 写全"之后就消失了；第二轮的模式不含反引号，本卡的两条 grep 命令行（§7 一条、本节一条）
必然各命中自己。行号又随本卡每次编辑漂移，所以下面不按行号引用，按内容认这几行。
终稿实测：第一轮 2 条命中（本卡 0 条）、第二轮 6 条命中（本卡 2 条）。

```
# 第 3 条第一轮（终稿实测，2 条）
grep -rn "改 \`executors.commands\` 需要重启" README.md docs web/src/content
  docs/design/tasks/web-profile/README.md:66
  docs/design/web-profile-design.md:13
```

| 命中 | 处置 |
| --- | --- |
| 本卡 | 第一轮**零命中**：这一节此前抄过那两条命中的原文，构成 2 条自命中（复核的第二条 Minor 指出上一版漏记的就是这类行），现在按内容描述、只写 `文件:行号`，自命中消失 |
| `web-profile/README.md:66` | **保留**：那是档位在线管理系列自己的收口记录，引号里是它当时改掉的旧句原文，属于"上一轮事实"的存档，不是给使用者读的现势说法 |
| `web-profile-design.md:13` | **保留**：同上——它记的是"使用者实测确认过这条限制"的历史动机，正是 W 系列立项的理由；把它改掉等于抹掉来源 |

```
# 第 3 条第二轮（终稿实测，6 条）
grep -rn "页面上不能新增或编辑档位" README.md docs web/src/content
  本卡 §7 的那条 grep 命令行                       ← 自命中
  本卡 丁 的这条 grep 命令行                        ← 自命中
  docs/design/tasks/web-profile/README.md:66
  docs/design/tasks/web-profile/task-w08-console-ui.md:94
  docs/design/tasks/web-profile/task-w09-docs-and-verification.md:25
  docs/design/web-profile-design.md:14
```

| 命中 | 处置 |
| --- | --- |
| 本卡两条 grep 命令行 | **保留**：它们是命令本身，不是文案；这一模式没有反引号，写在文档里就必然自命中，除非把命令拆散——那等于让 §7 不可执行 |
| W 系列四处 | **保留**：全是历史引用（旧句原文或"本卡复核"的记录），使用者读的 `web/src/content/job-template.md`、`README.md`、`docs/api.md`、`docs/deployment.md` 都已经没有这两句 |
| 使用者面复查 | 另跑一条 `grep -rn "executors.commands" README.md docs/api.md docs/deployment.md web/src/content/job-template.md \| grep -v reload` 看有没有无条件的"要重启"。**终稿实测 25 行命中**（`README.md` 3 行、`docs/api.md` 13 行、`docs/deployment.md` 7 行、`web/src/content/job-template.md` 2 行），逐条看过没有一处是无条件的"改 `executors.commands` 要重启"：说的都是"这一族来自配置、在写端点里只读"（`docs/api.md:653/675`）、"与档位文件是两份来源、同名以配置为准"（`README.md:486`）、"摊平的键路径形状"（`docs/deployment.md:573/574`）这类与重启无关的事实；条件式那句"打开 `reload.enabled` 之后不重启即生效"都带 `reload` 字样，被这条 grep 自己排除掉了。上一版这里只举了三处命中当全集，是漏记（D-R0716 第三条） |