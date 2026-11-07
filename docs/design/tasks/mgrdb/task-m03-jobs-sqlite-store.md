# TASK-M03　任务快照 SQLite 存储(store.type: sqlite)

- 所属阶段:独立卡
- 依赖任务:无(M02 可并行)
- 涉及文件:`internal/core/store.go`(抽 TrimTerminal)、`internal/core/config.go`、
  `internal/core/config_reload.go`、`internal/store/jobs/`(新包)、`cmd/server/main.go`、
  `configs/config.example.yaml` + 本机 config.yaml
- 预计规模:大,约 1–1.5 人日

## 1. 任务目标

`store.type: sqlite` 时任务快照落 `data/goqsys_jobs.sqlite`,调度器/API/目录加载器无感
(`core.Store` 接口不变);json 缺省行为零变化。

## 2. 背景与当前问题

任务快照每 200ms 全量重写 JSON,写放大随任务量线性上涨;`StoreConfig.Type` 字段与
`store.type` 的 ClassRestart 分类在初版即预留,本卡兑现它。

## 3. 要实现的功能

1. `core.TrimTerminal` 共享纯函数(留痕语义唯一来源),JSON 实现改为调用它,行为不变。
2. `core.StoreSQLiteConfig{Path,BusyTimeout,Synchronous}` + `StoreConfig.SQLite` 字段 +
   `DefaultJobsDBPath/BusyTimeout/Synchronous` 三常量 + Validate;
   热重载表补 `store.sqlite.*` 三键 ClassRestart。
3. `internal/store/jobs` 新包:`Open` = sqlite.OpenFile + Migrate("jobs_schema_migrations");
   实现 `core.Store` 全部方法(直写、每变更一事务)与 SetHistoryRetention;
   jobs/jobs_meta 两表 + 两索引(见设计文档 §6);一次性导入(三条件 + 单事务 + 标记);
   Flush 为 no-op、Close 幂等;写失败当场返回错误。
4. cmd/server 装配按 `cfg.Store.Type` 分岔,未知值启动报错。
5. config 两份 YAML 同步新键。

## 4. 实现步骤

1. 先做 core 侧(TrimTerminal 抽取 + config),`go test ./internal/core/...` 全绿——
   既有 store_history_test/store_hot_test 是行为不漂移的证据。
2. 新包 jobs:schema → db(六方法)→ import → 测试(往返/留痕对照/导入三条件/守卫)。
3. cmd/server 装配分岔 + YAML 同步。
4. 手工链路:`store.type: sqlite` 起服务 → 提交任务 → 重启恢复 → API 历史页可见终态。

## 5. 测试要求

- 单元:Store 六方法往返;留痕(limit/ttl/limit<0/平局按 ID)与 JSON 实现对照
  (同一操作序列 → 相同可见快照集合);导入三条件逐条;Close 幂等;
  守卫:jobs 包不 import 驱动(照 TestSQLitePackageImportsNothingInRepo)。
- 集成:cmd/server 现有测试在 type 缺省(json)下零变化。

## 6. 完成标准(DoD)

- [ ] 设计文档 §14 验收清单前四条全过
- [ ] 新旧两实现对照测试通过;`TestExampleConfigMatchesLocal` 通过
- [ ] 全量 build/vet/test 绿

## 7. 验收方式

```bash
go test ./internal/core/... ./internal/store/jobs/... ./cmd/server/...
# 手工:config.yaml 改 store.type: sqlite 起服务,跑提交→重启→恢复链路后改回
```

## 8. 不在本任务范围

- SQL 查询下推、状态过滤查询(接口不变);
- 批量写入器(直写够用,压测说话);
- JSON→SQLite 反向导出工具(回滚说明进 M04 文档)。

## 9. 风险与回滚

- 风险:trim 语义漂移——由"JSON 实现改为调用共享函数 + 既有测试钉死"消解;
- 风险:导入把过期任务带活——导入源就是当前 store.path,语义等价于 json 后端重启;
- 回滚:`store.type` 改回 json 重启;库文件删除即净(导入幂等,重开重导)。

## 10. 实现记录

- `core.TrimTerminal` 抽取按设计落地：JSON 的 `trimTerminalLocked` 改为薄包装（先 Load 两个
  原子值一起传入——D-R0201 的窗口注释原样保留），SQLite 侧用只填 ID/Status/UpdatedAt 的
  部分快照构造入参（该函数只读这三个字段，注释已写明）。
- **行为语义的教训**："limit<0 不留痕"是字面义的**写入即删除**——终态写入触发的 trim 连刚写入
  的那条一起删。测试首版断言"t2 留下"错了，修正为断言全空；JSON 实现历来如此（StoreOptions
  注释"负数表示不保留终态记录（写入即删除）"），共享函数没有引入新语义。
- 实现偏差（相对设计文档 §10）：`historyRetentionValues` 在 jobs 包有一份复制（core 的同名
  函数私有，跨包可见性的代价），两边由对照测试钉住；`ImportOnce` 在 Store 上加了一个方法
  包装（`s.ImportOnce(path)`），装配侧不必碰内部句柄。
- 默认值解析走 `Config.Normalized()`（SQLite.Path/BusyTimeout 空→DefaultJobs*，与 MgrStore
  同机位），jobs.Open 不再兜底空值——sqlite.OpenFile 本就拒绝空路径。
- 验证：往返/留痕四子态/导入三条件/Close 幂等/与 JSON 可见集合对照/包守卫全过；
  `TestExampleConfigMatchesLocal` 过（两份 YAML 已同步 store.sqlite 节）；全量 8 包全绿。
