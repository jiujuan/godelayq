# TASK-E08　参数校验与命令行参数生成

- 所属阶段：M2 进程执行
- 依赖任务：TASK-E02
- 涉及文件：新增 `executor/args.go`、`executor/args_test.go`
- 预计规模：中

## 1. 任务目标

把任务里的 `payload` 变成一份确定的启动参数列表：先检查提交的内容是否越界，再按档位的模板拼出 `argv`。这一步是"只能执行配置里写好的命令"这条规则真正落地的地方。

## 2. 背景与当前问题

设计文档 §2 的 D1、D2、D3 三条口径分别是：只允许档位、不接受源码、不经过 shell。前两条靠 E02 的启动校验保证；第三条要在此处保证：**拼出来的东西是 `[]string`，交给 `exec.Cmd` 的 argv，永远不拼成一条字符串再交给 shell 解析**。

现状里没有任何参数校验：`core/job.go` 的 `Payload` 是 `[]byte`，`api/handlers.go` 的 `createJobFromRequest` 只检查 JSON 能否绑定到 `CreateJobRequest`，业务内容由处理函数自己决定。

## 3. 要实现的功能

1. 提交侧的输入结构（只用来解码 payload，不持久化）：

   ```go
   type Submission struct {
       Args    map[string]string `json:"args,omitempty"`     // script/binary/http 共用
       Env     map[string]string `json:"env,omitempty"`      // 只有 profile.env_allow 里的键可注入
       Params  map[string]string `json:"params,omitempty"`   // http 的 URL 占位符
       Headers map[string]string `json:"headers,omitempty"`  // http，只有 header_allow 里的键
       Body    json.RawMessage   `json:"body,omitempty"`     // http
       Timeout string            `json:"timeout,omitempty"`  // 时长字符串，如 "90s"
   }
   ```

   解码要求：`DisallowUnknownFields`。也就是说 payload 里出现 `"cmd":"ls"`、`"script":"evil.sh"`、`"url":"http://internal"` 这类未声明的键，直接拒绝。
   `Args` 的值只接受字符串与数字两种 JSON 类型（数字转成字符串），其它类型（对象、数组、布尔、null）拒绝——布尔值很容易写成 `true`/`false` 看起来无害，但它意味着 payload 结构走偏，宁可让调用方写 `"on"`。
2. `func ValidateSubmission(p *Profile, payload []byte) (*Submission, error)`：
   - 空 payload 视为全默认值（只有当没有 `required` 参数时合法）。
   - 参数名必须已声明；缺 `required` 且无 `default` → 报错并列出缺哪个。
   - 每个值过 `Arg.Pattern`；未声明 `pattern` 时用默认安全集 `^[A-Za-z0-9._:/=,-]{1,256}$`（不含空格、不含 `-` 开头的第一个字符见下条、不含换行）。
   - **值不允许以 `-` 开头**，除非该参数在档位里显式声明 `allow_dash: true`。这条防的是参数值被解释成选项（例如把 `--level=-config=/tmp/x` 传进去）。`allow_dash` 是 E02 的 `Arg` 新增字段，本任务一并加。
   - 值长度上限 256，含控制字符（`\n`、`\r`、`\t`、`%00` 之类）一律拒绝。
   - `Env` 的键必须出现在 `profile.EnvAllow` 里；键名格式 `[A-Z0-9_]+`；值同样过控制字符检查；条数上限 16。
   - `Timeout`：解析失败报错；超过 `profile.Timeout`（或全局 `max_timeout`）时报错，不做静默夹取——提交时就告诉调用方上限是多少，比执行时被杀更好。
3. `func (p *Profile) Render(sub *Submission) (argv []string, err error)`：
   - `script`：`[程序路径, 脚本绝对路径, 渲染后的各项...]`。
   - `binary`：`[程序路径, fixed_args..., 渲染后的各项..., positional...]`。
   - `http`：不产生 argv（返回 `ErrWrongKind`，本 kind 由 E15 使用 `Render` 之外的路径）。
   - `args_render` 每项的 `{name}` 占位符替换为校验过的值；替换后若仍残留 `{` 或 `}`，报错（防止值里带大括号造成二次替换）。
   - 未使用的已声明参数不产生任何项（不补 `--x=`）。
   - `Positional`：`submission.args` 里保留键 `_positional`（字符串数组，最多 `max` 项，过 `pattern`），追加到 argv 末尾。用保留键是为了不给 `Submission` 再加一个字段，同时避免位置参数与具名参数混淆。
