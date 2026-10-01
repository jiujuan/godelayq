# TASK-W09　文档同步、全量验证与待做项登记

- 所属阶段：M4 收口
- 依赖任务：TASK-W01 … W08 全部
- 涉及文件：`README.md`、`docs/api.md`、`docs/deployment.md`、`docs/example.md`、`docs/design/web-profile-design.md`、`docs/design/executor-design.md`、`web/src/content/job-template.md`、本目录 `README.md`
- 预计规模：中

## 1. 任务目标

把"档位可以在线管理"这件事写进所有使用者会读到的地方，跑一遍本系列承诺过的全量验证，
并把设计文档 §7.2 的五条待做项与实施中新抓到的缺陷逐条登记成可查编号。
本卡不改任何行为。

## 2. 背景与当前问题

执行器那一系列收口时（`docs/design/tasks/executor/README.md` 与 SQLite 系列 S07）留过一条教训：
**文档与代码的偏差不会在运行期暴露**，只会让下一个人照着过期说明做决策。
本系列改动的恰好是"权限模型"，过期文档的直接后果是运维以为"改档位要重启"或以为"页面改不了什么"。

现在仍然写着旧事实的地方（本卡开工前先 grep 复核，卡片写的行号可能已被前几张卡改过）：

| 位置 | 现在写的 |
| --- | --- |
| `web/src/views/SettingsView.vue:100-104` | 改完要重启进程（W08 已改，本卡复核） |
| `web/src/content/job-template.md:272` | "页面上不能新增或编辑档位"（W08 已改，本卡复核） |
| `docs/design/executor-design.md` D1 | "加一条命令要改配置重启——与既有账号表口径一致，不新增例外" |
| `configs/config.example.yaml` 的 `executors` 注释（120-123 附近） | "能执行什么完全由这里决定" |
| `docs/api.md`、`docs/deployment.md`、`README.md` | 完全没有 `web_enabled` 这件事 |

## 3. 要实现的功能

### 3.1 文档改动清单

1. `docs/api.md`：三个写端点（请求体、状态码矩阵 400/403/404/409/500/503、角色 ops）、
   `GET /executors` 的四个新字段与顶层 `web_enabled`/`runtime_allow`、
   `path_display` 会对 reader 可见这件事。
2. `docs/deployment.md` 新增一节：
   - `executors.web_enabled` 与 `executors.profiles_path` 的取值、默认关闭的口径。
   - `exec-profiles.json` 的权限与备份：它是"能执行什么"的一部分，**必须随 `data/` 一起备份**；
     损坏时启动失败，删掉它可退回只看 yaml 的形态。
   - 多机部署：文件里的档位是**本机路径**，换机器后探测会失败（`available=false`），
     这是预期而不是故障；`env` 固定取值不进任何读口。
   - 一句明确的警告：打开 `web_enabled` 等于把"往执行面注入命令"的权限交给 ops 档 JWT，
     生产部署前应补设计文档 §7.2 的 S-1（路径白名单）。
3. `docs/design/executor-design.md`：在 D1/D2 那两行后面加一条 ⚠️ 指向 `web-profile-design.md`，
   按该文件既有的"落地位置"写法（`docs/design/executor-design.md:46-67` 那种格式），
   写清"档位来源从一处变两处、workspace 越界拒绝只对 config 侧保留"。
   **不要在那份文档里改写原决策本身**，它是历史决策记录，只加偏差标注。
4. `configs/config.example.yaml` 与 `configs/config.yaml`：`executors` 一节补两个新键的注释
   （W01 已加键，本卡复核注释是否说清了"默认关闭""文件不存在是正常状态""损坏会启动失败"）。
5. `docs/design/web-profile-design.md`：§2 决策表补"落地位置"段（每条 D 指向实际文件与函数），
   §7.1 那张偏离表按代码现状复核一遍，§12 的 P1/P2/P3 若有评审改动要回填。
6. 本目录 `README.md`：状态表逐行填完成日期与偏离摘要，
   "阶段进度与未验证项"与"遗留给后续卡的缺陷"两张表按 S07/E19 的体例写。

### 3.2 全量验证（逐条跑，逐条记输出）

