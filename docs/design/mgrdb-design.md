# godelayq 管理库与任务存储 SQLite 化设计

> 分支基线:refactor/go-layout(module `github.com/jiujuan/godelayq`,实现包已全部下沉 `internal/`)。
> 本文档分两段:§2–§9 记录**已落地**的管理库改造(S1–S6,feat/db-migration 分支实施、随本分支携带);
> §10–§14 是**本系列要实施**的剩余工作:任务快照迁 SQLite(M03)与控制台账号管理页(M02)。
> 范围演进说明:2026-11-07 初版方案曾把任务快照划出范围;随 go-layout 重构收口,任务快照迁库
> 纳入本系列(用户 2026-11-08 拍板),`store.type` 扩展点在初版即已预留。

## 1. 背景与目标

godelayq 的运行状态此前全部落在三处平面文件:任务快照 `data/jobs.json`、分组 `data/groups.json`、
执行器档位 `exec-profiles.json`(路径取自 `store.*` 配置),控制台账号则在 `server.auth.users`
配置节里明文结构、bcrypt 哈希入库文件。平面形态的问题:

- 任务快照每 200ms 全量重写一个 JSON,任务量增长后写放大与 fsync 代价线性上涨;
- 账号声明在配置文件里,增删账号必须重启进程(热重载明确拒绝凭据键);
- 分组改名要连带改写任务标签,读写双方都是"整文件读改写",没有并发保护。

目标:以 store/sqlite 公共底座(P1–P3 已完成)为地基,把账号/分组/档位收进管理库
`data/goqsys_mgr.sqlite`(**已完成**),把任务快照收进 `data/goqsys_jobs.sqlite`(**本系列**),
并提供控制台账号的在线管理页面(**本系列**)。

## 2. 决策摘要

| # | 决策 | 理由 |
| --- | --- | --- |
| D1 | 管理库 `data/goqsys_mgr.sqlite` 单库承载账号/分组/档位;任务快照**独立** `data/goqsys_jobs.sqlite` | 任务写入是热路径(每次状态迁移一写),与管理库的冷数据(账号变更)隔离 WAL 与锁竞争;观测库 `data/*.sqlite` 本就独立,三库各管一面 |
| D2 | 接口定义在消费方:`core.UserStore`/`core.GroupStore`/`core.ExecutorProfileStore` 在 core,`store/mgr` 隐式实现;任务存储沿用既有 `core.Store` | 仓内治理惯例(observe/mgr 同口径),守卫测试钉死方向 |
| D3 | 一次性导入,不做双写:`store/mgr` 库空且来源有值时导入一次、mgr_meta 打分项标记,之后配置与 JSON 不再读取;任务侧同一手法(jobs_meta) | 双写的对账复杂度不值得;导入失败宁可启动失败,不做半迁状态 |
| D4 | JWT 密钥与机器 token **不入库**,留在配置/环境变量 | 密钥轮换语义与库生命周期不同;配置节的 reject 语义已正确 |
| D5 | 任务 SQLite 存储为**直写**(每变更一事务),不做内存缓存合并 | WAL + synchronous=normal + MaxOpenConns(1) 下单写延迟毫秒级,任务状态迁移频度远低于事件流;批量器是 observe 高频事件流的方案,不预先照搬 |
| D6 | 终态留痕(trim)语义只在 core 有一份:抽 `core.TrimTerminal` 共享函数,JSON 存储与 SQLite 存储共用 | 留痕规则(条数/时长/平局按 ID)是行为契约,两份实现必然漂移 |
| D7 | `store.type: json|sqlite`,`store.type` 变更是 ClassRestart | 字段与热重载分类在初版已预留;存储后端切换天然需要重启,不伪装成热能力 |
| D8 | 账号管理 API 用 admin 档(非 ops),页面路由 `minimumRole: 'admin'` | 管账号的人不必然是运维;与 handlers_users.go 既有实现一致 |

## 3. 现状盘点(已核实,2026-11-08)

**已落地(S1–S6,随 refactor/go-layout 携带)**:

- `internal/core/config.go`:`MgrStoreConfig{Enabled,Path,BusyTimeout,Synchronous}`,默认路径
  `data/goqsys_mgr.sqlite`;热重载表里 `mgrstore.*` 全部 ClassRestart。
- `internal/core/user_store.go`:`UserRecord`/`UserStore` 接口;`core/group_store.go`、
  `core/executor_profile_store.go` 同构。
