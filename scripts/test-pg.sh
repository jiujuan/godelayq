#!/usr/bin/env bash
# PostgreSQL 集成测试一键入口（与 test-pg.ps1 同构成对，bash 4+，Git Bash 可用）。
#
# 用法：
#   export GODELAYQ_TEST_PG_DSN='postgres://user:pass@localhost:5432/postgres?sslmode=disable'
#   bash scripts/test-pg.sh
#
# DSN 指向任意有建库权限的连接（通常维护库 postgres）。测试库由 pgtest 自动
# CREATE/DROP（godelayq_test_<用途>_<随机>），跑完不留垃圾；口令只经环境变量
# 传递，任何文件不得落明文。
set -euo pipefail

if [ -z "${GODELAYQ_TEST_PG_DSN:-}" ]; then
  echo "未配置 GODELAYQ_TEST_PG_DSN（形如 postgres://user:pass@localhost:5432/postgres?sslmode=disable）。" >&2
  echo "配置后重跑本脚本；不配置直接跑 go test 时，PG 用例会干净跳过、不失败。" >&2
  exit 2
fi

echo "== PostgreSQL 集成测试矩阵（jobs/mgr/observe/server，库自建自拆）=="
exec go test -count=1 ./internal/store/... ./cmd/server/