```bash
go build ./...
go vet ./...
go test ./... -race -count=1
# 这一条必须带 -timeout：api 包一轮约 120-135s，五轮 1098s，超过测试二进制默认的 10 分钟上限。
# 照本卡原写法（不带 -timeout）跑出来是 `FAIL godelayq/api 600.319s` 加一片 goroutine dump，
# 那是超时不是正确性失败——见 §10.5 的 D-0901。
go test ./executor ./api ./core -race -count=5 -timeout 30m
go build -tags dashboard ./cmd/server
GOOS=linux GOARCH=amd64 go build ./cmd/server
GOOS=darwin GOARCH=arm64 go build ./cmd/server
GOOS=windows GOARCH=amd64 go build ./cmd/server
cd web && npx vue-tsc --noEmit && npm run build
```

`gofmt -l` 在本仓因 CRLF 全量误报，只做新增文件的 `gofmt -w`，不据其输出下结论。

### 3.3 端到端场景矩阵（临时目录冒烟，一条都不许省）

| # | 场景 | 预期 |
| --- | --- | --- |
| 1a | `executors.enabled=false` + `web_enabled=true` | **配置校验直接拒**：`executors.web_enabled requires executors.enabled to be true`，进程起不来（W01 §3.2 的配套规则，与 `loader_allow` 同一条） |
| 1b | `executors.enabled=false` + `web_enabled=false` | 写端点 503；不建文件；`GET /executors` 回 `enabled:false` |
| 2 | `enabled=true` + `web_enabled=false`（默认） | 写端点 503；不读文件；其余行为与本系列之前逐字节一致 |
| 3 | 两者都开，文件缺失 | 正常启动，`profiles` 只有 config 来源，文件不被创建 |
| 4 | 页面建 python 档位 → 不重启提交任务 | success，输出正文可读，产物落在 `executors.output.dir` |
| 5 | 页面建越界绝对路径档位 | 保存成功；探测按本机结论；`path_display` 给绝对写法 |
| 5b | （执行时补的一条）把同一个越界绝对路径写进 `executors.commands` 后启动 | 启动失败，原因是配置侧那句"必须相对 `executors.workspace`"——本卡的偏离只覆盖档位文件那一份来源 |
| 6 | 页面建与 yaml 同名 | 409；注册表不动 |
| 7 | 手改文件制造同名（改 yaml 后重启） | config 生效、store 条目 `degraded`，进程能起 |
| 8 | 文件写坏（截断 JSON）后重启 | 启动失败，文案含路径与"删掉该文件"的自救指引 |
| 9 | 删除有未终态任务的档位 | 未终态变 `paused`、running 未被打断；恢复后判失败 |
| 10 | 五种身份（viewer/operator/admin/ops/machine）打三个写端点 | 只有 ops 通过，其余 403 |
| 11 | 台账 | 三个动作各一行；整行搜不到 `env` 取值、脚本参数取值与请求体原文 |
| 12 | 重启留存 | 第 4 条建的档位重启后仍在，且 `available` 按新机器重算 |
| 13 | 崩溃恢复 | `exec.<store 档位>` 任务处于 running 时强杀进程 → 重启后被钉成 `paused`（守 §5.5 W05 那条顺序在真实进程里成立）⚠️ 判据是**存储里的** `running` 快照：接口的 `running` 早于落盘（`store` 是 200ms 合并写），照着"接口说 running 就立刻杀"做，磁盘上还是 `pending`，重启后照常重跑、守卫一行都不发（本卡第一轮就是这么失败的，见 §10.4） |
| 14 | 并发 | 两个 ops 会话同时 PUT 同一条 → 后写覆盖（无乐观锁，与分组一致），但**不产生**注册表半状态 |

场景 13 用强杀（`CTRL_BREAK_EVENT` 或任务管理器）而不是优雅停服，
Windows 下优雅停服那条路径已有先例可参照（SQLite 系列 S07 的记录）。

## 4. 实现步骤

1. 先跑 §3.2，把任何失败先修掉再动文档（文档描述"已经绿的东西"才有意义）。
2. 跑 §3.3 矩阵，逐条贴实际命令与输出；做不到的记进"未验证项"表。
3. 按 §3.1 清单改文档，每处改动都要能回答"读者从哪能找到新事实"。
4. grep 复核旧口径没有残留：
   `grep -rn "需要重启进程\|不能新增或编辑\|完全由这里决定" README.md docs web/src` ——
   剩下的每一处命中都要能解释为什么保留（例如 `config.yaml` 侧的 yaml 注释确实仍然成立）。
