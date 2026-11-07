# Go Layout 重构任务卡(L01 … L03)

把仓库重构为标准 Go 工程布局并完成开源前置改造:module 改名 `github.com/jiujuan/godelayq`、
全部实现包下沉 `internal/`、web 收敛为纯前端。方案讨论与代价评估的结论沉淀在本文件,
执行细节拆成三张卡:L01(下沉与改名)→ L02(web 构建链与杂物)→ L03(文档与终验)。

## 决策记录

| # | 决策 | 理由 |
| --- | --- | --- |
| D1 | module 改为 `github.com/jiujuan/godelayq` | 开源硬前提:不可被 import 的模块无法注册 pkg.go.dev,也无法被 fork 后正常引用 |
| D2 | `core` 一并进 `internal/`,**不拆包、不留在根** | 开源形态定位为"可自托管的服务",不是"可 import 的库"。嵌入用法(demo1/demo2)保留为仓内参考;后路不锁死——未来要补库形态,把 core `git mv` 回根或另立 `pkg/client` SDK 卡即可 |
| D3 | 公开面 = 零个包 + `cmd/server` | 编译器强制外部 import 不到实现;`docs/design/` 历史 6 份设计文档不改,按归档口径 |
| D4 | Gin 纪律:gin 类型不出 `internal/api` | 未来 SDK 卡(`pkg/client`)只暴露 `net/http` 语义 |
| D5 | `web/` 纯前端;embed 包落 `internal/web/` | Go/Node 两条构建链边界清晰;产物经 vite `outDir` 重定向到 `internal/web/dist` |
| D6 | 唯一被批准的代码修改:`core/config_test.go` 两行相对路径 | 目录加深一层后 `../configs/` → `../../configs/`;已核查这是全仓唯一的目录深度依赖(其余 `../` 均为越界校验的测试夹具字符串) |

## 目标目录结构

```
github.com/jiujuan/godelayq
├── cmd/                            # server / gensecret / hashpassword(不动)
├── internal/                       # 全部实现,外部 import 不到
│   ├── core/                       # 62 文件原样下沉
│   ├── api/                        # Gin 层,handlers_*.go 按域拆的组织保留
│   ├── executor/
│   ├── store/                      # sqlite / observe / mgr,依赖红线守卫测试随迁
│   └── web/                        # embed_dashboard.go + embed_stub.go
├── examples/                       # demo1/demo2 保留(嵌入用法=仓内参考,README 注明)
│   └── demo2/job_queue/            # ← 根目录 job_queue/ 归位到这里
├── web/                            # 纯前端;build.outDir → ../internal/web/dist
├── configs/  docs/  scripts/
├── data/  logs/  .dev/  exec-workspace/   # 运行时产物,不动
├── LICENSE                         # 开源配套,本系列不含(独立卡)
└── .github/workflows/ci.yml        # 同上
```

## 执行顺序

| 卡 | 内容 | 该卡结束时可验证的能力 |
| --- | --- | --- |
| L01 | 五包下沉 `internal/`、module 改名、import 一条 sed 重写、config_test 路径修正 | `go build/vet/test ./...` 全绿,旧 import 路径全仓零残留 |
| L02 | vite 产物重定向 `internal/web/dist`、job_queue 归位、删 dashboard 占位页 | `-tags dashboard` 单二进制可构建、可起服务、控制台可开 |
| L03 | README/docs 路径与目录图同步、嵌入叙事改写、敏感信息审查、终验 | 文档零旧路径残留(历史 design 归档除外),全量回归通过 |

依赖关系:L01 → L02 → L03 严格串行,每张卡的提交必须自身全绿。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过;`-race` 视本机工具链可用性执行(与 sqlite 系列口径一致)。
- `gofmt -l` 因 CRLF 不可用(见 `../sqlite/README.md` 口径),只对实际改动的文件执行 `gofmt -w <file>`。
- `TestExampleConfigMatchesLocal`(`core/config_test.go`)的 `../configs/` 在 L01 随目录加深改为 `../../configs/`,该测试是 L01 全绿的必要条件之一。
- 每张卡完成后跑一次:`grep -r '"godelayq/' --include='*.go' .` 必须为零(L01 起)。
- 提交信息用 Conventional Commits,中文描述;本系列偏离"纯移动单独成提交"的惯例——移动与 import 重写必须同提交,否则中间提交编译必断(与"每卡提交全绿"冲突,取舍见 L01 实现记录)。
- 如需新增代码抽象:优先 struct;确有复用场景才用小 interface + struct,不为重构顺手造接口。
