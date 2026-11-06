// Package observe 是 godelayq 观测层的 SQLite 存储：运行事件（job_events）、
// 产物索引（artifact_index）、写操作审计（write_audit）三张表。
// 机制（打开连接、PRAGMA、迁移框架）来自 store/sqlite 公共底座，本包只带观测域的
// 表结构、批量写入器与读数。
//
// 依赖边界（设计依据见 docs/design/sqlite-observability-design.md §5，本仓库的红线）：
//   - 本包经 store/sqlite 间接使用 SQLite 驱动，自己不 import 驱动；
//     全仓唯一引入驱动的地方仍是 store/sqlite。
//   - 接口定义在消费方：api 侧的事件读写与审计写入、executor 侧的产物索引各自声明窄接口，
//     本包只提供隐式实现。本包不导出任何"供 api 使用"的接口类型。
//   - 依赖 core 是为了读 core.ObservabilityConfig 与 core.Event 的形状；
//     依赖 api/executor 是 audit.go/artifacts.go 的既有依赖（历史反向依赖，
//     治理另立卡，不在搬包这一步扩大改动）。
//
// 本包只在 observability.enabled=true 时被装配（cmd/server），关闭时一个文件都不会创建。
package observe
