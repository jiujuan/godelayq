// Package observe 是 godelayq 观测层的 SQLite 存储：运行事件（job_events）、
// 产物索引（artifact_index）、写操作审计（write_audit）三张表。
// 机制（打开连接、PRAGMA、迁移框架）来自 store/sqlite 公共底座，本包只带观测域的
// 表结构、批量写入器与读数。
//
// 依赖边界（设计依据见 docs/design/sqlite-observability-design.md §5，本仓库的红线）：
//   - 本包经 store/sqlite 间接使用 SQLite 驱动，自己不 import 驱动；
//     全仓唯一引入驱动的地方仍是 store/sqlite。
//   - 接口定义在消费方：api 侧的事件读写与审计写入、executor 侧的产物索引各自声明窄接口，
//     本包只提供隐式实现，不 import api/executor——是否满足由装配方（cmd/server）
//     在编译期把关。共享词汇（core.AuditEntry、core.ArtifactIndexRecord）放 core，
//     本包与消费方各自依赖 core，互不相认。守卫见 package_guard_test.go。
//   - 依赖 core 是为了读 core.ObservabilityConfig 与共享类型（Event、AuditEntry、
//     ArtifactIndexRecord、ArtifactInfo）的形状。
//   - 测试文件例外：artifacts_test.go import executor 构造真实的 ArtifactStore
//     做索引集成验证；测试期依赖不进生产依赖图。
//
// 本包只在 observability.enabled=true 时被装配（cmd/server），关闭时一个文件都不会创建。
package observe
