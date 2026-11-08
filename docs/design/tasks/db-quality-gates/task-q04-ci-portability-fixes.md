# Q04 CI 首跑暴露的可移植性修复

## 1. 任务目标

让 Q01 的 CI 全绿：修复首跑暴露的 1 处生产路径规范化缺陷与 5 处测试可移植性问题
（全部与本分支的 store 改动无关——main 上同样会失败，CI 首跑把它们抓了出来）。

## 2. 背景与当前问题

CI run 42475983370：postgres-matrix ✓（store 层双方言矩阵在真实 PG 17 上通过）；
两个 sqlite-fast 死在 executor/api 的既有测试。三类根因：

1. **Windows 8.3 短名**：GH 跑步机的 TEMP 是 `C:\Users\RUNNER~1\...`。生产侧
   `resolveWorkspace`/档位路径都经 `filepath.EvalSymlinks` 规范成长名，但
   `checkAdhocScript` 对**提交值**的解析（resolveAnywhere）不做这步——
   短名绝对路径在 PathPrefixes 前缀判定（长名空间）里被误判越界。**生产缺陷**，
   本机用户名短所以从未显形。测试侧期望值（locationOf/Join 拼的字符串）也与生产
   不同空间。
2. **GOOS 偏置**：`build_profile_modes_test` 硬编码 `D:\srv\...` 当绝对路径，
   Linux 上它只是个怪文件名。
3. **时序赌注**：`TestConcurrent_ExecuteWhilePurging` 注释说 victim"已过期"，
   实际 TTL=1ms、刚建的目录要等 1ms 墙钟才真过期——CI 快机器上 30 轮清理跑完
   victim 还没满月，零删除。

## 3. 要实现的功能

- 生产：`checkAdhocScript` 在 resolveAnywhere 之后、prefix/扩展名判定之前，
  存在即 `evalSymlinks`（与 resolveInside 同一取向，缺失交给文件判据）。
- 测试：`locationOf` 升级为与生产同一规范化（canonicalPath）；Render/Positional
  的 argv 期望、api `newAdhocEnv` 的 workspace 同空间化；绝对路径用例 GOOS 化；
  victim 目录 mtime 拨回一小时。

## 4. 实现步骤

1. adhoc.go 生产修复；2. executor 测试助手与三处期望；3. api env 规范化；
4. build_profile GOOS 化；5. artifact_index 轮询消除；6. 本地全量 → push → CI 复跑。

## 5. 测试要求

本地（Windows）全量绿；CI 两 job 绿（ubuntu 由 CI 验证，本机无法跑 Linux 二进制）。

## 6. 完成标准(DoD)

CI run 全绿；生产缺陷有回归证据（短名提交在 PathPrefixes 下被正确接受——由 CI 的
windows 快档证明）。

## 7. 验收方式

```
go build ./... && go vet ./... && go test -count=1 ./...
git push && gh run watch   # 两个 job 全绿
```

## 8. 不在本任务范围

别的 flaky 治理、-race 恢复（另有环境问题）、P1/P2 门禁。

## 9. 风险与回滚

生产修复只收紧"存在即规范化"这一既有约定，行为面是"短名提交不再被误拒"；
若 CI 仍有别的环境暴露点，按同一手法续修。回退还原本卡提交。

## 10. 实现记录

- 2026-11-08 完成（CI run 42475983370：postgres-matrix ✓ / 两个 sqlite-fast ✗）。
- **生产修复 1 处**：adhoc.go checkAdhocScript——提交路径存在即 evalSymlinks，与
  resolveWorkspace 的长名空间对齐；修掉 Windows 8.3 短名环境下"PathPrefixes 误判
  越界"（workspace 长名 vs 提交短名，withinDirectory 前缀必然失配）。
- **测试修复 5 处**：locationOf 升级为与生产同一规范化（含存在豁免）；
  TestRender_NonAdhocUnchanged / TestValidateSubmission_Positional 的 argv 期望
  过 locationOf；api newAdhocEnv 的 workspace 规范化；build_profile 绝对路径用例
  GOOS 化（unix 用 /srv/...，windows 才有盘符写法）；并发清理 victim 目录
  os.Chtimes 拨回一小时（"已过期"从对机器速度的赌注变成事实）。
- 本机（Windows）全量绿；并发与 GOOS 用例 -count=5 稳定。ubuntu 行为由 CI 复跑验证。
- **第二轮 CI（42477163459）**：ubuntu ✓ / postgres-matrix ✓；windows 暴露残部——
  locationOf 只对"存在的路径"生效，bin/etl 这类不存在的档位路径仍走 Abs 兜底短名。
  本地 8.3 探针实证：EvalSymlinks 展开短名但对缺失路径报错、Rel(长,短) 产生畸形爬升。
- **系统性收口**：executor 测试包新增 tempWorkspace(t)（TempDir + EvalSymlinks，
  目录必存在故必能展开），137 处 t.TempDir() 全量替换——路径在源头进长名空间，
  成员派生与断言自然对齐，不再逐断言打补丁。
- **第三轮 CI（42477867138）**：executor/api 清零，windows 只剩 SQL 快照——
  正是设计期预判的 golden CRLF 问题（autocrlf 检出转 CRLF vs 运行时 LF）。
   新增 .gitattributes 钉 *.golden text eol=lf 收口。
- **第四轮 CI（42478145828）：三 job 全绿**。门禁闭环：CI 此后每次 push/PR
  自动跑 SQLite 快档（双 OS）+ PG 全矩阵。
