# TASK-W03　把档位登记表改成可变的（整表原子替换）

- 所属阶段：M1 可变注册表
- 依赖任务：TASK-W02
- 涉及文件：`executor/registry.go`、`executor/registry_test.go`、`executor/register.go`（只读确认 + 启动日志计数）、`executor/source.go`（新增，可并入 registry.go）
- 预计规模：中

## 1. 任务目标

让 `executor.Registry` 能在运行期被整表替换，并给每条档位加上来源与降级标记。
本卡结束时：读侧（api 请求路径）在任何并发下都只能看到一份自洽的表；写侧只在 W05/W06 里被调用。

## 2. 背景与当前问题

`Registry` 现在的承诺写在字段注释上："构造之后不再变化，因此读方法不需要加锁"
（`executor/registry.go:23`，`entries`/`keys` 在 33-34，一次填好在 44-85）。
而它的读方法全部在请求路径上：

| 读方法 | 请求路径调用点 |
| --- | --- |
| `Enabled`/`RequiredRole`/`MaxTimeout`/`Profiles`/`Available` | `api/handlers_executors.go:370-377`（`GET /executors`） |
| `RequiredRole` | `api/handlers_executors.go:495`（提交鉴权 `executorRole`） |
| `Lookup` | `api/handlers_executors.go:509`（`executorProfile`） |
| `Available` | `api/handlers_executors.go:560`（提交期"这台机器能不能跑"） |
| `EffectiveTimeout` | `api/handlers_executors.go:594`（生效超时落进任务） |
| `InlinePreview` | `api/handlers.go:697`（结果预览裁剪） |

保留这份"读者看到的一定是自洽的一张表"的承诺，比给它逐方法加锁更省事，也更难被后来人写错（D9）：
写侧构造一份全新的表，一次 `Store` 换掉指针。

## 3. 要实现的功能

1. 不可变快照：

   ```go
   type snapshot struct {
       entries map[string]*entry
       keys    []string // 已按注册键字典序
   }
   ```

   `Registry` 持有 `data atomic.Pointer[snapshot]`，`entries`/`keys` 两个字段消失。
   所有读方法改为**一次** `Load()`，禁止一个方法里读两次（两次可能拿到不同的表）。
2. `entry` 多两个字段：`source Source`（`SourceConfig` / `SourceStore`）、`degraded bool`。
   导出两个只读访问器 `SourceOf(key) (Source, bool)`、`Degraded(key) bool`，
   以及 `Profiles()` 返回的元素上能取到这两个信息（`Profile` 本身**不加**这两个字段：
   来源是登记表的事实，不是档位定义的一部分）。
3. 写侧：

   ```go
   func (r *Registry) ApplyStore(items []StoreEntry) error
   ```

   `StoreEntry{Profile *Profile, Probe ProbeResult, Source Source, Degraded bool}`。
   语义是**整表重建**：调用方给出此刻全部 store 档位，Registry 与既有 config 档位合并成新快照并替换。
   - 合并规则：注册键相同则返回错误（I1 全局唯一由调用方在写之前挡住，这里只是兜底）。
   - `degraded=true` 的条目**进表**（于是 `GET /executors` 看得见、`Lookup` 也查得到），
     但 W05 的注册循环会跳过它（`executor/register.go:84` 那一侧不动，跳过逻辑在 W05）。
   - 一次调用一个原子替换，没有"先删后加"的中间态。
4. `NewRegistry`（44-85）改为构造快照 + 填 config 来源条目；`enabled=false` 时仍然是
   完全惰性（57-61 那条早退不动），返回的 Registry 里 store 侧为空。
5. 读方法签名与语义**全部保持不变**：`Keys`/`Profiles`/`Lookup`/`ProbeOf`/`Available`
   的现有调用方（上表七处）一行不改。`Available` 对 `degraded` 条目返回 `(reason, false)`。
6. 删掉 `registry.go:23` 那句注释，换成新承诺：
   "表本身不可变；替换是整表一次完成，因此读侧永远看到自洽的一张表，读方法不需要加锁。"

## 4. 实现步骤

1. 先只动结构（`snapshot` + `atomic.Pointer`），读方法逐个改成一次 `Load()`；
   跑一遍 `go test ./executor ./api ./cmd/server -race`，确认这一步已经绿（行为应零变化）。
2. 加 `Source`/`Degraded` 与两个访问器。
3. 加 `ApplyStore`，内部用写锁串行化（一把 `sync.Mutex` 只保护"构造新快照并替换"这段，
   读侧不加锁）。
