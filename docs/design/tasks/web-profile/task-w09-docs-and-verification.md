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
go test ./executor ./api ./core -race -count=5
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
| 6 | 页面建与 yaml 同名 | 409；注册表不动 |
| 7 | 手改文件制造同名（改 yaml 后重启） | config 生效、store 条目 `degraded`，进程能起 |
| 8 | 文件写坏（截断 JSON）后重启 | 启动失败，文案含路径与"删掉该文件"的自救指引 |
| 9 | 删除有未终态任务的档位 | 未终态变 `paused`、running 未被打断；恢复后判失败 |
| 10 | 五种身份（viewer/operator/admin/ops/machine）打三个写端点 | 只有 ops 通过，其余 403 |
| 11 | 台账 | 三个动作各一行；整行搜不到 `env` 取值、脚本参数取值与请求体原文 |
| 12 | 重启留存 | 第 4 条建的档位重启后仍在，且 `available` 按新机器重算 |
| 13 | 崩溃恢复 | `exec.<store 档位>` 任务处于 running 时强杀进程 → 重启后被钉成 `paused`（守 §5.5 W05 那条顺序在真实进程里成立） |
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

- [ ] §3.2 九条命令全部跑绿，输出进实现记录。
- [ ] §3.3 十四条场景每条有记录；做不到的进"未验证项"表并写明谁能补。
- [ ] §3.1 六处文档改完，且第 4 步的 grep 命中逐条有处置结论。
- [ ] 设计文档 §2 有"落地位置"段，§7.1 偏离表与代码现状一致（尤其 `resolveInside` 只在严格模式生效这一条）。
- [ ] §7.2 五条待做项在设计文档里状态明确（未被"顺手做掉"、也没丢），并在本目录 README 复述一遍。
- [ ] 本目录 README 的状态表与"阶段进度"两张表填完。
- [ ] 架构缺陷清单（项目记忆里的编号清单）同步：本系列新登记的编号写进去。

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

（待补：命令与输出 / 十四条场景 / 文档改动清单 / 新登记缺陷 / 未验证项 / 与卡片差异）