- `internal/store/mgr/`:`doc.go`(依赖边界)、`schema.go`(五张表:
  mgr_users/mgr_groups/mgr_executor_profiles/mgr_meta/mgr_schema_migrations)、
  `db.go`(`Open(cfg core.MgrStoreConfig, logger)` = sqlite.OpenFile + Migrate)、
  `users.go`/`groups.go`/`profiles.go`(三 store,record_json 整存或列化按域选择)、
  `import.go`(`ImportOnce`,分项标记、库非空绝不覆盖)。
- `internal/api/`:`authenticator.go` 切库(库内账号 + 写后失效缓存),`handlers_users.go`
  账号管理端点(`GET/POST /admin/users`、`PUT/DELETE /admin/users/:name`,admin 档,
  未启用账号存储回 503,"最后一个 admin"不可删降)。
- `cmd/server/main.go`:`newMgrDB` 装配闭包,`mgrstore.enabled` 分岔账号/分组/档位三处来源。
- `.gitignore`:`data/*.sqlite*` 已覆盖两个库文件。

**任务存储现状(M03 的改造对象)**:

- `internal/core/store.go`:`Store` 接口六方法(Save/Update/Delete/LoadAll/Flush/Close +
  SetHistoryRetention),JSON 实现为内存 map + 周期合并落盘,终态留痕 trim 在写入后执行。
- `StoreConfig.Type` 字段已存在(注释"目前仅支持 json"),`store.type`/`store.path`/
  `store.flush_interval` 均已分类 ClassRestart,`store.history_limit`/`history_ttl` 为 ClassHot。
- 调度器恢复路径按状态过滤 `LoadAll()` 结果,终态记录是 API 历史页的数据源之一。

## 4. 范围

**本系列做**:

- M02 控制台账号管理页(对接既有 `/admin/users`,见 §13);
- M03 任务快照 SQLite 存储(`store.type: sqlite`),含一次性导入;
- M04 文档同步与终验(README、deployment、config.example 三处同步)。

**不做**(与初版口径一致):

- 事件流/审计迁 jobs 库(它们属于观测库,已各就其位);
- 双写/灰度切换(一次性导入口径,D3);
- 任务级 SQL 查询接口(Store 接口不变,SQLite 只是持久化后端,不做查询下推);
- jobs 库的远程/嵌入式以外形态(仍是本地单文件)。

## 5. 总体架构(M03 视角)

```
                    ┌─ store.type: json ────→ core.JSONFileStore(现状不变)
cmd/server 装配 ────┤
                    └─ store.type: sqlite ──→ store/jobs.Open(cfg.Store.SQLite, …)
                                                   │ sqlite.OpenFile + Migrate
                                                   ▼
                                        data/goqsys_jobs.sqlite
                                        jobs(id,status,name,updated_at,snapshot_json)
                                        jobs_meta(导入标记)

core.Store 接口不变;调度器、API、目录加载器全部无感。
```

- 新包 `internal/store/jobs`,与 observe/mgr 同构:经 `store/sqlite` 用驱动、自己不 import 驱动、
  实现的接口(`core.Store`)定义在 core;包守卫测试沿用 `TestSQLitePackageImportsNothingInRepo` 口径。
- 复用 sqlite 底座的 `Options/OpenFile/Migrate/SQL()`,迁移表名 `jobs_schema_migrations`。

## 6. 数据模型(M03)

```sql
-- version 1
CREATE TABLE jobs (
    id            TEXT PRIMARY KEY,      -- JobSnapshot.ID
    name          TEXT NOT NULL,         -- 冗余列,给将来的排查 SQL 用,不参与接口查询
    status        TEXT NOT NULL,         -- JobStatus 名(scheduler 写入侧已收敛封闭词表)
    updated_at    INTEGER NOT NULL,      -- UnixNano,留痕淘汰的排序键
    snapshot_json TEXT NOT NULL          -- JobSnapshot 整存,唯一权威;列只为运维观察存在
);
CREATE INDEX idx_jobs_status      ON jobs(status);
CREATE INDEX idx_jobs_updated_at  ON jobs(updated_at);

CREATE TABLE jobs_meta (               -- 与 mgr_meta 同手法:键值标记
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
```

- **snapshot_json 整存为权威,列只做冗余**:JobSnapshot 演进时改列是迁移负担,改 JSON 无感;
  列化只到"运维能肉眼排查"的程度(id/status/updated_at),与 mgr 库"分组列化、档位整存"的
  分级一致——查询需求出现在接口上之前,不提前列化。