4. 补测试（§5）。
5. 更新 `register.go` 的启动日志计数：`Registration`（`executor/register.go:25-33`）多一个
   `Degraded int` 字段，日志行加同名键（这是 W05 需要的信息，本卡一并给）。

## 5. 测试要求

1. 单元：
   - `ApplyStore` 后 `Keys()` 同时含 config 与 store 条目且按字典序。
   - 重复 `ApplyStore` 用同一批条目 → 结果幂等；用不同批 → 旧 store 条目消失（整表重建语义）。
   - 注册键与 config 条目相同 → 返回错误且**旧表仍在**（失败不得留下半成品）。
   - `degraded` 条目：`Lookup` 查得到、`Available` 回 `(原因, false)`、`SourceOf` 是 `store`。
   - `NewRegistry` 在 `enabled=false` 时 `ApplyStore` 可调用但表仍空，且不探测（守 57-61 那条早退）。
2. 并发（`-race` 关键用例）：一个 goroutine 循环 `ApplyStore`（100 轮，每轮换一批档位），
   同时 8 个 goroutine 循环 `Profiles()`+`Lookup()`+`Available()`；
   断言读者每次拿到的 `Profiles()` 里**所有元素来自同一轮**（给每轮一个可辨识的 marker，
   例如参数名里带轮次号）。这条用例就是 I3"读者永不自相矛盾"的落点。
3. 现有用例回归：`executor/register_test.go`、`cmd/server/main_test.go:44、59` 与
   `api/executors_submission_test.go` 不改预期即通过（读方法签名未变的直接证明）。
4. 手工：本卡无外部可见行为，跳过并在实现记录里写明"待 W05 之后统一走查"。

## 6. 完成标准（DoD）

- [ ] `entries`/`keys` 字段消失，读方法各自只做一次 `Load()`（grep 确认同一方法里没有第二次 `r.data.Load()`）。
- [ ] 七个既有请求路径调用点（§2 表）一行未改。
- [ ] `ApplyStore` 是整表重建 + 一次原子替换，失败路径不动旧表。
- [ ] `source`/`degraded` 有访问器，`Profile` 结构未被污染。
- [ ] `Registration` 多 `Degraded` 计数并进启动日志。
- [ ] §5.2 的并发用例在 `-race` 下绿，且**反向验证过**：临时把整表替换改成"先删后加"，
      该用例必须失败；改回后通过，记录里写清这两步的输出。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

## 7. 验收方式

```bash
go test ./executor -run 'TestRegistry' -race -count=5 -v
go test ./api ./cmd/server -race
go build ./... && go vet ./...
```

预期：全部 `ok`；`-count=5` 无 flake。

## 8. 不在本任务范围

- 不做调度器侧的热注册/摘除（W04）。本卡只保证登记表可变且可读得自洽。
- 不做启动合并（W05）、不做端点（W06）。
- 不改 `RequiredRole`/`MaxTimeout`/`InlinePreview`/`EffectiveTimeout` 的取值来源（仍是配置）。
- 不给 `Registry` 加"只改一条"的增量方法：页面一次改动就重建整表，条目数量级用不着增量。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 一个方法里读两次快照 | 拿到两份表，出现"profile 在但 probe 不在"这类自相矛盾 | §4.1 改完先跑一遍 + DoD 的 grep 检查 + §5.2 并发用例 |
| `atomic.Pointer[T]` 需要 Go 1.19+ 且 `T` 必须是指针 | 本仓已用（观测层同款写法），但仍要确认 `go.mod` | 若泛型写法受限，退化成 `sync.RWMutex` + 字段替换，DoD 不变 |
| 整表重建丢掉 config 条目 | `GET /executors` 突然少掉 yaml 里的档位 | `ApplyStore` 只接收 store 条目，config 部分由 Registry 自己保留 |
| 并发用例假绿（marker 设计不当） | 反向验证步骤就是为它准备的 | DoD 明确要求做"故意改坏"那一步 |

回滚：本卡改动集中在 `executor/registry.go`；读方法签名未变，所以退回只是把快照换回两个字段。
`Registration.Degraded` 若被 W05 依赖，回滚时要一并撤掉那一处日志键。

## 10. 实现记录（执行时补写）

（待补：落地的接口 / 与本卡写法的差异 / 验证证据 / 手工验收 / 缺陷 / 未覆盖项）
