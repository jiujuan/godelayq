// Package jobs 是任务快照的 SQLite 存储（TASK-M03）：jobs 一张主表整存
// JobSnapshot，jobs_meta 承载一次性导入标记。机制（打开连接、PRAGMA、迁移框架）
// 来自 store/sqlite 公共底座，与 store/observe、store/mgr 同一取向。
//
// 依赖边界（与两者同口径）：
//   - 本包经 store/sqlite 间接使用 SQLite 驱动，自己不 import 驱动；
//   - 实现的接口 core.Store 定义在 core（消费方声明能力），这里只做隐式实现；
//   - 终态留痕的淘汰语义只有一份，在 core.TrimTerminal——JSON 存储与本包共用，
//     任何一侧改规则都必须是改那个函数。
//
// 与 JSON 实现的两处行为差异（都是直写的必然结果，装配侧不感知）：
//   - Flush 恒为 no-op：每次变更已在一个事务里落盘，没有"未落盘的内存态"；
//   - 写失败当场返回错误：JSON 的后台合并失败还能等下一个周期重试，直写没有后台。
//
// 本包只在 store.type=sqlite 时被装配（cmd/server）。首次打开时若库空且
// store.path 指向的 JSON 文件存在，做一次整文件导入并打标记（ImportOnce），
// 之后 JSON 文件不再被读取——与 mgr 库的一次性导入同一手法。
package jobs
