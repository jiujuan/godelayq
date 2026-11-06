// Package mgr 是 godelayq 管理库的 SQLite 存储：控制台账号（mgr_users）、
// 分组元数据（mgr_groups）、执行器档位（mgr_executor_profiles）三张表，
// 外加 mgr_meta 承载一次性导入标记。机制（打开连接、PRAGMA、迁移框架）来自
// store/sqlite 公共底座，与 store/observe 同一取向。
//
// 依赖边界（与 store/observe/doc.go 同一口径）：
//   - 本包经 store/sqlite 间接使用 SQLite 驱动，自己不 import 驱动；
//   - 实现的接口定义在消费方：core.UserStore / core.GroupStore /
//     core.ExecutorProfileStore——core 也是共享词汇（UserRecord、Group、
//     ExecutorProfileRecord）的出处，本包与 api 各自依赖 core，互不相认；
//   - 密码哈希的 cost 校验在本包导入时做一次（bcrypt.Cost），那是本包唯一
//     依赖 golang.org/x/crypto 的位置。
//
// 本包只在 mgrstore.enabled=true 时被装配（cmd/server）。打开后三类数据以库为准：
// 首次启动做一次性导入（config 里的 users、两个 JSON 文件，库非空即跳过），
// 之后配置与 JSON 文件不再被读取。
package mgr