5. 更新设计文档的"落地位置"与本目录 README 状态表。
6. 最后一个提交（`docs(...)` 前缀）；若前面已按卡分提交，本卡不回头改历史。

## 5. 测试要求

本卡不新增用例，但要做两件事：

1. 把 §3.2 的输出原文进第 10 节（哪些命令、什么结果、耗时）。
2. 把 §3.3 的 14 个场景逐条记成"命令 + 实际响应 + 结论"，
   与卡片预期不一致的地方**按实际写**并登记成缺陷（编号沿用本系列 `D-09xx` 的写法）。

## 6. 完成标准（DoD）

- [x] §3.2 九条命令全部跑绿，输出进实现记录（§10.1；其中第五行按 D-0901 补了 `-timeout 30m`）。
- [x] §3.3 十四条场景每条有记录；做不到的进"未验证项"表并写明谁能补（§10.2 十四条 + 补的一条 5b，全部有实际值；§10.7 六行未验证项）。
- [x] §3.1 六处文档改完，且第 4 步的 grep 命中逐条有处置结论（§10.3 七行改动、§10.6 五行处置）。
- [x] 设计文档 §2 有"落地位置"段，§7.1 偏离表与代码现状一致（尤其 `resolveInside` 只在严格模式生效这一条）。
- [x] §7.2 五条待做项在设计文档里状态明确（未被"顺手做掉"、也没丢），并在本目录 README 复述一遍（那张表加"状态"列，README 末尾复述一次并逐条挂上实测证据）。
- [x] 本目录 README 的状态表与"阶段进度"两张表填完（W09 一行 + "阶段进度与未验证项" + "遗留给后续卡的缺陷" + "系列级待做项的现状"）。
- [x] 架构缺陷清单（项目记忆里的编号清单）同步：本系列新登记的编号写进去（`project-architecture-defect-backlog.md` 补一段"系列内 D 编号"的词汇说明，`project-web-profile-series.md` 记 D-0901…D-0905 与处置）。

## 7. 验收方式

```bash
grep -rn "需要重启进程\|不能新增或编辑\|完全由这里决定" README.md docs web/src configs
go test ./... -race -count=1
```

预期：第一条的每处命中都能对应到实现记录里的一行处置；第二条全绿。

## 8. 不在本任务范围

- 不做 S-1…S-5 任何一条的实现（它们只在文档里存在）。
- 不改任何生产代码路径；跑验证时发现的缺陷要**先记编号再判断是否当场修**，
  当场修的必须在本卡第 10 节写清改了什么、为什么。
- 不重构既有文档结构，不合并 executor 设计文档与本设计文档。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 只改了最新的那份文档 | 读者从 README 或 api.md 进来仍看到旧口径 | §3.1 六处 + §4.4 的 grep 复核 |
| 把 `executor-design.md` 的原决策改写掉 | 历史决策丢失，后来人不知道当初为什么这么定 | §3.1 第 3 条明确"只加 ⚠️ 标注" |
| 场景矩阵有"应该没问题"的省略 | 与 S07 的教训同一条 | DoD 要求每条有记录，缺一条即未完成 |
| 未验证项含糊带过 | Windows/Linux 差异被写成"已验证" | 参照 SQLite 系列与 E19 的"受平台限制没跑的项目"表体例，逐条列 |

回滚：本卡只有文档改动，逐文件 `git checkout --` 即可；
但如果本卡顺带修了缺陷，那些代码改动要单独成提交，不能混在文档提交里。

## 10. 实现记录（执行时补写）

### 10.1 全量验证（§3.2 逐条结果）

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | 通过，2.844s |
| `go vet ./...` | 通过，1.249s |
| `go test ./... -race -count=1` | 通过，4m8.3s：api 243.496s / cmd/server 5.960s / core 14.696s / executor 51.529s / store/sqlite 6.573s |
| `go test ./executor ./api ./core -race -count=5 -timeout 30m` | 通过：executor 122.509s / api 1098.190s / core 61.640s |
| `go build -tags dashboard ./cmd/server` | 通过，1.644s |
| `GOOS=linux GOARCH=amd64 go build ./cmd/server` | 通过 |
| `GOOS=darwin GOARCH=arm64 go build ./cmd/server` | 通过 |
| `GOOS=windows GOARCH=amd64 go build ./cmd/server` | 通过 |
| `cd web && npx vue-tsc --noEmit && npm run build` | 通过，`built in 4.30s`，产物里 `ProfilesView-*.js` 37.23 kB（gzip 12.14 kB） |

