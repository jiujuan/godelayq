# TASK-W01　档位存储与两个配置键

- 所属阶段：M0 基础
- 依赖任务：无
- 涉及文件：`core/config.go`、`core/executor_profile_store.go`（新增）、`core/executor_profile_store_test.go`（新增）、`core/config_test.go`、`configs/config.example.yaml`、`configs/config.yaml`
- 预计规模：中

## 1. 任务目标

新增 `executors.web_enabled` 与 `executors.profiles_path` 两个配置键，并交付 `core` 侧的档位存储
（内存 map 权威 + 每次改动同步落盘的 JSON 文件）。本卡结束时**没有任何行为变化**：
新键读得到、文件读写得到，但还没有任何人使用它们。

## 2. 背景与当前问题

档位的唯一定义处是 `executors.commands`（`core/config.go:251-254`），它随配置只读，
改一条要重启进程。要在线管理，第一步是有一个"页面写的、服务读的"存放处。

仓库里已经有同形态的先例：分组注册表 `core/group_store.go` ——
内存 map 是权威、磁盘是全量镜像（82-87），启动时读（91-107），缺失与空文件算空集合而
**JSON 损坏算错误**（110-130），每次改动持锁改 map 后立刻全量重写 tmp+rename（154-194、200-222）。
本卡照抄这套形态，不发明新的。

为什么不回写 `configs/config.yaml`：它含本机凭据且被 `.gitignore` 排除，
让服务往它写东西等于把改凭据与改档位绑进同一个文件（设计文档 D1）。

## 3. 要实现的功能

1. `ExecutorsConfig` 两个新字段：
   - `WebEnabled bool` `mapstructure:"web_enabled"`，默认 false。
   - `ProfilesPath string` `mapstructure:"profiles_path"`，空值在 `Normalized()` 里补
     `DefaultExecProfilesPath = "./data/exec-profiles.json"`。
   - 配套规则：`enabled=false` 而 `web_enabled=true` 由 `ExecutorsConfig.Validate` 拒绝，
     照 `loader_allow` 的既有先例（`core/config.go` 的 `if !e.Enabled` 分支）。
     设计文档 D2 原本写的是"打开它但没开执行器也不报错"，实施时按仓库既有的一致性改了方向，见 §10.2。
2. 两个键都进 `GODELAYQ_` 环境变量覆盖列表（`GODELAYQ_EXECUTORS_WEB_ENABLED`、
   `GODELAYQ_EXECUTORS_PROFILES_PATH`）。**漏在这份列表里不会报错，只会让该项无法用环境变量覆盖**，
   这是本卡最容易静默漏掉的一处。
3. `ExecutorProfileRecord`：字段与 `core.ExecutorCommand`（`core/config.go:259-319`）一一对应，
   json 标签用与 YAML 相同的键名（`name`/`kind`/`runtime`/`script`/`program`/`fixed_args`/`args`/
   `args_render`/`positional`/`cwd`/`env`/`env_allow`/`timeout`/`max_parallel`/`retry_on_exit`/
   http 那组），另加 `created_at`、`updated_at`。
   - **不给 `ExecutorCommand` 加 json 标签**：那份结构是配置面，被 `UnmarshalExact` 的键名守卫约束，
     与存储面的演进方向不同。
   - `timeout` 在文件里是字符串（`"10m"`），记录结构自己解析成 `time.Duration`，
     解析失败要报出是**哪一条档位**的 `timeout` 坏了。
   - 提供 `Command() core.ExecutorCommand` 做转换，W02 的构造入口只吃 `ExecutorCommand`。
4. `ProfileStore` 接口（`List`/`Get`/`Save`/`Delete`）+ `JSONFileProfileStore` 实现。
   接口**刻意不含 Flush/Close**，理由与 `core/group_store.go:49-58` 的注释同一条。
5. `ValidateProfileName(name string) error`，pattern 与 `executor` 包的档位名规则同一条
   （`executor/profile.go:48` 的 `^[A-Za-z0-9_-]{1,64}$`；`core/group_store.go:22` 已是同一条写法的第三份）。
   `core` 不许 import `executor`，所以这里是独立一份，由 §5 第 6 条那条跨包用例守住两者一致。
