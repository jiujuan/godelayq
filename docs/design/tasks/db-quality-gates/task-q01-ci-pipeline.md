# Q01 CI 流水线

## 1. 任务目标

`.github/workflows/ci.yml` 两个 job：SQLite 快档（ubuntu+windows）+ PostgreSQL 全矩阵
（services postgres:17），push/PR 自动触发。

## 2. 背景与当前问题

B06 交付的双方言矩阵只在人肉执行时存在；仓库无 CI。开源配套（开源化方案）本就欠
CI 一账，数据库双方言化让它从"加分项"变成"门禁本体"。

## 3. 要实现的功能

- `sqlite-fast`：matrix {ubuntu-latest, windows-latest}；`go build ./...`、
  `go vet ./...`、`go test -count=1 ./...`；无任何服务依赖。
- `postgres-matrix`：services 起 postgres:17（POSTGRES_USER/PASSWORD 显式、
  pg_isready 健康检查），env 注入 `GODELAYQ_TEST_PG_DSN`，
  `go test -count=1 ./internal/store/... ./cmd/server/`。
- go 版本 `go-version-file: go.mod`；job 15 分钟超时；同 ref 并发取消。
- 与本地 `scripts/test-pg.ps1|sh` 同口径：门禁只有一份。

## 4. 实现步骤

1. 写 ci.yml；2. YAML 语法自检；3. 本地等价命令复跑（两个 job 的命令清单）。

## 5. 测试要求

工作流无法在本机执行：以 YAML 解析通过 + 本地复跑 job 内命令全绿为准；
首跑验证发生在推送/合并后（实现记录注明）。

## 6. 完成标准(DoD)

ci.yml 落库，两 job 命令与本地验证口径逐字一致。

## 7. 验收方式

```
python -c "import yaml,io; yaml.safe_load(io.open('.github/workflows/ci.yml',encoding='utf-8'))"
go build ./... && go vet ./... && go test -count=1 ./...
GODELAYQ_TEST_PG_DSN='...' go test -count=1 ./internal/store/... ./cmd/server/
```

## 8. 不在本任务范围

覆盖率上报、lint（守卫走 parser，不引 linter）、benchmark job、Release 流水线。

## 9. 风险与回滚

setup-go 与 go.mod 的 toolchain 指令不匹配——go-version-file 是官方支持的读法；
windows 快档偶发超时——15 分钟上限按本地实测（全量 <5 分钟）留有余量。回退删除文件。

## 10. 实现记录

(执行时填写)