4. `func (p *Profile) EffectiveTimeout(requested time.Duration, cfg core.Config) time.Duration`：把档位超时、payload 超时、全局默认与上限合成一个值，保证结果永远大于 0（对应 D5："不填超时不代表无限")。
5. 错误信息规范：所有拒绝都带上"是哪个参数、为什么、允许什么"，例如
   `args.level: value "verbose" does not match pattern ^(info|debug)$`。
   这些字符串会直接出现在 HTTP 400 的 `details` 里（E16 使用），所以不要含敏感值原文：值如果来自被标记 `secret` 的参数，错误信息里用 `<redacted>` 替代。

## 4. 实现步骤

1. 加 `Submission` 与严格解码工具（一个 `decodeStrict([]byte, any) error`）。
2. 加 `Arg.AllowDash` 到 E02 的结构里，同步 E02 测试的默认集断言。
3. 写 `ValidateSubmission`，按"键名 → 必填 → 模式 → 长度/控制字符"四步分开。
4. 写 `Render`，三种 kind 分支；`http` 返回明确错误。
5. 写 `EffectiveTimeout`。
6. 加一条集成性质的表驱动测试，把设计文档 §5.2/§5.3 的两条示例档位与 payload 原样写进用例，断言 argv 逐元素相等。

## 5. 测试要求

`executor/args_test.go` 全部表驱动：

1. `TestValidateSubmission_UnknownKeys`：`cmd`、`script`、`url`、`program`、`shell` 五个越界键各一条 → 全部拒绝，且错误里指出键名。这条是 D1/D2 的核心守卫，注释里写明"这几条用例失败意味着白名单不起作用"。
2. `TestValidateSubmission_ValueTypes`：`args` 的值分别是 `true`、`null`、`{}`、`[]`、`123`、`"123"` → 前四种拒绝，后两种接受。
3. `TestValidateSubmission_DashPrefix`：默认拒绝 `-config=x`；声明 `allow_dash: true` 后接受。
4. `TestValidateSubmission_PatternAndRequired`：缺必填、不匹配模式、超长（>256）、含换行/制表/`\x00` → 各自拒绝且信息含参数名。
5. `TestValidateSubmission_Env`：不在 `env_allow` 的键、键名含小写或点、条数 17 → 拒绝；合法注入通过。
6. `TestValidateSubmission_SecretNotLeakedInError`：`secret: true` 的参数故意填错值 → 错误信息里不含该值原文，含 `<redacted>`。
7. `TestRender_Script` / `TestRender_Binary`：断言 argv 逐元素相等，特别是"含空格的值仍然是一个元素"（这是不经 shell 的直接证据）。
8. `TestRender_BraceInValue`：值 `{day}` → 报错不渲染。
9. `TestRender_HttpWrongKind` → `ErrWrongKind`。
10. `TestEffectiveTimeout`：四组输入（都为 0、payload 超上限、档位超全局上限、正常）断言结果与"永远大于 0"。
11. `TestRender_NoShellStringAnywhere`：一条防御性用例，断言 `Render` 的返回值类型是 `[]string` 且不存在把 argv `strings.Join` 后再拆开的代码路径（用注释与人工检查完成，测试里断言元素里若含空格则说明没有被拆分）。

## 6. 完成标准（DoD）

- [ ] payload 的任何未声明键都被拒绝，包括直接指定脚本路径、命令字符串、URL 三种最危险的写法，各有测试。
- [ ] `Render` 只产出 `[]string`，代码里没有任何 `sh -c`、`cmd /c`、字符串拼接后再解析的路径。
- [ ] 参数值以 `-` 开头默认被拒，例外必须显式声明。
- [ ] 超时永远大于 0，且提交期就会拒绝超上限的值。
- [ ] 错误信息可执行（指出参数名、原因、允许范围），并且对 `secret` 参数不泄漏原值。
- [ ] 设计文档 §5.2/§5.3 的两条示例作为用例存在并通过，保证文档与代码一致。

## 7. 验收方式

```bash
go test ./executor -run 'ValidateSubmission|Render|EffectiveTimeout' -v
go build ./... && go vet ./... && go test ./... -race
```

## 8. 不在本任务范围

- 不启动进程（E09）。
- 不做 HTTP 的 URL 模板渲染（E15，它需要主机与 IP 检查一起做）。
- 不在 API 层调用本卡的校验（E16 才接线到 `createJobFromRequest`）。本卡只做纯函数与测试，接线时另一张卡负责端到端。
- 不做参数值的加密或脱敏存储（明确不做，见设计文档 §8）。

## 9. 风险与回滚