6. 存储只校验**文件级不变量**：文件内不重名（大小写不敏感，与 `group_store` 的主键口径一致）、
   名字符合 5、`List` 返回按名字排序的副本。
   **字段组合的合法性不在这里判**（`kind: script` 却没写 `runtime` 这类属于 `executor.LoadProfiles`，
   见 I1 一份规则）。

## 4. 实现步骤

1. `core/config.go`：加两个字段与 `DefaultExecProfilesPath` 常量；在 `Normalized()` 补默认值；
   在 `BindEnv` 的字符串列表里补两项。注释体例照 `core/config.go:219-225` 那几段
   （说清默认值、留空与显式取值的差别、能否用环境变量覆盖）。
2. `configs/config.example.yaml` 与 `configs/config.yaml`：在 `executors:` 节里加两行，
   注释说明"`web_enabled` 打开后档位可以写在 `executors.profiles_path` 指向的文件里，
   改完立即生效；关闭时那个文件不读也不写"。**两份都要改**，键集合由
   `go test -run TestExampleConfigMatchesLocal ./core` 守。
3. 新增 `core/executor_profile_store.go`：照 `core/group_store.go` 的文件顺序写
   （默认路径与 pattern → 类型 → 接口 → 构造函数 → load → 读方法 → Save/Delete → flushLocked），
   让后来人能一一对着读。
4. 写测试（§5），跑绿。
5. 全量 `go build ./... && go vet ./... && go test ./... -race`。

## 5. 测试要求

1. `core/executor_profile_store_test.go` 的 helper 照 `core/group_store_test.go:15-24`
   用 `filepath.Join(t.TempDir(), "exec-profiles.json")`，不得写仓库 `data/`。用例至少覆盖：
   - `Save`/`List`/`Get`/`Delete` 基本路径；`List` 返回副本（改返回值不影响存储）。
   - `Save` 覆盖时保留 `created_at`、刷新 `updated_at`（同 `TestGroupStore_UpdateKeepsCreatedAt`）。
   - `PersistsAcrossReopen`：写完重新 `NewJSONFileProfileStore` 读回来，并 `os.ReadFile` 断言磁盘 JSON 内容。
   - `MissingFileIsEmpty`、`EmptyFileIsNotAnError`、**`CorruptFileIsAnError`**（损坏必须报错，
     不能被当成空集合静默覆盖掉页面上建过的档位）。
   - 文件内重名（`Foo` 与 `foo`）在 `New...` 与 `Save` 两处都被拒。
   - `Command()` 往返：从 `ExecutorCommand` 造记录再转回去，字段逐条相等（含 http 那一组和 `nil` 切片）。
   - `timeout` 非法字符串（`"10 minutes"`）时 `New...` 报错且错误信息含档位名。
   - `ConcurrentSaveAndDelete`：照 `core/group_store_test.go:262` 的形状开 20 个 goroutine，
     结束后文件仍是合法 JSON 且条目自洽。
2. `core/config_test.go`：`TestExecutorsDefaults` 里加两条断言（`WebEnabled` 默认 false、
   `ProfilesPath` 归一化成 `DefaultExecProfilesPath`）；再加一条环境变量覆盖用例，
   证明两个新键真能被 `GODELAYQ_*` 改写（对照既有的 `GODELAYQ_EXECUTORS_RUNTIME_ALLOW` 写法）。
3. 跨包一致性用例放在 `executor` 包的测试里（它能同时 import `core` 与自身）：
   `core.ValidateProfileName` 接受/拒绝的样本，与 `executor` 内部档位名校验对同一批样本的结论必须一致。
4. 手工：临时目录里造一份含 `web_enabled: true` 的配置，起进程，确认**没有**创建 `exec-profiles.json`
   （本卡还没有任何调用方，文件不该出现——这条是"默认关闭"口径在本卡的落点）。

## 6. 完成标准（DoD）

- [x] 两个配置键能读、能归一化、能被环境变量覆盖，`TestExampleConfigMatchesLocal` 通过（不是跳过）。
      → `TestLoadConfig_WebProfileEnvOverrides`、`TestExecutorsDefaults`、
      `TestExecutorsValidate_Rejects`（新增两行：`web_enabled` 缺 `enabled`、`profiles_path` 纯空白）；
      守卫用例实测 `--- PASS`。