- updated_at 用 UnixNano 整数:可排序、精度足、跨时区无歧义。

## 7. 写入路径与并发(M03)

- 每次变更(Save/Update/Delete)一个事务,`synchronous=normal` + WAL;连接
  `MaxOpenConns(1)` + `busy_timeout`(取值口径与 mgr 库一致,默认 5s)。
- **直写无缓存**:接口契约里"Flush 无变更时不产生写入"对 SQLite 实现是恒真;Flush 为 no-op。
  写失败即向调用方返回错误——JSON 实现的后台合并失败只记日志,那是因为它有"下次周期再写"的
  兜底;直写没有后台,失败必须当场暴露。调度器对 Store 错误的处理路径已存在,无需新增。
- **终态留痕 trim**:任一终态写入后执行,与 JSON 实现同一语义(见 D6):
  1) `limit<0`:删除全部终态行;
  2) `ttl>0`:删除 `updated_at < now-ttl` 的终态行;
  3) `limit>=0`:按 `updated_at DESC, id ASC` 保留前 limit 条终态行,其余删除。
  实现取**轻查询 + Go 判定**:SELECT id,status,updated_at(不取 snapshot_json),用
  `core.TrimTerminal` 算出待删集合后按 ID 批量 DELETE。行数受留痕上限约束(默认 1000),
  O(n) 判定不做 SQL 端复刻——语义只许有一份。
- `Pending/Running 永不淘汰` 的不变式由 `core.TrimTerminal` 统一保证。

## 8. 读取路径(M03)

- `LoadAll()`:SELECT snapshot_json 全量 → `json.Unmarshal` 逐行;调度器恢复与 API 历史页
  照旧按状态自行过滤,Store 不提供状态过滤查询(范围外,D7 注)。
- 一次性导入:打开库后,若 `jobs_meta['imported_from_json']` 不存在**且** jobs 表为空**且**
  `store.path` 指向的 JSON 文件存在 → 整文件读入 → 单事务插入 → 打标记。三条件缺一即跳过;
  打过标记后 JSON 文件永不再读。导入失败 = 启动失败(不做部分导入)。

## 9. 配置面(M03)

```yaml
store:
  type: json                 # json | sqlite;变更需重启(ClassRestart)
  path: ./data/jobs.json     # json 后端的数据文件;sqlite 后端下仅作为一次性导入源
  flush_interval: 200ms      # 仅 json 后端;sqlite 直写,此键不生效(文档明示)
  history_limit: 0           # 两后端同语义;ClassHot 运行期可调
  history_ttl: 0s            # 同上
  sqlite:                    # 仅 type: sqlite 时生效
    path: ./data/goqsys_jobs.sqlite     # 空取 core.DefaultJobsDBPath
    busy_timeout: 5s                    # 0 取默认,与 mgrstore 同值
    synchronous: normal                 # normal|full,与 mgrstore 同口径
```

- `core.StoreConfig` 增加 `SQLite StoreSQLiteConfig`(结构体字段,零值即默认);新增常量
  `DefaultJobsDBPath = "./data/goqsys_jobs.sqlite"`、`DefaultJobsBusyTimeout`、
  `DefaultJobsSynchronous`,取值与 mgr 三常量同值(不抽公共结构,防语义分叉——mgrstore 先例)。
- 热重载分类补三键:`store.sqlite.path|busy_timeout|synchronous` 全部 ClassRestart;
  `config.example.yaml` 与本机 config.yaml 同步(`TestExampleConfigMatchesLocal` 把关)。

## 10. 后端改动方案(逐文件)