- 风险：默认安全集里含 `/` 与 `:`，对路径类参数够用，但也允许 `--file=/etc/passwd` 这种"值本身是路径"的写法。真正的防线是档位脚本不解释参数为路径，这一点只能靠配置审查。缓解：在档位注释规范里建议对路径型参数显式写 `pattern`。
- 风险：`AllowDash` 加在 E02 的结构上会导致 E02 的测试改动，提交时把该改动放进本卡提交并在提交信息里说明，避免两个卡各自改同一结构造成冲突。
- 回滚：纯新增包，无外部影响，可单独 revert。

## 10. 实现记录（2026-09-30）

改动文件：新增 `executor/args.go`、`executor/args_test.go`；改 `executor/profile.go`
（拒绝把保留键声明成参数名、`env` 键折回大写）与 `executor/profile_test.go`
（既有用例改写 + 一条走配置文件的用例）。api 与 core 一行未改（§8 的口径：接线归 E16）。

### 与卡片的偏离与补充

1. **`Submission` 多了一个 `Positional []string` 字段**（卡片 §3.3 特意用保留键就是为了不加字段）。
   冲突点在卡片内部：§3.1 把 `Args` 的值类型限定成"字符串与数字"，而 §3.3 要位置参数是
   `args` 里的一个字符串数组。两者在同一张 map 上无法共存，所以**线上格式不变**
   （payload 仍写 `"args":{"_positional":["a.csv","b.csv"]}`），只在解码后的 Go 形态里单列一个字段。
   `fillValues` 会跳过这个键，档位里也不允许声明它（第 4 条）。
2. **顶层键的拒绝没有用 `DisallowUnknownFields`**，改成"先解成 map，再按白名单逐个筛键"。
   标准库报的是 `json: unknown field "cmd"`：键名在里面，但对调用方没有意义，也报不出
   "这个档位允许哪几个键"，而且一遇错就停。现在的错误是
   `payload key "cmd" is not accepted by profile "nightly_report" (allowed keys: args, env, timeout)`。
   §4.1 设想的 `decodeStrict([]byte, any)` 因此变成一个更小的工具：解码一个已经筛过键名的值。
3. **`AllowDash` 早在 E02 就落地了**（`core.ExecutorArg.AllowDash` 与 `executor.ArgSpec.AllowDash`），
   本卡没有再加字段，只补规则与用例（卡片 §3.2/§4.2 以为要在这里加）。§9 预判的"改动 E02 结构
   造成两张卡冲突"因此不存在，但第 6 条确实改了 E02 的函数，改动一并放进本卡提交。
4. **`checkArgs` 现在拒绝把 `_positional` 声明成参数名**：允许的话，一个数组值会撞上
   "值只能是字符串或数字"，错误信息看不出是被保留键拦下的。
5. **位置参数的减号开头没有例外**：档位结构里位置参数没有 `allow_dash` 字段，
   所以一律拒绝，并在错误里说明理由（`args._positional[0]: value "--curl" starts with "-", ...`）。
6. **档位 `env` 的键读进来时折回大写**（改 `buildProfile`）。冒烟跑真实 YAML 才暴露：
   viper 解码映射会把键统一变小写，`configs/config.example.yaml` 里示例写法
   `env: { REPORT_HOME: /srv/report }` 按原逻辑会以 `env key "report_home" must be upper-case`
   直接启动失败。折回大写只改变"本来会被拒的写法"，原先合法的写法不变；同时让 `env`（键）与
   `env_allow`（列表值，viper 不折叠）两侧大小写一致，"档位固定变量不许被 payload 覆盖"那条检查才成立。
   连带改了 E02 的一条用例（小写键被拒 → 减号键被拒）并新增 `TestLoadProfiles_ConfigFileEnvKeys`。
7. **模板引用了既无默认值也没被提供的参数时 `Render` 报错**，而不是拼出 `--day=`：
   卡片 §3.3 只写了反向情况（未使用的已声明参数不产生任何项）。空值参数在多数程序里表示
   "开关被关掉"或"路径为空"，静默传过去比拒绝更难解释。
8. **`argv[0]` 的写法固定**：script 用解释器名（探测得到的绝对路径留给 E09 替换）、
   workspace 内产物用绝对路径（子进程工作目录由 `cmd.Dir` 决定，相对路径会指向别处）、
   `runtime_allow` 里的程序名按原样给（`java` → `exec.Cmd` 自己走 PATH）。
9. **headers 与 body 的规则补齐**：headers 的键必须命中 `header_allow`（HTTP 头名不区分大小写，
   比对也不区分），条数沿用 env 的 16 条上限（卡片没给 headers 边界，一次提交不该没有上限）；
   body 只回答"档位让不让带体"，体的语义留给 E15。