另单跑 `go test -run TestExampleConfigMatchesLocal ./core` → `--- PASS`（本机有 `configs/config.yaml`，
本卡改的 `executors` 注释两份同步过，是"通过"而不是"跳过"）。
`gofmt` 没跑：本卡零 Go 改动。api 在全仓那一轮是 243s、三包单跑那几轮是 118s，
差别来自它与 §10.2 的冒烟探针并行过；两轮都通过，耗时只作记录不作判据。

### 10.2 端到端场景矩阵（§3.3 十四条 + 本卡补的一条 5b）

驱动脚本 `%TEMP%\w09-smoke\matrix.py`（跑完已删）：配置、数据、产物、观测库全在临时目录里，
仓库的 `configs/config.yaml` 与 `data/` 一个字节没写。进程由脚本自己起停，
端口 8124（IPv4/IPv6 都空），四个账号 ops1/operator1/admin01/viewer01 + 一个静态 token。
最后一行 `MATRIX DONE: 36 checks passed`，退出码 0。下表逐条记实际值。

| # | 实际看到的结果 |
| --- | --- |
| 1a | `GODELAYQ_EXECUTORS_ENABLED=false` 覆盖住文件里的 `enabled: true` 后启动：退出码非零，日志原文 `load config failed: executors.web_enabled requires executors.enabled to be true` |
| 1b | 两者都关：`GET /executors` → `enabled=false`、`profiles=[]`、`web_enabled=false`、`runtime_allow=["python"]`（关闭态是 200 不是错误）；POST/PUT/DELETE 三条都是 503 `executor profile management is not enabled`；`exec-profiles.json` 没被创建 |
| 2 | `enabled=true` + `web_enabled=false`：列表 1 行、`source` 全 `config`、`editable` 全 false、顶层 `web_enabled=false`；POST 503，文案与 1b 同一条（两者本来就是同一个门）；文件仍未创建 |
| 3 | 两者都开 + 文件缺失：正常启动，列表只有 `exec.cfg_py`，**文件不被创建** |
| 4 | 页面建 `w09_py`（带必填参数 `day`、`env` 固定项、`env_allow`）→ 201，`runtime_ok=true source=store editable=true path_display=scripts/py_hello.py`；**不重启**提交 `exec.w09_py` → `success`，输出正文首行 `hello from godelayq executor`，产物端点 `count=1`，产物目录 `data/exec/01a0f876-…` |
| 5 | 越界绝对路径（真在 workspace 之外：`{冒烟目录}/outside/sleeper.py`）→ 201，`runtime_ok=true`，`path_display=C:\Users\...\w09-smoke\outside\sleeper.py`（绝对、`os.path.isabs` 为真） |
| 5b（补） | 把同一个越界写法放进 yaml 的 `executors.commands` 再启动：退出码 1，`executors.commands[0] "cfg_py": script "C:/…/outside/sleeper.py" must be relative to executors.workspace`——本卡的偏离只覆盖档位文件那一份来源 |
| 6 | 页面建与 yaml 同名的 `cfg_py` → 409 `profile name belongs to a configuration profile`，`exec.cfg_py` 仍一行、`source=config`，注册表没动 |
| 7 | 手改文件加进一条同名 `cfg_py` 后重启：进程起来，启动日志 `executor handlers registered total=3 registered=3 unavailable=0 degraded=1`；`exec.cfg_py` 两行并存，生效那行 `source=config`，降级那行 `source=store editable=false runtime_ok=false`、`reason=profile "cfg_py" is already declared in executors.commands, the stored one is not registered` |
| 8 | 把文件截断成坏 JSON 后重启：退出码 1，error 行含路径与自救指引（`parse executor profile file "…exec-profiles.json" failed: unexpected end of JSON input; …`），完整原文见 §10.2 末尾的日志片段 |
| 9 | 一条 pending + 一条 running 时 `DELETE …?jobs=pause` → 200 `{paused_jobs:1, running_jobs:0, already_paused_jobs:0}`；待执行那条变 `paused`、正在执行那条仍是 `running`；对另一条用 `?jobs=block` → 409 `0 pending, 1 running and 0 paused job(s) still use "w09_slow"`；那条 running 自己跑完 `success`；恢复被置 `paused` 的那条并把触发时间提前 → 最终 `failed`，服务日志 `no handler registered for job handler_key=exec.w09_del` |
| 10 | 四种 JWT 身份 + 静态 token 打四个端点（读定义 + 三个写）：ops 依次 200/201/200/200，admin、operator、viewer、machine 一律 403 |
| 11 | 台账 `GET /admin/audit?limit=200`：`executor.profile_create` 9 行、`executor.profile_update` 12 行、`executor.profile_delete` 4 行（含被 403/409 拒掉的那些）；整份响应里搜不到 `env` 固定取值、搜不到参数取值、搜不到 `args_render` 与 `"script":` 这类请求体原文 |
| 12 | 重启后页面建的两条仍在（`w09_py` timeout 已是第 14 条写进去的值、`w09_outside` `runtime_ok=true` 由本机重新探测）；`GET /executors/profiles/w09_py` 给 `env_keys=["REPORT_HOME"]`，响应体里搜不到那个取值，文件里它还在 |
| 13 | `exec.w09_crash` 任务：接口报 running 之后**再等 `jobs.json` 里那条的 `status` 落成 1**（1=running）才强杀，强杀后磁盘上仍是 1；重启后那条变 `paused`，`stats paused=1 heap=0 running=0`，启动日志有 `paused executor jobs after crash count=1 reason=restore_after_crash`，任务事件里带同一来源标记 |
| 14 | 两个 ops 会话并发 PUT 同一条，5 轮：状态码每轮都是 `[200, 200]`，最终 timeout 落在 `1m30s`（2 轮）或 `3m0s`（3 轮）＝后写覆盖、没有乐观锁（与分组一致）；每轮之后该注册键在列表里恒为一行，没有半状态 |

