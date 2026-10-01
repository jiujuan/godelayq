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

   `StoreEntry{Profile *Profile, Probe ProbeResult, Source Source, Degraded bool, Reason string}`。
   语义是**整表重建**：调用方给出此刻全部 store 档位，Registry 与既有 config 档位合并成新快照并替换。
   - 合并规则：注册键与 config 条目撞上时**分两种** —— 条目带了 `Degraded` 就进降级面
     （`Registry.Degraded()`），没带就返回错误。落地后修订，见 §10.2 第 2 条：
     注册键是 map 的键，同名两条无法在 `entries` 里共存，所以卡原文的"`degraded=true` 的条目进表、
     `Lookup` 也查得到"改成"进降级面"，`Lookup`/`Profiles`/`Available` 一律只看生效那一条；
     `GET /executors` 要把两批合起来输出（W07 §3 已按此改写）。
   - 同批内注册键重复、`Profile` 为 nil、执行器关闭：三种都返回错误且旧表原样不动。
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
   - `NewRegistry` 在 `enabled=false` 时表是空的且不探测（守 57-61 那条早退）；
     此时 `ApplyStore` **直接返回错误**（落地后修订：卡原文写的是"可调用但表仍空"，
     那会造出"接口看得见、调度器跑不了"的半状态；见 §10.2 第 4 条）。
2. 并发（`-race` 关键用例）：一个 goroutine 循环 `ApplyStore`（落地是 120 轮、每轮换一批两条 store 档位），
   同时 8 个 goroutine 循环 `Profiles()`+`Lookup()`+`Available()`+`Keys()`+`SourceOf()`+`Degraded()`；
   轮次标记落地时从"参数名里带轮次号"改成**用 `Profile.Timeout` 当标记**（不必构造合法参数声明，
   且同轮两条档位共享同一个值），断言两件事：
   一次 `Profiles()` 里所有 store 档位的 `Timeout` 相同（跨轮混读即失败），
   以及一次 `Keys()` 里 store 键的个数只能是 0 或 2（半成品表即失败）。
   这条用例就是 I3"读者永不自相矛盾"的落点，反向验证见 §10.4。
3. 现有用例回归：`executor/register_test.go`、`cmd/server/main_test.go:44、59` 与
   `api/executors_submission_test.go` 不改预期即通过（读方法签名未变的直接证明）。
4. 手工：本卡无外部可见行为，跳过并在实现记录里写明"待 W05 之后统一走查"。

## 6. 完成标准（DoD）

- [x] `entries`/`keys` 字段消失，读方法各自只做一次 `Load()`（grep 确认同一方法里没有第二次 `r.data.Load()`）。
      → `executor/registry.go` 的 9 个读方法各 1 次 `data.Load()`，`ApplyStore` 1 次，
      `Enabled`/`RequiredRole`/`LoaderAllowed`/`InlinePreview`/`MaxTimeout`/`EffectiveTimeout` 不读表
      （它们只取构造时定下的字段与配置），实测 grep 清单进 §10.3。
- [x] 七个既有请求路径调用点一行未改。
      → 本轮 `api/` 包零改动（`git status --short -- api` 空），
      `cmd/server` 与 `api` 的既有测试未改预期即通过。
- [x] `ApplyStore` 是整表重建 + 一次原子替换，失败路径不动旧表。
      → `TestApplyStore_MergesWithConfigAndReplacesStoreSide`（整批替换、幂等、config 保留）、
      `TestApplyStore_RejectsBadBatchesAndKeepsOldTable`（同批重复键 / 空 Profile / 执行器关闭三种失败后表原样）。
- [x] `source`/`degraded` 有访问器，`Profile` 结构未被污染。
      → `SourceOf(key)`、`Degraded()`；`Profile` 一个字段都没加（见 §10.2 第 2 条）。
- [x] `Registration` 多 `Degraded` 计数并进启动日志。
      → `TestRegisterCountsDegradedProfiles`：`Total=2 / Registered=2 / Degraded=1`，
      且注册表里只有生效那两条的键。
- [x] §5.2 的并发用例在 `-race` 下绿，且**反向验证过**：临时把整表替换改成"先删后加"，
      该用例必须失败；改回后通过，记录里写清这两步的输出。
      → 见 §10.4，故意改坏后 `-race` 报 8 条 `WARNING: DATA RACE`（读侧 `Profiles()` registry.go:282
      vs 写侧 `ApplyStore` registry.go:225），撤销后同一命令绿。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

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

## 10. 实现记录（2026-10-01）

### 10.1 落地的接口

`executor/registry.go`（176 行 → 约 340 行）：

- `Source`（`SourceConfig` / `SourceStore`）；`entry` 多 `source` 字段。
- `snapshot{entries, keys, degraded}` + `newSnapshot(entries, degraded)`（keys 在这里一次算出）。
- `Registry`：删掉 `entries`/`keys` 两个字段，换成 `writeMu sync.Mutex` + `data atomic.Pointer[snapshot]`；
  类注释里"构造之后不再变化，因此读方法不需要加锁"换成新口径，并写明**每个方法只做一次 Load**这条约束。
- 读方法：`Keys`/`Profiles`/`Lookup`/`ProbeOf`/`Available` 签名与语义不变，各自一次 `Load()`；
  新增 `SourceOf(key) (Source, bool)`、`Degraded() []DegradedProfile`（副本 + 按键排序）。
- 写方法：`ApplyStore(items []StoreEntry) error`，`StoreEntry{Profile, Probe, Source, Degraded, Reason}`，
  `DegradedProfile{Profile, Probe, Reason}`。
- `NewRegistry` 只装 config 侧；`enabled=false` 时存一张空表后早退（原那条惰性口径不动）。
- `executor/register.go`：`Registration` 多 `Degraded` 字段，启动日志多 `"degraded"` 键。