10. **`ParseTimeout` 单独导出**（卡片未列）：提交期校验与 E09/E16 都要解析同一种写法，
    不留两份实现。超上限时报错不夹取（§3.2），错误里带上档位允许的值。
11. **卡片 §5.7 与 §5.11 合成一条用例** `TestRender_ValueStaysOneElement`：
    既断言含空格的值仍是单独一个元素，也断言 argv 里没有 `sh -c`/`cmd /c` 之类的形态。
12. **越界键清单扩到 7 个**（卡片列 5 个，另加 `runtime`、`argv`），并断言错误里出现键名本身。

### 验证结果

- 单元测试：`executor/args_test.go` 14 个测试函数（含子测试），§5 要求的 11 项全部覆盖。
  其中三条是白名单本身的守卫，注释里写明"这几条失败意味着 D1/D2 不起作用"：
  顶层越界键、`args` 里未声明的键、`args` 的值类型（布尔/null/对象/数组全拒）。
- 设计文档 §5.2/§5.3 的两条示例档位原样进用例（DoD 最后一条）：
  `node <workspace>/scripts/report.mjs --day=yesterday --level=debug` 与
  `<workspace>/bin/etl --window=20260929 a.csv b.csv`、`java -jar app/app.jar` 都逐元素断言相等；
  §5.2 的示例 payload 带 `env`，而那条档位没声明 `env_allow`，用例断言它被拒——
  文档与代码在同一处口径上对齐。
- `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿
  （api 41s / core 8s / cmd 5s / executor 1.7s）；linux、darwin 交叉编译与 `-tags dashboard` 通过；
  新增与改动文件 `gofmt -l` 无输出。
- 真实链路冒烟（系统临时目录里一份 YAML → `core.LoadConfig` → `LoadProfiles` →
  `ValidateSubmission` → `Render`，跑完已删除，仓库 `configs/`、`data/` 未被写入）。
  五条档位全部加载；七条应通过的 payload 打出 argv，例如
  `{"args":{"window":"20260929","_positional":["input/a.csv","b.csv"]}}` →
  `[<workspace>\bin\etl --window=20260929 input/a.csv b.csv]`（4 个元素，路径与参数分列）；
  二十四条应被拒的 payload 各得一句可执行的错误，摘几条：
  `{"cmd":"ls"}` → `payload key "cmd" is not accepted by profile "nightly_report" (allowed keys: args, env, timeout)`；
  `{"args":{"day":true}}` → `args.day: value type is not accepted: use a string or a number, got a boolean`；
  `{"args":{"token":"0000000"}}`（secret）→ `args.token: value "<redacted>" does not match pattern ^[a-f0-9]{8}$`；
  `{"args":{"day":"today"},"env":{"REPORT_HOME":"/tmp"}}` → `env.REPORT_HOME: fixed by profile ... cannot be overridden`；
  `{"args":{"day":"today"},"timeout":"4h"}` → `timeout 4h0m0s exceeds the 10m0s allowed by profile ...`；
  两个对象拼在一起 → `payload must be one JSON object`。
  生效超时四组：`10m0s`（档位值）、`1m30s`（payload 值）、`30m0s`（超全局上限被夹住）、`5m0s`（全局默认）。
- 冒烟顺带暴露两件事，都已处置或登记：第 6 条的 viper 键名折叠（已修）；
  YAML 单引号里的 `pattern` 若写成 `'\d'`，档位能正常加载但正则永远不匹配
  ——`checkArgs` 只保证"能编译"，不保证写法意图。建议 E19 在文档里点一句"正则用单引号、单反斜杠"。

### 未验证

- `Render` 的 argv 交给 `exec.Cmd` 之后的行为要等 E09（本卡不启动任何进程，§8）。
- http 档位的 `params` → URL 渲染、headers/body 的实际语义归 E15。
- 本卡没有 HTTP 侧可观测的现象：`ValidateSubmission` 还没被 api 调用（接线归 E16），
  所以 400 响应里的 details 文本此刻只能在库层面看到。

### 留给后续卡片的接口形状

- E09：`sub, err := ValidateSubmission(profile, job.Payload)` → `argv, err := profile.Render(sub)` →
  `timeout := profile.EffectiveTimeout(mustParseTimeout(sub.Timeout), cfg)`；
  `submission.Env` 是 payload 请求注入的变量，与 `profile.Env` 合并时档位固定值优先（已在 E08 挡住覆盖）。
- E15：http 档位用 `sub.Params`（已过 `args` 声明的正则）渲染 `URLTemplate`，
  `sub.Headers` 已过 `header_allow`，`sub.Body` 是档位允许形态下的请求体原文。
- E16：400 的 `details` 直接用这里返回的错误文本；secret 参数的值不会出现在文本里。
