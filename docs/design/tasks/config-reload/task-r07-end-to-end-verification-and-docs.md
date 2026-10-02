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

每条都要在 `%TEMP%` 下一个独立目录里实跑（配置 + `data/` + 产物目录 + 独立端口），
判据一律取自日志、`/admin/runtime` 的 `reload` 对象、`/api/v1/pools` 与 REST 响应，
**不允许**用"看起来任务跑得快了"当判据。跑完删除临时目录。

| # | 场景 | 判据 |
| --- | --- | --- |
| 1 | `reload.enabled` 缺省（不写这一节） | 进程起来后 goroutine 数与 R06 之前同量级、`/admin/runtime` 无 `reload` 键、改文件没有任何反应（等三个窗口） |
| 2 | `reload.enabled: true` + `debounce: 10ms` | **启动即失败**，错误文案含 `reload.debounce`（R01 的下界） |
| 3 | `enabled: true` 且 `-config` 指向不存在的文件 | 启动失败（显式指定的文件缺失是错误，既有口径未变），不产生"盯着空气"的监听器 |
| 4 | `enabled: true` 但走默认值部署（没有 `configs/config.yaml`，也不给 `-config`） | 起服务、不建 watcher、日志里一条 warn 说明"用默认值运行，无文件可盯"（R06 §5.2 #13） |
| 5 | 改 `logging.level: info→debug→error` | 两次都 `result==ok`；debug 期间能看到 debug 行、error 期间只剩 error；`applied_keys` 各含该键 |
| 6 | 改 `scheduler.workers: 4→12→2` | `/api/v1/pools` 与 `/admin/runtime` 的 `scheduler.workers` 随两次改动变；缩到 2 时在跑的普通任务一条都不失败（提交 6 条各睡 1s 的任务，全部 `success`） |
| 7 | 改 `scheduler.queue_capacity` | `result==ok`、`ignored_keys` 含该键、`/pools` 的队列容量读数**不变**（下次重启才变） |
| 8 | 改 `store.history_limit: 1000→5` | 提交 8 条会很快终态的任务，`GET /api/v1/jobs?status=success` 的条数受新上限约束（`/stats` 的 completed 同步）；`applied_keys` 含该键 |
| 9 | 改 `observability.*.retention_*`（观测层启用时） | `result==ok`、`applied_keys` 含该键；下一个批量周期的淘汰按新值（构造一次超过新条数的事件量，查 `GET /api/v1/events` 的行数上界变化） |
| 10 | 观测层**未启用**时改同样两个键 | `result==ok`、两键进 `ignored_keys`、不失败（R06 §3.2 #5） |
| 11 | 改 `executors.commands` 增一条档位（执行器与鉴权都开着） | `GET /api/v1/executors` 多一行且 `source=="config"`、`GET /api/v1/job-types` 出现 `exec.<新名>`；不重启提交它跑成 `success`；`applied_keys` 含 `executors.commands` |
| 12 | 再从 `commands` 里删掉那条 | 该键的处理函数被摘除：`/job-types` 不再列出；提交它 400；既有任务留痕不丢 |
| 13 | 执行器**关闭**时改 `executors.commands` | `result==ok` 且有一条 warn 说明"未启用，本次不生效"（R06 §3.1 第三条），`/executors` 不因此多出档位 |
| 14 | 改 `executors.commands[].script`（档位内的执行许可字段） | `result==rejected`、`rejected_keys` 含该键、生效表一字未动（`/executors` 与 `/job-types` 与改前逐字节相同） |
| 15 | 改 `server.auth.token` / 加一个 `users` 条目 / 改 `jwt.secret` 三种各一次 | 三次都 `rejected`，旧凭据仍可用（用旧 token 打一次 `GET /api/v1/jobs` 得 200），`rejected_keys` 各自给出对应键名 |
| 16 | 写坏 YAML（未知键 / 缩进错 / 空文件 / 删掉文件）四种 | 四种都 `rejected`（空文件与删除的结论按 R06 §3.1 第 1 步，判据里写清各是什么错误文本），**当前生效值全部不变**（改前把 level 设成 debug、坏写入后仍是 debug）；修好后下一个窗口自动回 `ok`/`unchanged` |
| 17 | 一次编辑器式原子存盘（写 `config.yaml.tmp` 再 rename 覆盖） | 只发生一次重载（`last_attempt_at` 一个窗口内只推进一次），后续事件仍能触发（`watcher` 没死） |
| 18 | 连续快速改三个键（同一个窗口内） | 合并成一次重载，`applied_keys` 一次给出三个键（防抖真的合并了） |
| 19 | 页面档位写入与文件重载交替（`web_enabled: true` + `reload.enabled: true`） | 交替各 5 次后：`/executors` 的来源标注、`/job-types` 的注册表与登记表三者自洽；无 5xx、`-race` 无关（真进程），日志里没有 panic |
| 20 | 优雅关闭（`SIGINT` / `Ctrl+C`）在重载进行中 | 关闭日志顺序为 watcher 停 → server 停 → scheduler 停（R06 §5.2 #14 的真进程版）；无 goroutine 泄漏迹象（关闭在 `shutdown_timeout` 内返回） |
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
| N1 执行器池的运行期扩缩 | 场景 6 之外再做一次：改 `executors.concurrency` → 断言进 `ignored_keys` 且 `/pools` 的 exec 读数不变 |
| N2 顶层 timeout 与 required_role | 改 `executors.default_timeout` 与 `executors.required_role` → 都进 `ignored_keys`，且既有档位的生效超时不变 |
| N3 三个 interval/capacity | 改 `store.flush_interval`、`observability.flush_interval`、`observability.queue_capacity` → 三条都在 `ignored_keys` |
| N4 凭据热更 | 场景 15 已覆盖 |
| N5 配置端点 | `GET /api/v1/config` 与 `POST .../config/reload` 都回 404；`web/` 里 grep 不到 reload 相关调用 |
| N6 变更台账 | 台账页/`GET /admin/audit` 里搜不到 `config.reload` 这类动作行（本系列没登记任何动作词） |
| N7 与目录加载器共用防抖 knob | 改 `reload.debounce` 后，任务目录加载器仍按 100ms（`core/load.go:94` 是常量）工作：往任务目录投两个文件验证 |

