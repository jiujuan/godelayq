# TASK-RE03　幂等提交

- 所属阶段：可靠执行
- 依赖任务：RE02（同库迁移顺延，避免版本号冲突）
- 涉及文件：`store/jobs` 迁移 v3 与 `idempotency.go`；新增 `api/idempotency.go`、`api/idempotency_test.go`；改 `api/dto.go`、`api/handlers.go`、`api/server.go`（Option）、`core/store.go`（`SnapshotReader`）、`core/config.go`、`core/scheduler.go`（findSnapshot 收敛）
- 预计规模：中

## 1. 任务目标

同一请求重发不产生第二个任务：键首次出现登记、同指纹重放返回既有任务、异指纹冲突 409。

## 2. 背景与当前问题

设计文档 §3.2：`createJobFromRequest` 不收客户端 ID，`Schedule` 恒 `generateID()`——提交方超时重试就是重复任务。调度器侧 `findSnapshot` 靠 `LoadAll` 线性扫描（既有注释挂账"等 Store 有 Get(jobID) 之后一起收敛"），幂等撞 `job_id` 的判定也需要按 ID 查询。

## 3. 要实现的功能

1. `core/store.go` 增 `SnapshotReader` 窄接口：`Get(jobID) (JobSnapshot, bool, error)`；`core.JSONFileStore`（map 查）与 `store/jobs.Store`（主键查）实现；`Scheduler.findSnapshot` 改用它（收敛既有挂账），`hasStoredJob` 同步受益。
2. `store/jobs` 迁移 v3：`job_idempotency` 表（设计文档 §6.1 DDL）+ 查重/登记/按 TTL 淘汰方法；`store.idempotency_ttl` 配置（默认 24h）。
3. api 层：
   - 幂等存储窄接口 `IdempotencyStore`（`Lookup(key)` / `Put(key, jobID, fingerprint)` / `PruneExpired`）定义在 `api/idempotency.go`（消费方是 api），`store/jobs.Store` 隐式实现 + 编译期断言，`server.go` Option 注入；RE07 事务化后登记并入 `UpdateWithEvents`。
   - `Idempotency-Key` 请求头（trim 后 1..128 字符，超长/重复空白 400）；`CreateJobRequest` 增 `job_id`（可选，`[A-Za-z0-9_-]{1,64}`）。
   - 查重与登记在 `createJobFromRequest` 的 `Schedule` 之前（查表 → 命中按 §7.3 的响应表走 200/409 → 未命中才创建并登记）。登记与创建的原子性：库模式在 `Schedule` 同事务能力就绪前（RE07），登记在创建成功后独立写——登记失败记 error 日志（幂等保护降级但不挡提交），注释写明该窗口。
   - 指纹：设计文档 §7.3 的固定结构体 `json.Marshal` + SHA256；结构体字段就是指纹口径的唯一权威，增删请求字段必须同步（注释声明）。
   - 响应头 `Idempotency-Replayed: true` 标记重放；批量接口（`POST /jobs/batch`）逐条独立走同一套。
4. JSON 模式：api 进程内 map 实现（`server.go` 的 Option 注入），重启失效——Option 文档字符串写明"JSON 模式的幂等只在进程生命周期内成立"。
5. 配置：`store.idempotency_ttl`。

## 4. 实现步骤

1. `SnapshotReader` 接口 + 两实现 + `findSnapshot` 收敛（独立提交，回归网明确）。
2. 迁移 v3 + jobs 侧方法 + golden 重生成。
3. api 幂等路径 + 指纹 + 测试。
4. JSON 模式 map 实现 + 配置。

## 5. 测试要求

- `api/idempotency_test.go`：同键同 payload 重放 → 200 + 同 job_id + `Idempotency-Replayed: true`（且库/堆里只有一条）；同键异 payload → 409；`job_id` 撞非终态同指纹 → 200、异指纹 → 409；无键 → 行为与现状一致（201）；批量逐条独立（一条 409 不影响其余 201）。
- 指纹敏感性：payload/cron/max_retries 任一变化都判异（表驱动）。
- jobs 双后端：登记/查重/TTL 淘汰。
- `SnapshotReader`：`findSnapshot` 收敛后既有调度器测试全绿。

## 6. 完成标准（DoD）

1. 重放不产生第二个任务；409 响应 details 能指明冲突键。
2. 迁移 v3 落地、golden 重生成；`findSnapshot` 不再 LoadAll 扫描。
3. `store.idempotency_ttl` 进 config.example 与本机 config，测试把关。

## 7. 验收方式

curl 连发两次同一 `Idempotency-Key` 的提交（中间加 `-w '%{http_code}'`）→ 第一次 201、第二次 200 且 `GET /jobs` 只有一条；改 payload 重发 → 409。JSON 模式重启后同键可再建（预期降级）。

## 8. 不在本任务范围

幂等键与 outbox 同事务（RE07 的 `UpdateWithEvents` 覆盖后收口）、`/jobs/batch` 的事务性（保持逐条独立）、控制台提交页的幂等键输入框。

## 9. 风险与回滚

- **风险：指纹口径与调用方预期不一致**（改了无关字段也 409）。缓解：指纹字段集在设计文档 §7.3 定死并注释；触发时间类字段用**解析后**的 trigger_at 参与（同一延迟语义的不同写法同指纹）。
- **风险：登记与创建之间的窗口**（创建成功、登记失败 → 重发会重复）。缓解：RE07 事务化前先记 error 日志留痕；窗口仅在存储故障时出现，与既有"落盘失败只记日志"口径一致。
- 回滚：revert 后遗留表无害；api 层头部参数对旧调用方不可见。

## 10. 实现记录

（待实现后回填。）