### 10.3 文档改动清单（§3.1 六处 + 两处顺带）

| 位置 | 改成什么 |
| --- | --- |
| `README.md` | 核心文件表新增 `core/executor_profile_store.go` 一行；目录树补 `executor_profile_store.go`、`api/handlers_executor_profiles.go`、`executor/merge_profiles.go`、`executor/applier.go` 四个文件，`registry.go` 的说法从"只读登记表"改成"运行期可整表替换的登记表"，`profile.go` 补"两种路径模式"；特性列表里"执行器档位"那段重写为两份来源 + 立即生效 + 活过重启；`executors:` 示例补 `web_enabled` 与 `profiles_path` 两行；文档索引补档位在线管理的设计文档与 TASK-W01…W09 卡片链接 |
| `docs/api.md` | 本系列前几张卡已写全（状态码矩阵、四个新字段、`web_enabled`/`runtime_allow`、`path_display` 对 reader 可见）。本卡补两处：门禁早退导致的连接重置现象与调用方处置（§档位的在线管理末尾），`path_display` 的写法是"解析并归一后的绝对路径"而不是请求体原文 |
| `docs/deployment.md` | "开启执行器"新增第 13 条：两个键与默认关闭口径、两条硬前提、必须随 `data/` 一起备份、`0640`、文件不存在是正常状态、损坏即启动失败并给出原文自救文案、多机部署把档位当本机文件（探测失败是预期）、ops 档 JWT 的权限警告（点名 S-1/S-2）、`env` 取值不进任何读口 |
| `docs/design/executor-design.md` | **没有改写 D1/D2 原决策**，只在"落地位置"块开头加两条 ⚠️：来源从一处变两处、"能执行什么"的权限扩大到 ops 档 JWT；workspace 越界拒绝只对配置侧保留，档位文件走 `PathAnywhere`/`resolveAnywhere`，判越界用 `withinDirectory`。§7 风险表的"路径逃逸"一行同步标注 |
| `configs/config.example.yaml` + `configs/config.yaml` | `executors.commands` 上方那句"能执行什么完全由这里决定"换成三行：这是两份来源之一、本节改动仍需重启且同名以本节为准、两种来源都不接受自由命令行与内联源码 |
| `docs/design/web-profile-design.md` | 头部状态改成"已实施（W01…W08，W09 收口）"；§2 决策表后补"落地位置"段（每条 D 指向实际文件与函数，行号按当前代码 grep 核实）；§6.8 的 `path_display` 措辞修正；§7.1 偏离表的引用刷新并加一条"页面那条记录读口同样是 ops 档，没有替 reader 多开面"；§7.2 加"状态"列，逐条写明 S-1…S-5 未实施与实测证据；§12 确认 P1/P2/P3 三条推荐值照原样成立 |
| `docs/example.md` | 目录加载器拒绝 `exec.*` 那一段补两句：档位来自哪一份来源都不影响这条拒绝 |

