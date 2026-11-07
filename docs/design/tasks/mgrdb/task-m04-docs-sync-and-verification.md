# TASK-M04　文档同步与终验

- 所属阶段:收口卡
- 依赖任务:M02、M03
- 涉及文件:`README.md`、`docs/deployment.md`、`docs/api.md`、`docs/example.md`(如涉及)
- 预计规模:小,约 0.25–0.5 人日

## 1. 任务目标

使用者文档反映三件新事实:账号可在线管理、`store.type: sqlite` 可用、设置页文案已更新。

## 2. 背景与当前问题

README/deployment 仍按"账号改配置重启、任务快照恒为 JSON"叙述;config.example.yaml 的
`store.type` 注释写"目前仅支持 json"。

## 3. 要实现的功能

1. deployment.md:任务存储一节补 sqlite 后端(键说明、导入语义、回滚说明、`flush_interval` 不生效);
   账号管理一节从"改配置重启"改为"控制台在线管理 + API",保留 API 说明。
2. README:目录树补 `internal/store/jobs` 与账号页提法;核心特性相应一句。
3. config.example.yaml:`store.type` 注释更新、`store.sqlite` 节入库模板。
4. 终验:设计文档 §14 验收清单全勾,M02/M03 的 DoD 复核。

## 4. 实现步骤

1. 文档逐份过(只改事实,不重写)。
2. 全量回归 + dashboard 冒烟(与 go-layout L03 同口径)。

## 5. 测试要求

- `go build/vet/test ./...` 全绿;vue-tsc/npm run build 绿。
- 文档 grep:不再出现"目前仅支持 json""改动需重启进程"(账号语境)。

## 6. 完成标准(DoD)

- [ ] 三份使用者文档 + 模板一致;全量回归绿;验收清单全勾
- [ ] 本卡与 M02/M03 实现记录补写完毕

## 7. 验收方式

```bash
go test ./... && grep -rn "目前仅支持 json" README.md docs/ configs/ | wc -l   # 预期 0
```

## 8. 不在本任务范围

- docs/design/ 历史文档修订(归档口径);mgrdb-design.md 自身(实现差异记在卡里)。

## 9. 风险与回滚

- 纯文档 + 验证,无回滚面。

## 10. 实现记录

- deployment.md 三处：store 配置骨架补 sqlite 节与"flush_interval 仅 json 生效"注；
  任务存储段新增 SQLite 后端说明（直写、一次性导入、回滚注意 jobs.json 是旧快照）；
  账号段从"增删需重启"改为"管理库启用后在线管理（控制台账号页 / /admin/users），即时生效"，
  配置文件路径保留为未启用管理库时的口径。
- README 目录树补 internal/store/jobs 行、views 清单补"账号"并按实际页面数重列（原文"八页"
  与实际不符，顺带修正）；config.example.yaml 的 `type` 注释更新。
- "目前仅支持 json"全仓清零（mgrdb 设计文档与卡片里的引用是引述旧文案，保留）；
  本机 configs/config.yaml 注释同步（不入库，键集合测试要求两份一致）。
- 终验记录见提交：go build/vet/test 全量绿、vue-tsc/build 绿、dashboard 冒烟过。
