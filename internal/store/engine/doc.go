// Package engine 是 godelayq 的数据库公共访问底座：按方言打开连接（bun.DB）、
// 连接级设置（SQLite PRAGMA / PG DSN）、版本化迁移框架。它只做机制，不认识任何
// 一张业务表——观测库（store/observe）、管理库（store/mgr）、任务库（store/jobs）
// 在这里拿句柄，然后建自己的表、用 bun 查询构建器读写自己的行。
//
// 依赖边界（本仓库的红线，由本包与各业务包的 package_guard_test 钉住）：
//   - 本包是全仓唯一 import 数据库驱动（modernc.org/sqlite，纯 Go 无 CGO；
//     B05 起另有 pgx）与 bun 的"底座"包。core、api、executor 与 examples 都不许
//     引入驱动，否则零重型依赖的调度库和两个可以直接 go run 的示例会被动继承
//     一个大体积依赖。
//   - 本包对仓内业务包零依赖：不 import core/api/executor，选项与迁移都由调用方
//     传入（Options、Migration）。默认值（如 DefaultSynchronous）在这里自持一份
//     同值常量，配置层的默认值来源仍在 core，两处取值必须保持一致。
//   - 接口定义在消费方：api 侧的事件读写与审计写入、executor 侧的产物索引各自
//     声明窄接口，各业务包只提供隐式实现。本包不导出任何"供 api 使用"的接口类型。
//   - core 不依赖本包。
//
// 方言规则：SQLite 是既事后端，行为与 store/sqlite 时代逐字一致；PostgreSQL 自
// B05 起可用。方言分歧只允许出现在两处——迁移 DDL（Migration.UpPostgres）与打开
// 路径（Options.DSN）；查询层经 bun 构建器天然双方言，业务代码不得按方言分叉。
//
// 迁移规则：每个业务库用自己的迁移表名（observe_schema_migrations、
// mgr_schema_migrations…）调 Migrate，多库共存时迁移记录不能撞车；表结构一旦落地
// 就是契约，改动只能以新增迁移版本号的方式表达。
package engine