### 10.4 与本卡写法的偏离

1. **场景 13 第一轮是真的失败，失败在卡面判据不足**：卡写"`exec.<store 档位>` 任务处于 running 时强杀"，
   而守卫的判据在 `core/scheduler.go` 的 `Restore` 里读的是**存储快照**（`askRestoreGuard(snap)`），
   `store` 是 200ms 合并落盘。第一轮按"接口报 running 就立刻杀"做，磁盘上还是 `pending`，
   重启后那条照常重跑、`paused executor jobs after crash` 一行都不发——这不是产品缺陷
   （`docs/deployment.md` 第 9 条原话就是"判据是存储里的 `running` 快照"），是卡面少写了一句判据。
   已把判据补进 §3.3 第 13 行，第二轮按"等 `jobs.json` 里 `status=1` 再杀"跑通。
2. **场景 5 的"越界"得真的越界**：第一版冒烟把脚本放在 `{冒烟目录}/workspace/scripts/` 下写成绝对路径，
   那是 workspace **之内**，`path_display` 给相对写法是正确行为，被我的断言判成失败。
   改成 `{冒烟目录}/outside/sleeper.py` 之后给出绝对写法（顺带抓到措辞问题，登记 D-0905）。
3. **补了一条 5b**：卡只验"页面那份来源放开"，但本卡改的 `executor-design.md` 与 `deployment.md`
   都承诺"配置侧口径不变"，只验半边等于把这句承诺留在原地没人核。5b 用同一份越界路径写进 yaml，
   确认启动即拒。这一条已回填进 §3.3 表格并标注"执行时补"。
4. **`-timeout` 补进 §3.2**：见 D-0901。第一轮照卡面命令跑出的 `FAIL godelayq/api 600.319s` 是超时，
   goroutine dump 停在 `api/history.go:58-59` 那个长期存活的缓冲协程上，与正确性无关。
5. **冒烟脚本给所有请求加了最多三次重试**（只在连接层失败时），并打印每一轮：见 D-0904。
   最终那轮的 `[retry …]` 行都在日志里留着，没有把现象抹平成"一次就过"。
6. **本机 `configs/config.yaml` 一起改了注释**（卡 §3.1 第 4 条要求），它被 `.gitignore` 排除，
   所以提交里只有 example 那份；`TestExampleConfigMatchesLocal` 是 `--- PASS`。
7. **§4.4 的 grep 命令多加了 `configs` 一个参数**（卡 §7 验收方式那条本来就带 `configs`）：
   两处命令不一致，按验收方式那条跑。命中清单与逐条处置见 §10.6。

### 10.5 新登记的缺陷

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| D-0901 | §3.2 的 `go test ./executor ./api ./core -race -count=5` 没带 `-timeout`：`api` 一轮 120-135s，五轮 1098s，超过测试二进制默认的 10 分钟，照卡面命令跑必然"失败"在超时上 | **已修**：命令补 `-timeout 30m`，并把照原命令的实测结果写在该行注释里，防止下一个人把超时当回归 |
| D-0902 | 缺陷编号跨系列重号：本系列的 `D-0701`（设计文档 §6.8 的 `editable` 公式漏 `!degraded`）与 SQLite 系列的 `D-0701`（`dropped` 无在线出口）是两件事，`D-0803` 也重了 | **登记不修**：改编号会打断两张卡与设计文档里已有的交叉引用。缓解措施：引用本系列编号时带系列名（本目录 README 与系列记忆文件按此写法）；真正的解法是把 D 编号收到项目级唯一表里，属后续卡 |
| D-0903 | 卡 §3.3 场景 13 的判据不足（见 §10.4 第 1 条）：只说"处于 running 时强杀"，没说是**存储里的** running | **已修**：§3.3 那一行补判据与本卡第一轮的失败现象；产品行为不变（`deployment.md` 第 9 条的口径本来就是存储快照） |
| D-0904 | 任何"门禁在读请求体之前就中止"的写请求（本组端点的两条 503、角色判定那条 403）都可能让客户端看到**连接被重置**而不是 JSON 错误体：本机循环口带体 POST 各 30 次，三轮分别 2、3、0 次重置；而处理器读完体再拒的路径 90 次零重置，服务端两边的访问日志与台账里状态码都在 | **登记不修**：跨系列现象（所有被门禁拒掉的写请求一样，不止档位），一行 drain 请求体的改法落 `api/server.go` 的中间件层，与本卡"不改任何行为"冲突。现象与调用方处置（写请求遇重置就重试）已写进 `docs/api.md` |
| D-0905 | `path_display` 的文档措辞"绝对路径**原样**"与实现不符：给的是解析并归一后的路径，Windows 上写 `C:/a/b.py` 读回反斜杠形式 | **已修**：`docs/api.md` 的字段表与设计文档 §6.8 各改一句，并注明是本卡场景 5 实测 |