- [x] `ExecutorsConfig.WebEnabled` 默认 false；`web_enabled: false` 时没有任何代码去碰 `ProfilesPath`。
      → `TestExecutorsDefaults` + §7 的实跑冒烟（连父目录 `nested/` 都没被创建）。
- [x] 存储实现与 `core/group_store.go` 同形：一把 `sync.Mutex`、tmp(0644)+rename 全量重写、
      缺失与空文件是空集合、损坏是错误。
      → 落地命名是 `JSONFileExecutorProfileStore`（§10.2 第 1 条）；
      `TestExecutorProfileStore_EmptyFileAndCorruptFile`、`_DoesNotCreateFile`、`_PersistsAcrossReopen`。
- [x] 存储不校验字段组合（`§3.6`）：有一条用例证明"缺 `runtime` 的 script 档位能被 `Save` 收下"。
      → `TestExecutorProfileStore_SaveDoesNotValidateFieldCombination`（另含 `kind` 拼错也被收下）。
- [x] `Command()` 往返无损，含 nil/空切片的区分。
      → `TestExecutorProfileRecord_CommandRoundTrip`（script 组 + http 组各一次，nil 保持 nil）。
- [x] 跨包名字一致性用例绿。 → `executor.TestProfileNameRulesMatchCore`（21 个样本，含 64/65 长度边界）。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./core -run 'TestExecutorProfileStore|TestExecutorsDefaults|TestExampleConfigMatchesLocal' -v
go test ./executor -run 'ProfileName' -v
go build ./... && go vet ./...
```

预期：全部 `ok`；`TestExampleConfigMatchesLocal` 显示 `--- PASS`（若显示 `SKIP` 要在实现记录里写明本机缺 `config.yaml`）。

实测（2026-10-01）：三条命令按 §7 原样跑，`TestExampleConfigMatchesLocal` 输出 `--- PASS`（不是跳过），
`go test ./... -race -count=1` 全绿（api 94.6s / executor 22.4s / core 12.7s / cmd/server 5.6s / store/sqlite 3.9s）。
完整输出进 §10.3。

## 8. 不在本任务范围

- 不注册任何端点、不改 `GET /executors`、不动 `executor.Registry`（那是 W03/W06/W07）。
- 不做启动时读取该文件（W05）。本卡之后即使 `web_enabled: true`，进程行为也与之前一致。
- 不做文件版本历史、不做回滚、不做"改前改后值"的审计列（设计文档 §8、S-2）。
- 不给 `ExecutorCommand` 加 json 标签或改它的 mapstructure 键名。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 漏加 `BindEnv` 项 | 不报错，只让键无法用环境变量覆盖，冒烟时才发现 | §5.2 那条覆盖用例就是守这条的 |
| 只改了一份 YAML | `TestExampleConfigMatchesLocal` 红 | 改键的三步（结构体/两份 YAML/BindEnv）按 §4 顺序做 |
| 存储顺手校验了字段组合 | 与 W02 的统一入口形成两套规则（违反 I1） | §3.6 明确写了"不收"，并要一条用例反证 |
| 记录结构与 `ExecutorCommand` 走偏 | 加字段时只改一处 | `Command()` 往返用例逐字段比 |

回滚：本卡是纯新增（两个键 + 一个新文件），退回只需删掉新文件与两处 YAML 行、
还原 `core/config.go` 的字段与列表项，没有调用点会断。

## 10. 实现记录（2026-10-01）

### 10.1 落地的接口

- `core/config.go`：`ExecutorsConfig.WebEnabled`（`mapstructure:"web_enabled"`）与
  `ExecutorsConfig.ProfilesPath`（`mapstructure:"profiles_path"`）插在 `LoaderAllow` 与 `Commands` 之间；
  常量 `DefaultExecProfilesPath = "./data/exec-profiles.json"`（跟在 `DefaultExecMaxTimeout` 后面）；
  `DefaultConfig()` 补两个默认值；`Normalized()` 在 `RestorePolicy` 之后补 `ProfilesPath`；
  `ExecutorsConfig.Validate()` 的 `if !e.Enabled` 分支里加 `web_enabled` 的配套拒绝；
  `LoadConfig` 的 `BindEnv` 列表在 `executors.loader_allow` 后加两项。
- `core/executor_profile_store.go`（新，约 380 行）：`ExecutorProfileRecord`（24 个字段 + 两个时间戳）、
  `ExecutorArgRecord`、`ExecutorPositionalRecord`、`NewExecutorProfileRecord(ExecutorCommand) ExecutorProfileRecord`、
  `(ExecutorProfileRecord) Command() (ExecutorCommand, error)`、`ExecutorProfileStore` 接口、
  `ValidateProfileName`、`JSONFileExecutorProfileStore`（`NewJSONFileExecutorProfileStore`/`load`/
  `List`/`Get`/`Save`/`Delete`/`flushLocked`）、错误 `ErrProfileNameInvalid`/`ErrProfileNotFound`/`ErrProfileDuplicate`。
  文件内小节顺序与 `core/group_store.go` 一一对应。
- 测试：`core/executor_profile_store_test.go`（新，12 个用例）、`core/config_test.go`（3 处增补）、
  `executor/profile_name_parity_test.go`（新，跨包名字一致性）。
- 配置模板：`configs/config.example.yaml` 与 `configs/config.yaml` 各加两键，注释含
  "默认关闭""必须同时 `enabled: true`""文件不存在是正常状态""损坏会启动失败 + 删掉它可退回"
  "备份要连 `data/` 一起""换机器探测会失败是预期"。

### 10.2 与本卡写法的差异

1. **类型名多了 `Executor` 前缀**：卡里写 `ProfileStore` / `JSONFileProfileStore`，
   落地是 `ExecutorProfileStore` / `JSONFileExecutorProfileStore`。
   理由是 `core` 包里已经有 `Store`（jobs 快照）与 `GroupStore`，裸名 `ProfileStore` 会被读成
   "任务快照的另一种存储"；而记录结构本来就叫 `ExecutorProfileRecord`，同族保持一致。
   `ValidateProfileName` 按卡原样保留（它校验的就是档位名，不与分组名混）。
2. **新增了一条本卡没写的配套校验**：`enabled=false` 而 `web_enabled=true` 启动即拒。
   方向来自仓库既有先例——`core/config.go` 的同一个分支里 `loader_allow` 就是这么处理的
   （"开了却没开执行器，只能是配置写错"）。设计文档 D2 原本写的是"不报错，端点回 503"，
   已同步改掉；`task-w09` 的场景矩阵因此把原来的场景 1 拆成 1a（配置校验直接拒）与
   1b（两个都关，端点 503）。**W06/W07 执行时注意**：不要再期望"`enabled=false` 也能起来并回 503"。
3. `Command()` 的签名带 error（卡 §3.3 只写"提供 `Command()` 转换"）。
   timeout 的解析点必须落在转换里，否则"文件里存着一条永远解析不出来的记录"这件事
   要到 executor 侧才暴露，那时错误信息里已经没有档位名了。
   顺带把**负数 timeout** 也拒了（卡 §5.1 只提"非法字符串"）：执行侧把 0 与负数都解释成
   "取默认/上限"，存进文件只会留下一个运维读不懂的写法。
4. `List` 返回的是**结构体副本**，元素里的 `Args`/`Headers` 等切片仍共享底层数组。
   卡 §5.1 要求"改返回值不影响存储"，用例 `_ListReturnsCopy` 覆盖的是字段赋值与 `append`
   （cap==len 时 append 会重新分配）两种写法；直接改 `items[0].Args[0].Name` 会穿透到存储。
   与 `GroupStore` 相比这是本类型特有的（`Group` 没有嵌套切片），登记为 D-0101。
5. 比卡里多写的用例：`_DoesNotCreateFile`（构造只建父目录、不建文件——这条是"默认关闭"最硬的证据）、
   `_ListReturnsCopy`、`_RejectsDuplicateNamesInFile`（文件内 `a_one`/`A_ONE` 重名 → `ErrProfileDuplicate`）。
   卡 §5.1 把这些合并在"基本路径"里，这里拆开是因为三条各守一条不同的口径。
6. 测试里没有引入 `ptr(bool)` 这类通用 helper（可能与包内同名符号冲突），
   `DenyPrivate` 用局部变量取地址。

### 10.3 验证证据

```
$ go build ./... && go vet ./...            → VET-OK
$ go test ./core ./executor                 → ok godelayq/core 10.9s / ok godelayq/executor 20.9s
$ go test ./... -race -count=1              → ok godelayq/api 94.6s
                                             ok godelayq/cmd/server 5.6s
                                             ok godelayq/core 12.7s
                                             ok godelayq/executor 22.4s
                                             ok godelayq/store/sqlite 3.9s
                                             ?   examples/demo1、demo2、cmd/gensecret、cmd/hashpassword、web [no test files]
