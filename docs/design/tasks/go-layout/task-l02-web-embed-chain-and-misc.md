# TASK-L02　web 构建链迁移 + 杂物归位

- 所属阶段:第二步
- 依赖任务:L01
- 涉及文件:`web/vite.config.ts`、`web/.gitignore`、`internal/web/embed_*.go`(注释)、`internal/api/console.go`(注释)、`job_queue/`(移动)、`dashboard/index.html`(删除)
- 预计规模:小,约 0.5 人日

## 1. 任务目标

`web/` 成为纯前端目录(Go/Node 构建链彻底分离),单二进制构建链(`-tags dashboard`)在新位置可用;根目录杂物清零。

## 2. 背景与当前问题

L01 之后 embed 包已在 `internal/web/`,但 `//go:embed all:dist` 解析到的是 `internal/web/dist`,而 vite 仍输出到 `web/dist`,带 tag 的构建会因缺目录失败;根目录还留着示例任务目录(`job_queue/`)与零引用的占位页(`dashboard/index.html`,已核实全仓无代码引用)。

## 3. 要实现的功能

1. `web/vite.config.ts` 增加 `build: { outDir: '../internal/web/dist', emptyOutDir: true }`(outDir 在 root 外,vite 会告警,`emptyOutDir` 必须显式声明)。
2. `web/.gitignore` 的 dist 条目改指 `internal/web/dist`(或在根 `.gitignore` 落一条,以现有文件结构为准)。
3. `internal/web/embed_dashboard.go`、`embed_stub.go` 与 `internal/api/console.go` 注释里的路径提法同步(`web/dist` → `internal/web/dist`、构建顺序说明)。
4. `git mv job_queue examples/demo2/job_queue`(demo2 的 `Dir: "./job_queue"` 不改代码,运行目录语义变化记入文档,由 L03 承接)。
5. `git rm dashboard/index.html`。

## 4. 实现步骤

1. 先改 vite 配置与 ignore,跑 `cd web && npm run build` 确认产物落在 `internal/web/dist`。
2. 改三处注释。
3. 移动 job_queue、删除占位页。
4. 按 §7 验收。

## 5. 测试要求

- `npm run build` 成功且产物位置正确。
- `go build -tags dashboard ./cmd/server` 成功(此构建要求 dist 存在)。
- 不带 tag 的 `go build ./...` 与全量测试不受影响(stub 路径不依赖产物)。

## 6. 完成标准(DoD)

- [ ] `internal/web/dist` 存在且含 `index.html`,`web/dist` 不再产生
- [ ] `go build -tags dashboard ./cmd/server` 可构建
- [ ] 起服务后根路径返回控制台页面(HTTP 200,HTML)
- [ ] `web/` 下无 `.go` 文件;根目录无 `job_queue/`、无 `dashboard/`

## 7. 验收方式

```bash
cd web && npm run build && cd ..
go build -tags dashboard -o .dev/bin/godelayq-server-dash.exe ./cmd/server
# 起服务后:curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/   预期 200
git status --short   # job_queue rename、dashboard 删除可见
```

## 8. 不在本任务范围

- README 里 demo2 运行方式的说明 → L03
- 单二进制发布的 goreleaser/CI 配置 → 独立开源配套卡,不在本系列

## 9. 风险与回滚

- 风险:本机前端工具链不可用导致无法验证 embed 构建——降级口径:仅验证 `npm run build` 与 tag 构建其中之一可用即记录实际情况,不假装通过。
- 风险:vite `emptyOutDir` 漏配导致旧产物残留——验收时检查 `web/dist` 不再出现。
- 回滚:revert 本卡提交;job_queue 移动与占位页删除均为纯移动/删除,无数据迁移。

## 10. 实现记录

- 旧 `web/dist/`(迁移前的构建产物)在移除 web/.gitignore 的 `dist` 条目后暴露为 untracked,确认内容为旧 vite 产物后删除;新产物经 `npm run build`(4.4s)直落 `internal/web/dist/`。
- 冒烟结果(端口 8091,避开可能在跑的 8080 开发实例):`go build -tags dashboard` 成功;`GET /` 200(index.html,no-cache)、真实 `/assets/index-*.js|css` 200(immutable)、未认证 `GET /api/stats` 401、SPA 深链 `GET /login` 不带 `Accept: text/html` 时被鉴权拦下、带头时 200 回 index——与 `internal/api/console.go` 的设计逐条一致。冒烟实例已 taskkill 停止。
- 工具坑:Git Bash 的 MSYS 路径转换会把**以 `/` 开头的 grep 模式参数**转成 Windows 路径导致匹配错乱(表现为匹配结果被截断、后续 curl 拿到坏路径 000);模式写成不以 `/` 开头即可绕开。
- job_queue 三个示例文件 rename 100%;dashboard/index.html 删除(全仓零代码引用,执行前已复核)。