| 文件 | 动作 |
| --- | --- |
| `internal/core/store.go` | 抽出 `TrimTerminal(data map[string]JobSnapshot, limit int64, ttl time.Duration) []string`(纯函数);JSON 的 `trimTerminalLocked` 改为调用它,行为由既有 store_history_test 钉死 |
| `internal/core/config.go` | `StoreSQLiteConfig` 类型 + `StoreConfig.SQLite` 字段 + 三个 Default 常量 + Validate |
| `internal/core/config_reload.go` | 三个 `store.sqlite.*` 键分类 ClassRestart |
| `internal/store/jobs/doc.go` | 包说明与依赖边界(照 mgr/doc.go 口径) |
| `internal/store/jobs/schema.go` | version 1 迁移:jobs + jobs_meta + 两索引 |
| `internal/store/jobs/db.go` | `Open(cfg core.StoreSQLiteConfig, historyLimit int, historyTTL time.Duration, logger *slog.Logger) (*DB, error)`;实现 `core.Store` 六方法 + SetHistoryRetention(原子值,与 JSON 实现同口径);Flush no-op、Close 幂等 |
| `internal/store/jobs/import.go` | 一次性导入(三条件 + 单事务 + 标记) |
| `internal/store/jobs/*_test.go` | 往返/留痕(条数与 TTL)/导入三条件/Close 幂等/守卫;与 JSON 实现的行为对照用例 |
| `cmd/server/main.go` | 装配闭包按 `cfg.Store.Type` 分岔(`json`→现状,`sqlite`→jobs.Open);未知值启动报错 |
| `.gitignore` | 已覆盖(`data/*.sqlite*`),无需改 |

## 11. 安全模型

- jobs 库内容是任务快照(payload 可能含业务数据),不入 git(已覆盖),权限沿用 data/ 目录;
- 导入源 JSON 处理完后**不删除不重命名**——删除是运维动作,进程不代劳(文档注明可归档);
- 无新增网络面;库文件损坏 = 启动失败并报错,不做静默重建。

## 12. 兼容性与回滚

- `store.type` 缺省即 `json`,现有部署行为零变化(默认关闭口径,与 observability/mgrstore 同);
- 切到 sqlite 后想回 json:改回 `store.type: json` 重启即可,但 **jobs.json 是旧的**——
  回滚前需把库中快照导出(本系列提供 `LoadAll` 语义的离线导出说明,不写工具);
- 导入只发生在"库空 + 未打标"时,重复启动幂等;
- schema 迁移走底座 `Migrate` 框架,version 1 起步,后续演进只追加。

## 13. 控制台账号管理页(M02)

对接既有 `GET/POST /api/v1/admin/users`、`PUT/DELETE /admin/users/:name`
(`UserView{name,role,created_at,updated_at}`,哈希绝不出后端;未启用账号存储回 503;
最后一个 admin 保护在后端)。前端按仓内既有模式:

- `web/src/api/types.ts`:`UserView`/`CreateUserRequest`/`UpdateUserRequest`(RoleName 已有);
- `web/src/api/admin.ts`:`listUsers/createUser/updateUser/deleteUser` 四函数;
- `web/src/api/keys.ts`:`users` 查询键;
- `web/src/views/AdminUsersView.vue`:CRUD 范本取 GroupsView(列表+表单+UiConfirm 删除),
  503 说明态取 AuditView(notEnabled),错误描述/busy/toast 取 AdminView 的 run() 模式;
  密码输入 `type="password"`;删除确认预置"最后一个 admin 不可删除"后果文案;
- `web/src/router/index.ts`:`/admin/users`,meta `{title:'账号', icon:Users, minimumRole:'admin'}`,
  菜单与守卫自动生效;
- `web/src/views/SettingsView.vue`:94–97 行"改动需重启进程"文案过时,改写并加账号页入口
  (设置页保持只读概览原则,不做第二套表单)。

## 14. 验收清单

- [ ] `store.type: sqlite` 下启动:建库、迁移、导入(若 jobs.json 存在)、调度器恢复、任务 CRUD 全链可用
- [ ] 导入幂等:重复启动不重复导入;打过标记后改 JSON 无效果
- [ ] 留痕语义与 JSON 实现一致:同一组操作序列在两后端产生相同的可见快照集合(对照测试)
- [ ] `store.type: json`(缺省)行为零变化;未知 type 启动报错
- [ ] `go build/vet/test ./...` 全绿;`-race` 视工具链;守卫测试通过(jobs 包不 import 驱动)
- [ ] 账号页:admin 可增改删,ops 不可见入口;503 态、最后 admin 保护有界面反馈
- [ ] `npx vue-tsc --noEmit` 与 `npm run build` 通过
- [ ] config.example 与本机 config.yaml 键集合一致(既有测试把关)

## 15. 实施计划与实现差异

| 卡 | 内容 | 状态 |
| --- | --- | --- |
| M01 | 本设计文档 | 本提交交付 |
| M02 | 控制台账号管理页(§13) | 待实施 |
| M03 | 任务快照 SQLite 存储(§5–§12) | 待实施 |
| M04 | 文档同步与终验 | 待实施 |

实现与设计的差异逐条标注在 `tasks/mgrdb/` 对应卡的"实现记录"节。
