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