测试：`executor/registry_mutable_test.go`，8 个顶层用例（含 4 个子例）。

### 10.2 与本卡写法的差异

1. `ApplyStore` 的入参从"两个平行参数（`profiles []*Profile` + `probes map[string]ProbeResult`）"
   改成一条 `StoreEntry` 列表：把两份东西按名字对上，漏一条探测就静默变成"可用"，
   而结构体字段让编译器替我们把住。
2. **降级条目的位置改了**：卡 §3.3 说"进表，`GET /executors` 看得见、`Lookup` 也查得到"。
   实际做不到——`entries` 的键就是注册键，同名两条不能共存，硬塞要么顶掉 config 那条（违反 D4）
   要么让 `Lookup` 语义二义。落地是 `snapshot.degraded` 单独一份 + `Degraded()` 访问器：
   `Profiles()`/`Lookup()`/`Available()` 只看生效那一条，接口层把两批合起来输出。
   设计文档 §5.2、§6.3 与 W07 §3 已同步改写；**W07 输出 `degraded` 那批时 `key` 与生效那条相同**。
3. `source`/`degraded` 不放进 `Profile`：卡 §3.3 原话说"Profiles() 返回的元素上能取到这两个信息"，
   落地改成"按 `HandlerKey()` 反查 `SourceOf()` / 在 `Degraded()` 里"。
   理由写在方法注释里：`Profile` 是档位定义，来源与降级是登记表对它的位置判断，
   放进 `Profile` 会让"同一条档位定义"在两份来源下变成两个值。
4. `ApplyStore` 在执行器关闭时**返回错误**，不是卡 §5.1 说的"可调用但表仍空"。
   那种组合会造出"接口看得见档位、调度器没有处理函数"的半状态；
   而 W01 的配套校验（`web_enabled` 要求 `enabled=true`）已经让正常装配走不到这里，
   所以这条只是跨包写入口的边界。
5. `Degraded` 标志只在真撞名时有意义：没撞名却标了 `Degraded` 的条目按普通 store 条目生效，不报错。
   **W05/W06 注意**：降级判定要由调用方基于"与 config 撞名"给出，不能拿这个字段表达"我不想注册它"。
6. 并发用例的轮次标记用 `Profile.Timeout`（卡里建议的是参数名里带轮次号），
   并且多加一条 `Keys()` 里 store 键个数只能是 0 或 2 的断言——它比"同一轮"更容易抓到"删了两个只加回一个"。

### 10.3 验证证据

一次 `Load()` 的 grep 清单（`grep -n "data.Load()\|func (r \*Registry)" executor/registry.go`）：
9 个读方法各 1 次、`ApplyStore` 1 次；`Enabled`/`RequiredRole`/`LoaderAllowed`/`InlinePreview`/
`MaxTimeout`/`EffectiveTimeout` 不读表（只取构造期定下的字段与配置），因此与替换时序无关。

```
$ go test ./executor -run 'TestApplyStore|TestRegistry|TestRegisterCountsDegraded' -race   → ok (1.4s)
$ go test ./executor -race                                                                 → ok 22.6s
$ go build ./... && go vet ./...                                                           → 通过
$ go test ./... -race -count=1   → ok godelayq/api 90.5s
                                   ok godelayq/cmd/server 5.7s
                                   ok godelayq/core 12.4s
                                   ok godelayq/executor 22.6s
                                   ok godelayq/store/sqlite 4.3s
```

`api/` 包本轮零改动（`git status --short -- api` 为空），
`cmd/server/main_test.go`、`api/executors_submission_test.go` 等既有登记表用例未改预期即通过——
这是"读方法签名与语义未变"的直接证据。

### 10.4 反向验证（DoD 那条）

把 `ApplyStore` 的收尾从"构造新快照 + 一次 `Store`"临时改成"原地删掉 store 条目、逐条加回、重算 keys"
（就是卡 §9 第一条风险说的"先删后加"），跑：

```
$ go test ./executor -run 'TestRegistryReaderAlwaysSeesOneSelfConsistentTable|TestRegistryConcurrentReadsWithWrites|TestApplyStore' -race
WARNING: DATA RACE   × 8
  Read at 0x00c000117288 by goroutine 37:
    godelayq/executor.(*Registry).Profiles()  executor/registry.go:282
    ...registry_mutable_test.go:268
  Previous write at 0x00c000117288 by goroutine 32:
    godelayq/executor.(*Registry).ApplyStore() executor/registry.go:225
FAIL	godelayq/executor
```

撤销临时改动后同一命令 `ok`。结论：用例确实抓得住"跨两张表 / 原地改表"这一类实现，
不是假绿。失败的形式是 race detector 直接报读写冲突（比"读到跨轮标记"更硬）。

### 10.5 缺陷

本卡没发现生产缺陷；D-0101 仍挂着。

一条**留给 W05 的接口性质**（不是缺陷）：`ApplyStore` 只重建 store 部分，config 部分永久保留——
也就是说这份 API 无法表达"改配置里的档位"，这正是设计文档 §8 的口径（yaml 那部分仍然只能改文件 + 重启）。

### 10.6 未覆盖项

- `Snapshot` 层面的内存增长没有测：每轮整表替换都会留下旧快照等 GC，长时间高频写页面档位的表现没测过
  （数量级判断：档位几十条、写入按人工点击频率，可接受）。
- `Degraded()` 的 `Probe` 字段只是原样带出，没有测"降级 + 不可用"两个原因同时存在时的展示优先级——
  那是 W07 的文案问题，留给它。
- 手工验收跳过：本卡没有任何生产调用方（W05 才接线），界面与 REST 上看不出来变化，
  与卡 §5.4 的约定一致，端到端走查留到 W05/W06。

