# TASK-L03　文档同步 + 叙事改写 + 敏感审查 + 终验

- 所属阶段:收口卡
- 依赖任务:L01、L02
- 涉及文件:`README.md`、`docs/api.md`、`docs/deployment.md`、`docs/example.md`、`docs/core-scheduler-heap-event-load-analysis.md`
- 预计规模:中,约 0.5–1 人日

## 1. 任务目标

全部使用者文档与新目录结构、新 module 名一致;README 的产品叙事与"core 已 internal"的定位一致;终验全绿。

## 2. 背景与当前问题

README 约 48KB,含 147–277 行的目录结构图与多处 `godelayq/core` import 示例,现已全部过期;更根本的是,core 进 internal 后"嵌入用法"不再是外部能力,README 若继续以嵌入为第一叙事会误导开源访客。

## 3. 要实现的功能

1. README 目录结构图整节重写(目标树见本目录 `README.md`)。
2. 全文路径替换:`godelayq/core|api|executor|store|web` → `github.com/jiujuan/godelayq/internal/...`(仅文档,注意放过 `docs/design/` 历史归档)。
3. 叙事改写:快速开始以"部署 server + REST API"为主线;嵌入用法降级为"仓内开发参考",明确注明 import 走 `internal/`、外部集成走 REST API(未来 `pkg/client` SDK 另立卡)。
4. demo2 运行方式更新:示例任务目录已随包迁至 `examples/demo2/job_queue`,在该目录下运行。
5. `docs/*.md` 路径核对与修正。
6. 开源前敏感信息审查:docs 有无内网信息、`configs/config.example.yaml` 复核、deployment 文档脱敏检查。
7. 终验(见 §7)。

## 4. 实现步骤

1. 先对四份使用者文档跑路径 sed,人工过目录图与 import 示例块。
2. 改写 README 快速开始与适用场景的叙事段。
3. 敏感信息审查逐文件过(仅读不改历史归档)。
4. 终验清单全跑。

## 5. 测试要求

- 文档层面:README/`docs/*.md` 中 `grep -n '"godelayq/'` 零命中;`grep -n 'module godelayq'` 零命中。
- 代码层面:`go build ./... && go vet ./... && go test ./...` 终验全绿。
- 构建层面:`-tags dashboard` 构建产物可起服务(L02 口径复跑一次作为终验)。

## 6. 完成标准(DoD)

- [ ] README 目录图与磁盘实际结构一致
- [ ] 四份使用者文档零旧路径
- [ ] `docs/design/` 六份历史文档未改动(`git diff --name-only` 验证)
- [ ] 敏感审查有结论记录(实现记录里写明查了什么、结论如何)
- [ ] 全量测试 + dashboard 冒烟通过

## 7. 验收方式

```bash
grep -rn '"godelayq/' README.md docs/*.md | wc -l        # 预期 0
git diff --name-only main -- docs/design/ | wc -l        # 预期 0(历史归档未动)
go test ./...
```

## 8. 不在本任务范围

- LICENSE、CI workflow、goreleaser、打 tag → 独立开源配套卡
- 历史设计文档的内容修订 → 永不(归档口径)
- `pkg/client` SDK → 独立功能卡

## 9. 风险与回滚

- 风险:sed 误伤文档正文(如提到"模块 godelayq"的散文)——只替换带包路径形态的串,人工过 diff。
- 风险:敏感信息审查流于形式——实现记录必须列出检查过的文件清单与结论。
- 回滚:文档提交独立,revert 即可。

## 10. 实现记录

(执行时补写)
