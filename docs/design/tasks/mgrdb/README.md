# 管理库与任务存储 SQLite 化任务卡(M01 … M04)

设计说明在 `../../mgrdb-design.md`,本目录把它拆成可逐个执行的卡片。
管理库本体(账号/分组/档位 + 装配 + 认证器切库 + 账号 API)已在 feat/db-migration 分支完成
(随 refactor/go-layout 携带),不设卡;本系列只拆剩余三件:

## 执行顺序

| 阶段 | 任务 | 内容 | 该卡结束时可演示的能力 |
| --- | --- | --- | --- |
| M01 | 设计文档 | `docs/design/mgrdb-design.md` | 方案定稿,含 jobs 迁库的范围回扩说明 |
| M02 | 前端 | 控制台账号管理页 | admin 在控制台增改删账号,无需改配置重启 |
| M03 | 后端 | 任务快照 SQLite 存储 | `store.type: sqlite` 全链可用,一次性导入幂等 |
| M04 | 收口 | 文档同步与终验 | README/deployment/config.example 一致,全量回归过 |

依赖:M02 与 M03 互不依赖,可并行或单独挑做(前端只依赖已完成的账号 API);
M04 需要 M02、M03 全部完成。

## 每张卡片的结构

与 `../sqlite/README.md` 的十段结构一致(任务目标/背景/功能/步骤/测试/DoD/验收/不在范围/风险/实现记录)。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过;涉及前端的卡另跑
  `cd web && npx vue-tsc --noEmit` 与 `npm run build`(产物落 `internal/web/dist`)。
- import 一律 `github.com/jiujuan/godelayq/internal/...`(go-layout 之后的口径)。
- 改 `core/config.go` 的配置键,同步 `configs/config.example.yaml` 与本机 `configs/config.yaml`,
  `go test -run TestExampleConfigMatchesLocal ./internal/core` 把关。
- 新 store 包的依赖红线:`internal/store/jobs` 不 import SQLite 驱动(经底座)、守卫测试随包交付。
- 提交信息 Conventional Commits,中文;每卡一个提交(跨面大可拆 2–3 个),提交必须自身全绿。
- 如需新增代码抽象:优先 struct;确有复用才小 interface + struct,接口定义放消费方。
