# TASK-M02　控制台账号管理页

- 所属阶段:独立卡(只依赖已完成的账号 API)
- 依赖任务:无(M03 可并行)
- 涉及文件:`web/src/api/types.ts`、`web/src/api/admin.ts`、`web/src/api/keys.ts`、
  `web/src/views/AdminUsersView.vue`(新)、`web/src/router/index.ts`、`web/src/views/SettingsView.vue`
- 预计规模:中,约 0.5 人日

## 1. 任务目标

admin 档用户在控制台完成账号的增、改(密码/角色)、删,不再需要改配置文件 + 重启进程。

## 2. 背景与当前问题

后端 `/api/v1/admin/users` 全套 CRUD 已交付(handlers_users.go,admin 档,503 态、
最后 admin 保护齐备),但控制台没有对应页面;SettingsView 只读展示身份,
文案还写着"账号与角色由服务端配置声明(server.auth.users),改动需重启进程"——已过时。

## 3. 要实现的功能

1. 列表页:列出账号(name/role/created_at/updated_at),当前登录者自己标注"我"。
2. 新建:表单(name/password/role),密码 `type="password"`。
3. 编辑:改密码(留空不改)与角色;改动即时生效(后端写库 + 失效缓存)。
4. 删除:UiConfirm(tone=danger),文案预置"最后一个 admin 不可删除;删除自己会被拒绝"。
5. 503 说明态:账号存储未启用时展示"未启用管理库(mgrstore.enabled)"引导而非错误横幅。
6. 权限:路由 `minimumRole: 'admin'`;菜单项自动出现(Sidebar 由路由派生,无需改动)。

## 4. 实现步骤

1. types.ts + admin.ts + keys.ts 补类型与四个 API 函数(对照 handlers_users.go 的请求/响应体)。
2. AdminUsersView.vue:骨架 = GroupsView 的 CRUD + AuditView 的 503 notEnabled +
   AdminView 的 run()/describeError;vue-query 拉列表,mutation 后 invalidate。
3. 路由注册(lucide `Users` 图标);守卫与菜单自动生效。
4. SettingsView.vue 94–97 行文案改写 + 账号页入口(照 107–114 行"去档位管理"的 RouterLink 模式)。

## 5. 测试要求

- `npx vue-tsc --noEmit`、`npm run build` 通过。
- 手工:admin 登录可见可操作;operator 登录菜单无此项、直敲 URL 被守卫踢回;
  mgrstore.enabled=false 时显示 503 说明态;删除最后一个 admin 收到后端拒绝且 toast 可读。

## 6. 完成标准(DoD)

- [ ] 四个 API 函数与后端契约字段一一对应(哈希不出现在任何前端类型里)
- [ ] 503 态、最后 admin 保护、自删拒绝均有界面反馈
- [ ] vue-tsc / npm run build / go test ./... 全绿

## 7. 验收方式

```bash
cd web && npx vue-tsc --noEmit && npm run build && cd ..
go test ./internal/api/...
# 手工:起服务(mgrstore.enabled=true)验证上述四条手工项
```

## 8. 不在本任务范围

- 账号 API 的任何后端改动(S6 已交付);
- 密码强度策略(后端 bcrypt cost 已定);
- 设置页的第二套账号表单(只读概览原则)。

## 9. 风险与回滚

- 风险:后端契约字段记忆偏差——实现前对照 handlers_users.go 现文,不凭记忆;
- 回滚:纯前端提交,revert 即可。

## 10. 实现记录

(执行时补写)
