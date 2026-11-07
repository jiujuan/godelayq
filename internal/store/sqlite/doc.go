// Package sqlite 是 godelayq 的 SQLite 公共访问底座：打开库文件、连接参数（PRAGMA）、
// 版本化迁移框架。它只做机制，不认识任何一张业务表——观测库（store/observe）、
// 管理库（store/mgr）等各业务包在这里拿句柄，然后建自己的表、读写自己的行。
//
// 依赖边界（设计依据见 docs/design/sqlite-observability-design.md §5，本仓库的红线）：
//   - 本包是全仓唯一 import SQLite 驱动（modernc.org/sqlite，纯 Go 无 CGO）的包。
//     core、api、executor 与 examples 都不许引入驱动，否则零重型依赖的调度库
//     和两个可以直接 go run 的示例会被动继承一个大体积依赖。
//   - 本包对仓内业务包零依赖：不 import core/api/executor，选项与迁移都由调用方传入
//     （Options、Migration）。默认值（如 DefaultSynchronous）在这里自持一份同值常量，
//     配置层的默认值来源仍在 core，两处取值必须保持一致。
//   - 接口定义在消费方：api 侧的事件读写与审计写入、executor 侧的产物索引各自声明窄接口，
//     各业务包只提供隐式实现。本包不导出任何"供 api 使用"的接口类型。
//   - core 不依赖本包。
//
// 迁移规则：每个业务库用自己的迁移表名（observe_schema_migrations、mgr_schema_migrations…）
// 调 Migrate，多库共存时迁移记录不能撞车；表结构一旦落地就是契约，改动只能以新增
// 迁移版本号的方式表达。
package sqlite
