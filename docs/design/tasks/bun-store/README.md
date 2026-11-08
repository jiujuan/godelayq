# 存储层 bun 化与 PostgreSQL 就绪任务卡(B01 … B07)

设计说明在 `../../bun-store-design.md`,本目录把它拆成可逐个执行的卡片。

## 执行顺序

| 阶段 | 任务 | 内容 | 该卡结束时可演示的能力 |
| --- | --- | --- | --- |
| B00 | 设计文档 | `docs/design/bun-store-design.md` | 方案定稿 |
| B01 | 底座 | `store/sqlite`→`store/engine` + bun.DB + Migrate 移植 | 三库仍跑 SQLite,全部测试绿,行为零变化 |
| B02 | 后端 | jobs 包 bun 化 | 任务快照存取全走查询构建器 |
| B03 | 后端 | mgr 包 bun 化 | 账号/分组/档位与一次性导入走 lower(name) 统一查询 |
| B04 | 后端 | observe 包 bun 化 | 批量写/审计查询/工件索引走构建器 |
| B05 | 方言 | PG 打开路径 + 三份 PG DDL + 配置面 | 本机 PG 起三库冒烟通过 |
| B06 | 测试 | 方言矩阵测试 + 本机 PG 集成 + 双平台脚本 | `GODELAYQ_TEST_PG_DSN` 一给即跑,不给干净跳过 |
| B07 | 收口 | 文档同步 | architecture/README 与实现一致 |

依赖:严格串行 B01→B02→B03→B04→B05→B06→B07(每张卡都把前一张的产物当作既定事实)。

## 每张卡片的结构

与 `../mgrdb/README.md` 的十段结构一致(任务目标/背景/功能/步骤/测试/DoD/验收/不在范围/风险/实现记录)。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过。
- import 一律 `github.com/jiujuan/godelayq/internal/...`。
- 改 `core/config.go` 配置键的卡同步 `configs/config.example.yaml` 与本机 `configs/config.yaml`,
  `go test -run TestExampleConfigMatchesLocal ./internal/core` 把关。
- 依赖红线:只有 `internal/store/engine` import 数据库驱动;`store/{jobs,mgr,observe}` 可以
  import bun 但不得 import 任何驱动;守卫测试随卡更新。
- 提交信息 Conventional Commits,中文;每卡一个提交,提交自身全绿。
- PG 凭据只经 `GODELAYQ_TEST_PG_DSN` 环境变量传递,任何文件不得落明文密码。