本卡**没有**改任何生产代码：`git diff --stat` 里只有文档、YAML 注释与本目录两张卡。
S07/E19 那两条"当场修缺陷要单独成提交"的口径因此不需要启用。

### 10.6 grep 复核（§4.4）与处置

命令：`grep -rn "需要重启进程\|不能新增或编辑\|完全由这里决定" README.md docs web/src configs`

| 命中 | 处置 |
| --- | --- |
| 本目录 `README.md` 的"全卡共同的验证口径"那条（动笔时写着 `job-template.md` "现在写着…W08/W09 要按新事实改"） | 已改：那句现在写明"W08 已按新事实改成…，W09 grep 复核过没有残留"，引号里保留改造前的原文作为对照 |
| `docs/design/tasks/web-profile/task-w08-console-ui.md:94` | 保留：那是 W08 卡片动笔时的现状陈述，属历史执行记录，不改写 |
| `task-w09-docs-and-verification.md:25/27/101/127`（本卡自己的 §2 表与 §4/§7） | 保留：本卡的"现在写的"列就是开工前的快照，改了它这条验收就失去对照物 |
| `docs/design/web-profile-design.md:13-14` | 保留：那是"改造前的现状 + 用户诉求"的由来段落，§2 之后新增的"落地位置"段说的是落地后的事实 |
| `configs/` 两份、根 `README.md`、`web/src`、`docs/api.md`、`docs/deployment.md`、`docs/example.md` | 零命中："完全由这里决定"那句已被 §10.3 的三行替换；W08 改过的 `job-template.md` 与 `SettingsView.vue` 现在只说"改 yaml 那一份要重启" |

另跑一条本卡自己加的 grep：`grep -rn "executors.commands" README.md docs/*.md web/src` 里所有带"重启"的行
（`docs/api.md:515`、`docs/deployment.md:350`、`web/src/content/job-template.md:273`、
`web/src/views/ProfilesView.vue:205/279`）**都只说配置那一份需要重启**，与新事实一致，不残留。

### 10.7 未验证项

| 没跑的项目 | 现状 | 谁能补 |
| --- | --- | --- |
| Linux / macOS 的真机运行 | 本卡只做了三条交叉构建（编译）；§10.2 的十四条场景全在 Windows 真机 | 任一 Linux 冒烟一轮；`relativeTo` 的跨盘符分支与 `killTree` 的 unix 分支同样只有用例 |
| 场景 12 的"换一台机器" | 在同一台机器上重启，`runtime_ok` 由本机重新探测（这正是那句话的机制），但没有真的换机 | 双机部署演练一次 |
| 控制台的浏览器实测 | 本卡零代码改动，界面事实由 W08 §10.4 的十步走查覆盖，本卡没重跑 | 下次改 `web/` 的卡片顺带 |
| 场景 14 的其它并发形态 | 只覆盖"两个 ops 会话同时 PUT 同一条"；删除与新建并发、写与启动合并并发都没有真机现场 | 需要并发压力卡；D-0602（页面写入与手改文件之间无乐观并发）是同一族的另一半 |
| D-0904 的重置率 | 只有 Windows 循环口、Python `urllib` 客户端、单进程 30 次/轮的样本；curl 与浏览器侧没测 | 补 D-0904 的那张卡；换客户端复测就能判它是不是本机环境特有 |
| 台账"搜不到请求体原文"的判据 | 用的是关键字搜索（`args_render`、`"script":`、两个 canary 值），不是结构化字段白名单断言 | 观测层后续卡若要给台账加列，应同时把这条改成结构化断言 |