## 4. 实现步骤

1. 先跑 §3.2 的八条命令，把输出原样抄进 §10.3（**先建立基线**，改文档不影响它们，但场景实测会）。
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

- [ ] §3.1 的 22 条场景逐条有实录，判据不成立的登记为缺陷并写清处置（修 or 不修 + 归属卡）。
- [ ] §3.2 八条命令全部跑过，输出抄在 §10.3；`TestExampleConfigMatchesLocal` 是 `--- PASS`
      而不是 SKIP（若本机缺 `config.yaml` 要写明）。
- [ ] §3.3 七处文档全部改到位，旧说法 grep 无残留（命中清单与逐条处置在 §10.6）；
      `docs/api.md` 的 `reload` 字段表与设计文档 §5.4 的字段名逐个对得上。
- [ ] 设计文档状态行改为"已实施"，§2 后面有"落地位置"段，并有一节逐条列出实现与设计的偏离
      （含待拍板 P1–P5 的实际答案）。
- [ ] §3.4 的 N1–N7 七条都有"未实施 + 实测证据"，不是只抄设计文档那段话。
- [ ] 本目录 README 的状态表七行齐全，"阶段进度与未验证项"里把平台受限项列清。
- [ ] `go test ./core ./executor ./cmd/server -race -count=5 -timeout 30m` 无 flake
      （本系列的用例大量等时间窗口，这条是防 flake 的主关口）。
- [ ] 若本卡动过任何 `*.go`（补缺口），改动逐条对应到某张前序卡的测试要求，并在 §10.2 写明。
- [ ] 缺陷编号一律带系列名"配置热重载系列"（档位在线管理系列的 D-0902 就是重号问题，别再犯）。

## 7. 验收方式

```bash
# 1) 确认零生产代码改动
git diff --stat
# 2) 全量验证
go build ./... && go vet ./...
go test ./... -race -count=1
go test ./core ./executor ./cmd/server -race -count=5 -timeout 30m
go build -tags dashboard ./cmd/server
# 3) 旧说法是否还有残留
grep -rn "改 \`executors.commands\` 需要重启" README.md docs web/src/content || echo "no-stale-claim"
grep -rn "页面上不能新增或编辑档位" README.md docs web/src/content || echo "no-stale-claim-2"
# 4) 守卫用例
go test ./core -run TestExampleConfigMatchesLocal -v
```

预期：第 1 条只有文档、两份 YAML 注释与本目录卡片；第 3 条两次都打 `no-stale-claim*`
（若仍有命中，逐条写进 §10.6 说明为什么保留）；第 4 条 `--- PASS`。

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

### 10.2 文档改动清单（新旧句对照）与本卡动过的 `*.go`

### 10.3 验证证据（八条命令的实际输出）

### 10.4 场景实测记录（22 条，含每条的写入片段与观察到的字段）

### 10.5 待做项 N1–N7 的实测核对

### 10.6 缺陷登记与 grep 命中清单的逐条处置
