// Package sqlite 是 godelayq 观测层的 SQLite 存储：运行事件、产物索引、写操作审计三张表。
//
// 依赖边界（设计依据见 docs/design/sqlite-observability-design.md §5，本仓库的红线）：
//   - 本包是全仓唯一 import SQLite 驱动（modernc.org/sqlite，纯 Go 无 CGO）的包。
//     core、api、executor 与 examples 都不许引入驱动，否则零重型依赖的调度库
//     和两个可以直接 go run 的示例会被动继承一个大体积依赖。
//   - 接口定义在消费方：api 侧的事件读写与审计写入、executor 侧的产物索引各自声明窄接口，
//     本包只提供隐式实现。本包不导出任何"供 api 使用"的接口类型。
//   - core 不依赖本包，本包依赖 core 只是为了读 core.ObservabilityConfig 与 core.Event 的形状。
//
// 本包只在 observability.enabled=true 时被装配（cmd/server），关闭时一个文件都不会创建。
package sqlite
