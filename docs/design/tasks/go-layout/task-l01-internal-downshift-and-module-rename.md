# TASK-L01　五包下沉 `internal/` + module 改名 + import 重写

- 所属阶段:唯一的前置卡,L02/L03 均依赖本卡
- 依赖任务:feat/db-migration 合入(实际上经决策在本分支上先行,见实现记录 §1)
- 涉及文件:`go.mod`、`core/` `api/` `executor/` `store/` `web/embed_*.go`(移动)、全部引用内部 import 的 `.go`、`core/config_test.go`(两行路径)
- 预计规模:小(机械活),约 0.5 人日

## 1. 任务目标

仓库源码树变为标准布局:实现包全部在 `internal/` 下,module 名为 `github.com/jiujuan/godelayq`,全量测试全绿。

## 2. 背景与当前问题

`core/`、`api/`、`executor/`、`store/` 平铺在仓库根,与前端(`web/`)、运行时数据(`data/`)、开发工具(`scripts/`)混在一层;module 名是单段 `godelayq`,开源后既不能被引用也不能上 pkg.go.dev。

## 3. 要实现的功能

1. `go.mod`:`module godelayq` → `module github.com/jiujuan/godelayq`。
2. `git mv` 五处:`core`→`internal/core`、`api`→`internal/api`、`executor`→`internal/executor`、`store`→`internal/store`、`web/embed_dashboard.go`+`web/embed_stub.go`→`internal/web/`。
3. import 重写,一条规则全覆盖(所有内部 import 都走 `internal/`,无例外清单):
   `s|"godelayq/|"github.com/jiujuan/godelayq/internal/|g`
   该规则顺带覆盖守卫测试里的 `"godelayq/api"` 等字符串字面量与子包(`store/mgr` 等)。
4. `core/config_test.go` 的 `../configs/` → `../../configs/`(全仓唯一的目录深度依赖)。

## 4. 实现步骤

1. `mkdir internal` 后按上表逐条 `git mv`(移动与重写**同一个提交**,原因见实现记录 §2)。
2. 改 `go.mod`,跑 import 重写 sed,对改动文件 `gofmt -w`。
3. 修 `core/config_test.go:424-425` 两行常量。
4. 验证口径全跑一遍(见 §7)。

## 5. 测试要求

- 单元/集成:`go build ./... && go vet ./... && go test ./...` 全绿(测试文件随包移动,不改内容)。
- 守卫测试(`store/observe/package_guard_test.go`、`store/sqlite` 同款)原样通过——它们的 import 字面量已被 sed 覆盖。

## 6. 完成标准(DoD)

- [ ] `go build ./... && go vet ./... && go test ./...` 全绿
- [ ] `grep -r '"godelayq/' --include='*.go' .` 零残留
- [ ] `git log --follow internal/core/scheduler.go` 历史连续到重构前
- [ ] 移动的文件在 `git status` 中呈现为 rename

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./...
grep -r '"godelayq/' --include='*.go' . | wc -l   # 预期 0
git log --follow --oneline internal/core/scheduler.go | tail -3
```

## 8. 不在本任务范围

- web 构建链(vite outDir、.gitignore、embed 注释)→ L02
- job_queue 归位、dashboard 占位页删除 → L02
- README/docs 的路径与目录图 → L03(文档里的旧路径在本卡之后短暂过期,是预期状态)
- core 拆包、api 按域重组、pkg/client SDK → 均不在本系列

## 9. 风险与回滚

- 风险:目录加深导致相对路径失效——执行前已核查,全仓唯一真实依赖是 `config_test.go` 两行;运行时路径(`configs/`、`data/`、`exec-workspace/`)全是 cwd 相对,与源码树无关。
- 风险:rename 检测失效导致 blame 断裂——移动与重写同提交时相似度远高于阈值(已论证),DoD 有 `--follow` 验证。
- 回滚:整分支 `git revert` 或弃分支即可;无数据、无配置、无接口变化。

## 10. 实现记录

- **分支策略偏离**:Stage 0(先合 feat/db-migration)经决策跳过,本分支直接基于 feat/db-migration HEAD(799edcb)创建。剩余迁库卡(docs/前端/jobs)后续 rebase 时要注意:store/ 与全部 import 路径已改名。
- **与惯例的偏离**:移动与 import 重写合并为单提交(6df7771)——"纯移动单独成提交"会产生编译必断的中间提交,与"每卡提交全绿"冲突。事后验证 rename 检测全部成功(相似度 93–100%),`git log --follow` 连续。
- **执行口径**:Git Bash 下 `xargs` 会吃掉反斜杠路径,中转文件又带 `\r`,两次失败后改用 `grep -rlZ … | xargs -0`(null 分隔)一次通过;共重写 146 处 import + config_test.go 两行路径(全仓唯一深度依赖,已核查)。
- **教训**:两次 `git add -u` 把开工前已存在的 data/jobs.json 运行时改动卷进提交,均用 `git restore --source=HEAD~1 --staged <file>` + `--amend` 摘除;此后提交一律显式列路径。
- 全绿证据:`go build/vet/test ./...` 通过(cmd/server 6.4s、api 31.3s、core 20.5s、executor 24.4s、store/* 均 ok),旧路径 `grep '"godelayq/'` 零残留。