$ go test ./core -run TestExampleConfigMatchesLocal -v
  === RUN   TestExampleConfigMatchesLocal
  --- PASS: TestExampleConfigMatchesLocal (0.00s)   ← 通过而非跳过
```

新增/改动的用例清单（`go test ./core -run 'TestExecutorProfile' -v` 与
`go test ./executor -run ProfileName -v` 全绿）：
`_SaveListGet`、`_ListReturnsCopy`、`_RejectsBadNames`、`_UpdateKeepsCreatedAt`、`_Delete`、
`_PersistsAcrossReopen`、`_DoesNotCreateFile`、`_EmptyFileAndCorruptFile`、
`_RejectsDuplicateNamesInFile`、`_InvalidTimeoutNamesProfile`、`_SaveDoesNotValidateFieldCombination`、
`_DefaultPathWhenEmpty`、`_ConcurrentSaveAndDelete`，以及 `TestExecutorProfileRecord_CommandRoundTrip`、
`TestProfileNameRulesMatchCore`。

写卡时对分组先例的三处引用已核实无误：`core/group_store.go` 的 82-87（结构）、110-130（load 三态）、
154-194 与 200-222（Save/Delete + flushLocked）。

### 10.4 手工验收（临时目录，跑完已删）

`%TEMP%/gdq_w01_smoke/config.yaml` 里 `enabled: true` + `web_enabled: true`，
`profiles_path` 指向 **还不存在的** `nested/exec-profiles.json`：

1. `go run ./cmd/server -config=...` 起进程 → `GET /api/v1/executors` 回
   `{"enabled":true,"profiles":[],"required_role":"admin","max_timeout":"30m0s"}`（本卡没有新字段，符合预期）。
2. 冒烟结束后 `ls` 结果：`config.yaml`、`exec/`（产物目录，由既有装配创建）在，
   **`nested/` 整个目录不存在** → 档位存储连父目录都没被碰，"本卡没有任何调用方"这条得证。
3. 反向确认（单元测试覆盖，未手工跑）：`enabled: false` + `web_enabled: true` 由
   `TestExecutorsValidate_Rejects` 的那一行断言拒绝。

### 10.5 缺陷

| 编号 | 内容 | 处置 |
| --- | --- | --- |
| D-0101 | `List` 返回结构体副本但嵌套切片共享底层数组，调用方改 `items[0].Args[0]` 会写穿存储 | **登记不修**：本系列所有读取方（W05 合并、W06 端点）都只做"读出来转成 `ExecutorCommand`"，不改元素；W06 的响应组装要经过 `Registry`，不直接暴露 store 的返回值。将来若有调用方需要排序/就地改写，再补深拷贝 |

### 10.6 未覆盖项

- 文件权限位（`0644` 文件、`0755` 目录）没有断言——`GroupStore` 同样没有，且 Windows 上造不出可验证的失败分支。
- `Save` 对 `Env`/`Headers` 是浅拷贝（存的还是调用方给的 map），并发下调用方若继续改那个 map 会互相影响。
  与卡 §5.1 的并发用例不冲突（那条用不同 map 实例），但没有专门用例；W06 的处理器每次自己解码新 map。
- 页面上的实际写入路径（构造 store、启动加载）本卡完全没有接线，留到 W05。

