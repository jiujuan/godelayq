package jobs

import "database/sql"

// sqloutlet.go 是任务库惟一的原生语句出口（TASK-RE08 登记进 package_guard_test
// 的豁免白名单）：与 engine.DB 的 SQL() 同形透传。运维排查与跨库集成测试偶发
// 需要原生语句（如把 dispatched 出箱行拨回 pending 演练崩溃重放）；业务代码
// 不得使用——豁免只认这个文件，往 db.go / attempts.go 等回加 .SQL() 调用会被
// 守卫当场拦下。
func (s *Store) SQL() *sql.DB { return s.h.SQL() }
