#Requires -Version 5.1
<#
.SYNOPSIS
    godelayq 本地开发环境的前后端进程管理脚本（Windows PowerShell 5.1+）。

.DESCRIPTION
    管理的两个进程：
      backend  —— go build 出 .dev\bin\godelayq-server.exe，工作目录固定在仓库根再启动。
                  configs/config.yaml 里的 ./data、./exec-workspace 都是相对路径，
                  工作目录不对就会在别处凭空建一套 data 目录。
      frontend —— node web\node_modules\vite\bin\vite.js，等价于 web 目录下的 npm run dev。

    运行记录（进程号、实际端口、启动时间、日志路径）写在 .dev\<service>.json，
    日志写在 .dev\logs\。整个 .dev 目录由 .dev\.gitignore 忽略。

    端口占用分析：配置端口 -> 该端口上的监听表项 -> 监听者是哪些真实进程。
    表项里的绑定地址决定可用范围：Go 侧的 :::8080 是双栈（IPv4 与 IPv6 都收），
    Vite 默认只绑 ::1（只有 IPv6 回环能用 localhost 连到它）。

    注意：部分 Windows 版本/更新后，TCP 表对 :: 双栈监听行整行查不到
    （2026-10 的更新后本机实测如此）。脚本对此的对策：等待监听时表查不到
    就直接探回环口；vite 端口认不到就从它的 Local: 日志行解析；
    启动前"表里没行但端口能连上"按占用处理并说明，不再盲起进程去撞 bind。

    Vite 在配置端口被占用时会自动顺延（vite.config.ts 未设 strictPort），
    因此前端实际端口取自它自己打印的 "Local:" 一行，而不是配置文件里的数字。

    后端只走优雅关停：Stop-Process 是 TerminateProcess，调度器与 store 的收尾都不会执行。
    脚本改为在独立进程里 AttachConsole 目标进程、发 CTRL_BREAK_EVENT，
    让 cmd/server/main.go 的 signal.Notify(SIGINT/SIGTERM) 收到 os.Interrupt 后自行退出。
    等待超时会报错并保留运行记录，不代为强杀。

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev.ps1 start
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev.ps1 start backend
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev.ps1 status
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev.ps1 ports
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev.ps1 logs frontend -Follow
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('start', 'stop', 'restart', 'status', 'list', 'ports', 'logs', 'help')]
    [string]$Command = 'help',

    [Parameter(Position = 1)]
    [string]$Target = 'all',

    # 本次启动临时覆盖端口：后端转成环境变量 GODELAYQ_SERVER_PORT，前端传给 vite --port。
    # 不写配置文件，也不影响下一次运行。
    [int]$BackendPort = 0,
    [int]$FrontendPort = 0,

    # 后端改用 -tags dashboard 的单二进制形态（需先 cd web; npm run build）。
    # 该形态只有一个端口（8080），因此不再起 Vite。
    [switch]$Dashboard,

    # 单二进制产物名。
    [string]$ConsoleBin = 'godelayq-console.exe',

    # 复用 .dev\bin 里已有的产物，跳过 go build。
    [switch]$SkipBuild,

    # 等待优雅退出的秒数。
    [int]$StopTimeout = 20,

    # logs 子命令。
    [switch]$Follow,
    [int]$Tail = 40,

    # 内部用途：独立进程里给目标进程发 CTRL_BREAK_EVENT（见 docs/脚本说明顶部）。
    [int]$SignalPid = 0
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# ---------------------------------------------------------------- 路径与常量

$RepoRoot = Split-Path -Parent $PSScriptRoot
$DevDir = Join-Path $RepoRoot '.dev'
$LogDir = Join-Path $DevDir 'logs'
$BinDir = Join-Path $DevDir 'bin'
$ServerBin = Join-Path $BinDir 'godelayq-server.exe'
$ConsolePath = Join-Path $BinDir $ConsoleBin
$ConfigFile = Join-Path $RepoRoot 'configs\config.yaml'
$ViteDir = Join-Path $RepoRoot 'web'
$ViteEntry = Join-Path $ViteDir 'node_modules\vite\bin\vite.js'

$DefaultBackendPort = 8080
$DefaultFrontendPort = 5177
$ViteProbeCount = 8
$HealthPath = '/api/v1/health'

# ---------------------------------------------------------------- 输出助手

function Write-Head { param([string]$Text) Write-Host ''; Write-Host "== $Text" -ForegroundColor Cyan }
function Write-Line { param([string]$Text) Write-Host "   $Text" }
function Write-Key {
    param([string]$Name, [string]$Value, [string]$Color = 'Gray')
    Write-Host ('   {0,-16}' -f $Name) -NoNewline
    Write-Host $Value -ForegroundColor $Color
}

function Get-Prop {
    param([object]$Obj, [string]$Name, [string]$Default = '')
    if ($null -eq $Obj) { return $Default }
    if ($Obj.PSObject.Properties.Name -contains $Name) {
        $v = $Obj.$Name
        if ($null -eq $v) { return $Default }
        return [string]$v
    }
    return $Default
}

# ---------------------------------------------------------------- 运行记录

function Get-RecordPath { param([string]$Service) Join-Path $DevDir ($Service + '.json') }

function Read-Record {
    param([string]$Service)
    $path = Get-RecordPath -Service $Service
    if (-not (Test-Path -LiteralPath $path)) { return $null }
    try {
        return (Get-Content -LiteralPath $path -Raw -Encoding UTF8 | ConvertFrom-Json)
    } catch {
        Write-Line "$path 读不出来（$($_.Exception.Message)），本次按无记录处理。"
        return $null
    }
}

function Write-Record {
    param([string]$Service, [hashtable]$Fields)
    New-Item -ItemType Directory -Force -Path $DevDir | Out-Null
    ($Fields | ConvertTo-Json -Depth 4) | Set-Content -LiteralPath (Get-RecordPath -Service $Service) -Encoding UTF8
}

function Remove-Record {
    param([string]$Service)
    Remove-Item -LiteralPath (Get-RecordPath -Service $Service) -Force -ErrorAction SilentlyContinue
}

function Test-ProcessAlive {
    param([int]$TargetPid)
    if ($TargetPid -le 0) { return $false }
    $p = Get-Process -Id $TargetPid -ErrorAction SilentlyContinue
    if (-not $p) { return $false }
    return (-not $p.HasExited)
}

# ---------------------------------------------------------------- 端口分析

function Get-ListenRows {
    param([int]$Port)
    # 无监听时 Get-NetTCPConnection 报错，静默成空数组
    $rows = @(Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue)
    $seen = @{}
    $out = @()
    foreach ($r in $rows) {
        $key = "$($r.LocalAddress)|$($r.LocalPort)|$($r.OwningProcess)"
        if ($seen.ContainsKey($key)) { continue }
        $seen[$key] = $true
        $out += [pscustomobject]@{ Address = $r.LocalAddress; Port = [int]$r.LocalPort; Pid = [int]$r.OwningProcess }
    }
    return $out
}

function Describe-Process {
    param([int]$TargetPid)
    $info = @{ Name = '(进程已退出)'; Path = ''; Cmd = '' }
    if ($TargetPid -le 0) {
        $info.Name = 'System (0)'
        return $info
    }
    $proc = Get-Process -Id $TargetPid -ErrorAction SilentlyContinue
    if (-not $proc) { return $info }
    $info.Name = $proc.ProcessName
    try { $info.Path = $proc.Path } catch { $info.Path = '' }
    try {
        $ci = Get-CimInstance -ClassName Win32_Process -Filter "ProcessId=$TargetPid" -ErrorAction Stop
        $info.Cmd = [string]$ci.CommandLine
    } catch { $info.Cmd = '' }
    return $info
}

function Format-Command {
    param([string]$Cmd, [int]$Width = 90)
    if ([string]::IsNullOrWhiteSpace($Cmd)) { return '(取不到命令行)' }
    if ($Cmd.Length -le $Width) { return $Cmd }
    return $Cmd.Substring(0, $Width) + '...'
}

function Get-StackNote {
    param([array]$Rows)
    if (@($Rows).Count -eq 0) { return '无监听表项' }
    $map = @{
        '::'      = ':: 双栈（IPv4 与 IPv6 都能连）'
        '0.0.0.0' = '0.0.0.0 只收 IPv4'
        '::1'     = '::1 只收 IPv6 回环'
        '127.0.0.1' = '127.0.0.1 只收 IPv4 回环'
    }
    $notes = @($Rows | ForEach-Object {
        if ($map.ContainsKey($_.Address)) { $map[$_.Address] } else { $_.Address }
    } | Sort-Object -Unique)
    return ($notes -join '; ')
}

function Analyze-Port {
    param(
        [int]$Port,
        [int[]]$OwnPids = @()
    )
    $rows = @(Get-ListenRows -Port $Port)
    $foreign = @()
    $managed = @()
    foreach ($procId in ($rows | ForEach-Object { $_.Pid } | Sort-Object -Unique)) {
        if ($OwnPids -contains [int]$procId) {
            $managed += [pscustomobject]@{ Pid = [int]$procId; Info = (Describe-Process -TargetPid ([int]$procId)) }
        } else {
            $foreign += [pscustomobject]@{ Pid = [int]$procId; Info = (Describe-Process -TargetPid ([int]$procId)) }
        }
    }
    $verdict = 'free'
    if ($rows.Count -gt 0) {
        if ($foreign.Count -gt 0) { $verdict = 'foreign' } else { $verdict = 'managed' }
    }
    return [pscustomobject]@{
        Port        = $Port
        Rows        = $rows
        Foreign     = $foreign
        Managed     = $managed
        ManagedPids = @($managed | ForEach-Object { $_.Pid })
        Verdict     = $verdict
        Stack       = Get-StackNote -Rows $rows
        V4          = Test-TcpPort -Port $Port
    }
}

function Show-PortBlock {
    param([string]$Title, [object]$Analysis)
    $head = ('   {0,-18} 端口 {1}' -f $Title, $Analysis.Port)
    $tail = ('  [{0}]  127.0.0.1 探测={1}' -f $Analysis.Stack, $(if ($Analysis.V4) { '通' } else { '空' }))
    switch ($Analysis.Verdict) {
        'free' {
            Write-Host $head -NoNewline
            if ($Analysis.V4) {
                # 表里没有 127.0.0.1 的项但能连进去：多半是 :: 双栈表项被系统折叠，不是空闲
                Write-Host ('   [无监听表项，但能连上]' + $tail) -ForegroundColor DarkYellow
            } else {
                Write-Host '   [空闲]' -ForegroundColor Green
            }
            return
        }
        'managed' { Write-Host $head -NoNewline; Write-Host ('   [本项目进程]' + $tail) -ForegroundColor DarkCyan }
        default {
            Write-Host $head -NoNewline
            Write-Host ('   [被 {0} 个外部进程占用]' -f $Analysis.Foreign.Count) -ForegroundColor Red
            Write-Line $Analysis.Stack
        }
    }
    foreach ($row in $Analysis.Rows) {
        Write-Line ('监听 {0}:{1}  pid={2}' -f $row.Address, $row.Port, $row.Pid)
    }
    foreach ($item in @($Analysis.Foreign) + @($Analysis.Managed)) {
        $mark = if ($Analysis.ManagedPids -contains $item.Pid) { '本项目' } else { '外部' }
        $line = 'pid {0}  [{1}]  {2}' -f $item.Pid, $mark, $item.Info.Name
        if ($item.Info.Path) { $line += ('  <- ' + $item.Info.Path) }
        Write-Line $line
        Write-Line ('      cmd: ' + (Format-Command -Cmd $item.Info.Cmd))
    }
}

function Test-TcpPort {
    param([int]$Port, [string]$Address = '127.0.0.1', [int]$TimeoutMs = 300)
    # 只探 IPv4 回环：IPv6 字面量走这个重载会被当成 DNS 名解析失败，
    # 栈覆盖情况由监听表（Get-StackNote）给出。
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $task = $client.BeginConnect($Address, $Port, $null, $null)
        if (-not $task.AsyncWaitHandle.WaitOne($TimeoutMs)) { return $false }
        $client.EndConnect($task)
        return $client.Connected
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

function Invoke-HealthProbe {
    param([int]$Port)
    $url = ('http://127.0.0.1:{0}{1}' -f $Port, $HealthPath)
    try {
        $resp = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 3 -ErrorAction Stop
        return [pscustomobject]@{ Code = [int]$resp.StatusCode; Detail = '' }
    } catch {
        $code = 0
        $resp = $_.Exception.Response
        if ($resp -and $resp.StatusCode) { $code = [int]$resp.StatusCode }
        return [pscustomobject]@{ Code = $code; Detail = $_.Exception.Message }
    }
}

# ---------------------------------------------------------------- 配置读取

function Get-ConfiguredBackendPort {
    param([int]$Override)
    if ($Override -gt 0) { return [int]$Override }
    if ($env:GODELAYQ_SERVER_PORT -match '^\d+$') { return [int]$env:GODELAYQ_SERVER_PORT }
    if (Test-Path -LiteralPath $ConfigFile) {
        $txt = Get-Content -LiteralPath $ConfigFile -Raw -Encoding UTF8
        $m = [regex]::Match($txt, '(?m)^server:[\s\S]*?^\s{2}port:\s*"?(\d+)"?')
        if ($m.Success) { return [int]$m.Groups[1].Value }
    }
    return $DefaultBackendPort
}

function Get-ConfiguredFrontendPort {
    param([int]$Override)
    if ($Override -gt 0) { return [int]$Override }
    if ($env:DEV_FRONTEND_PORT -match '^\d+$') { return [int]$env:DEV_FRONTEND_PORT }
    $viteFile = Join-Path $ViteDir 'vite.config.ts'
    if (Test-Path -LiteralPath $viteFile) {
        $txt = Get-Content -LiteralPath $viteFile -Raw -Encoding UTF8
        $m = [regex]::Match($txt, '(?m)^\s*port:\s*(\d+)')
        if ($m.Success) { return [int]$m.Groups[1].Value }
    }
    return $DefaultFrontendPort
}

# ---------------------------------------------------------------- 优雅信号

Add-Type -Namespace QDev -Name ConsoleCtrl -MemberDefinition @'
[DllImport("kernel32.dll", SetLastError = true)]
public static extern bool AttachConsole(uint dwProcessId);
[DllImport("kernel32.dll", SetLastError = true)]
public static extern bool FreeConsole();
[DllImport("kernel32.dll")]
public static extern bool SetConsoleCtrlHandler(IntPtr handlerRoutine, bool add);
[DllImport("kernel32.dll", SetLastError = true)]
public static extern bool GenerateConsoleCtrlEvent(uint dwCtrlEvent, uint dwProcessId);
'@ -ErrorAction SilentlyContinue

function Send-BreakInThisProcess {
    param([int]$TargetPid)
    # 顺序要紧：先 detach 本进程的 console，才能 attach 目标进程那个 console。
    # SetConsoleCtrlHandler(NULL,$false) 想让自己免疫这次事件，实测拦不住
    # helper 被带走（退出码 0xC000013A），所以那一侧的判定见 Send-BreakToConsoleOwner。
    [void][QDev.ConsoleCtrl]::FreeConsole()
    if (-not [QDev.ConsoleCtrl]::AttachConsole([uint32]$TargetPid)) { return 3 }
    [void][QDev.ConsoleCtrl]::SetConsoleCtrlHandler([IntPtr]::Zero, $false)
    # 1 = CTRL_BREAK_EVENT；进程号传 0 表示送达该 console 上的所有进程组
    $ok = [QDev.ConsoleCtrl]::GenerateConsoleCtrlEvent(1, 0)
    if (-not $ok) { return 4 }
    return 0
}

function Send-BreakToConsoleOwner {
    param([int]$TargetPid)
    # 必须另起一个进程去做那三步：在脚本自己的进程里做会把用户当前终端的 console 摘掉。
    $p = Start-Process -FilePath 'powershell.exe' `
        -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $PSCommandPath, '-SignalPid', $TargetPid) `
        -WindowStyle Hidden -PassThru -Wait
    $code = [int]$p.ExitCode
    # -1073741510 = 0xC000013A（STATUS_CONTROL_C_EXIT）：helper 自己也被这次事件带走，
    # 恰好说明事件已经送达那个 console。目标进程有没有真退出由 Stop-Service 判定。
    if ($code -eq -1073741510) { return 0 }
    return $code
}

# ---------------------------------------------------------------- 启动

function Start-Backend {
    param([int]$Port, [switch]$UseDashboard, [int]$OverridePort = 0)

    $bin = if ($UseDashboard) { $ConsolePath } else { $ServerBin }
    $record = Read-Record -Service 'backend'
    # 运行记录优先于端口表项：一个在跑的 backend 无论端口对错都占着同一份 ./data，
    # 再起一个只会让两个进程互相踩快照。pid 活着且形态相同就直接认定"已在运行"，
    # 不再依赖端口表——TCP 表看不见 :: 双栈监听行的环境下，端口分析根本找不到它。
    if ($record) {
        $recPid = 0
        [void][int]::TryParse((Get-Prop $record 'pid'), [ref]$recPid)
        if (Test-ProcessAlive -TargetPid $recPid) {
            if ((Get-Prop $record 'command') -ne $bin) {
                throw ('backend 已在运行（pid {0}，端口 {1}，形态 {2}），换端口或换 -Dashboard 形态请先 stop。' -f $recPid, (Get-Prop $record 'port'), (Get-Prop $record 'command'))
            }
            Write-Line ('backend 已在运行（pid {0}，端口 {1}），本次不重复启动。' -f $recPid, (Get-Prop $record 'port'))
            return
        }
    }
    $ownPids = @(Get-ManagedPids -Service 'backend')

    $analysis = Analyze-Port -Port $Port -OwnPids $ownPids
    if ($analysis.Verdict -eq 'foreign') {
        Show-PortBlock -Title 'backend' -Analysis $analysis
        throw ('backend 需要的端口 {0} 被外部进程占用：先处理它，或用 -BackendPort 换一个端口。' -f $Port)
    }
    if ($analysis.Verdict -eq 'free' -and $analysis.V4) {
        # 表里没有监听行但端口能连上：本机 TCP 表看不见这类监听行（:: 双栈行在某些
        # Windows 版本/更新后查不到）。占用者是谁无法确认，盲目再起一个只会撞 bind，
        # 然后留下一个没有运行记录的孤儿——拒绝并说清处置办法。
        Show-PortBlock -Title 'backend' -Analysis $analysis
        $msg = ('端口 {0} 能连通但监听表里查不到占用者（本机 TCP 表看不见这类监听行）。' -f $Port)
        $msg += '若是上次没记上账的 godelayq-server，先结束该进程再 start；或用 -BackendPort 换端口。'
        throw $msg
    }

    $running = @($analysis.Managed | Where-Object { Test-ProcessAlive -TargetPid $_.Pid })
    if ($running.Count -gt 0) {
        # Describe-Process 交回的是哈希表，按键取而不是走属性适配器
        $same = @($running | Where-Object { $_.Info['Path'] -eq $bin })
        if ($same.Count -gt 0) {
            Write-Line ('backend 已在运行（pid {0}，端口 {1}），本次不重复启动。' -f $same[0].Pid, $Port)
            return
        }
        throw ('端口 {0} 上是本项目的另一种形态（pid {1}，{2}），切换 -Dashboard 需要先 stop。' -f $Port, $running[0].Pid, $running[0].Info['Path'])
    }

    if (-not $SkipBuild) {
        New-Item -ItemType Directory -Force -Path $BinDir, $LogDir | Out-Null
        $buildArgs = @('build')
        if ($UseDashboard) { $buildArgs += @('-tags', 'dashboard') }
        $buildArgs += @('-o', $bin, './cmd/server')
        Write-Line ('go ' + ($buildArgs -join ' '))
        $output = & go @buildArgs 2>&1
        if ($LASTEXITCODE -ne 0) {
            $output | ForEach-Object { Write-Host ('     ' + $_) -ForegroundColor DarkRed }
            throw ('go build 失败，退出码 {0}。' -f $LASTEXITCODE)
        }
    }
    if (-not (Test-Path -LiteralPath $bin)) {
        throw ('找不到后端产物 {0}，去掉 -SkipBuild 重新 start。' -f $bin)
    }

    $logPath = Join-Path $LogDir 'backend.log'
    $errPath = Join-Path $LogDir 'backend.err.log'
    New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
    Set-Content -LiteralPath $logPath -Value '' -Encoding UTF8 -NoNewline
    Set-Content -LiteralPath $errPath -Value '' -Encoding UTF8 -NoNewline

    $procArgs = @()
    if (Test-Path -LiteralPath $ConfigFile) {
        $procArgs += ('-config=' + (Resolve-Path -LiteralPath $ConfigFile).Path)
    }
    # 只在显式给了 -BackendPort 时用环境变量覆盖；否则交给配置文件
    $savedEnv = $env:GODELAYQ_SERVER_PORT
    if ($OverridePort -gt 0) { $env:GODELAYQ_SERVER_PORT = [string]$OverridePort }
    try {
        # 独立（隐藏）console：优雅停止靠的就是这个 console 归属
        $p = Start-Process -FilePath $bin -ArgumentList $procArgs -WorkingDirectory $RepoRoot `
            -WindowStyle Hidden -RedirectStandardOutput $logPath -RedirectStandardError $errPath -PassThru
    } finally {
        if ($OverridePort -gt 0) {
            if ($null -eq $savedEnv) { Remove-Item Env:\GODELAYQ_SERVER_PORT -ErrorAction SilentlyContinue }
            else { $env:GODELAYQ_SERVER_PORT = $savedEnv }
        }
    }

    $listenPid = $p.Id
    $actual = Wait-ForListen -Port $Port -OwnPid $listenPid
    if ($actual -le 0) {
        $alive = Test-ProcessAlive -TargetPid $listenPid
        if (-not $alive) { throw ('backend（pid {0}）启动后退出，见日志 {1}' -f $listenPid, $logPath) }
        throw ('backend（pid {0}）6 秒内没有监听端口 {1}，见日志 {2}' -f $listenPid, $Port, $logPath)
    }
    if ($actual -ne $Port) {
        Write-Line ('（请求端口 {0}，实际监听 {1}：看是 configs\config.yaml 的 server: port 还是 GODELAYQ_SERVER_PORT 在生效）' -f $Port, $actual)
    }
    Write-Record -Service 'backend' -Fields @{
        service   = 'backend'
        pid       = $listenPid
        command   = $bin
        args      = ($procArgs -join ' ')
        port      = $actual
        url       = ('http://localhost:{0}' -f $actual)
        dashboard = [bool]$UseDashboard
        startedAt = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss')
        log       = $logPath
    }
    Write-Key 'backend 启动' ('pid {0}  端口 {1}  http://localhost:{1}' -f $listenPid, $actual) 'Green'
}

function Wait-ForListen {
    param([int]$Port, [int]$OwnPid, [int]$Tries = 20)
    for ($i = 0; $i -lt $Tries; $i++) {
        if (-not (Test-ProcessAlive -TargetPid $OwnPid)) { return -1 }
        $ports = @(Get-OwnListenPorts -OwnPid $OwnPid)
        if ($ports -contains $Port) { return $Port }
        # 进程活着却绑在别的端口（配置与环境变量各说各话）：如实交回，不在这里替它编理由
        if ($ports.Count -gt 0) { return $ports[0] }
        # TCP 表查不到监听行时的兜底：本机 TCP 表对 :: 双栈监听行可能整行不可见
        # （2026-10 的 Windows 更新后实测如此，Go 服务绑的 [::]:8080 查不到）。
        # 直接探回环口；探到后再等一拍确认进程没死——端口真被外部占用时，
        # bind 冲突的进程会在这 400ms 里退出，那样交回 -1 走"启动后退出"的报错。
        if (Test-TcpPort -Port $Port) {
            Start-Sleep -Milliseconds 400
            if (-not (Test-ProcessAlive -TargetPid $OwnPid)) { return -1 }
            return $Port
        }
        Start-Sleep -Milliseconds 300
    }
    return 0
}

function Remove-Ansi {
    param([string]$Text)
    if ([string]::IsNullOrEmpty($Text)) { return '' }
    return ($Text -replace '\x1b\[[0-9;?]*[ -/]*[@-~]', '')
}

function Get-OwnListenPorts {
    param([int]$OwnPid)
    # 一次查询拿全某个进程的所有监听端口：逐端口问 Get-NetTCPConnection 每轮要发八次 CIM，
    # 等待循环会被拖到分钟级。
    @(Get-NetTCPConnection -State Listen -OwningProcess $OwnPid -ErrorAction SilentlyContinue |
        ForEach-Object { [int]$_.LocalPort } | Sort-Object -Unique)
}

function Find-OwnListenPort {
    param([int]$BasePort, [int]$OwnPid, [int]$Span)
    # Vite 占用时会顺延，但顺延后的端口一定是它自己那个 pid 在监听，
    # 所以按 pid 扫这一段比解析它的彩色输出可靠。
    $ports = @(Get-OwnListenPorts -OwnPid $OwnPid)
    foreach ($candidate in $ports) {
        if ($candidate -ge $BasePort -and $candidate -lt ($BasePort + $Span)) { return $candidate }
    }
    if ($ports.Count -gt 0) { return $ports[0] }
    return 0
}

function Get-VitePortFromLog {
    param([string]$LogPath)
    # TCP 表查不到监听行时的兜底：Vite 自己打印的 Local: 行就是实际端口
    # （启动时设了 NO_COLOR=1，日志里没有 ANSI 转义码）。
    if (-not (Test-Path -LiteralPath $LogPath)) { return 0 }
    $hit = Select-String -LiteralPath $LogPath -Pattern 'Local:\s*https?://localhost:(\d+)' -ErrorAction SilentlyContinue |
        Select-Object -Last 1
    if ($hit) { return [int]$hit.Matches[0].Groups[1].Value }
    return 0
}

function Start-Frontend {
    param([int]$Port)

    if (-not (Test-Path -LiteralPath $ViteEntry)) {
        throw ('找不到 {0}，先在 web 目录执行 npm install。' -f $ViteEntry)
    }
    $ownPids = @(Get-ManagedPids -Service 'frontend')
    $record = Read-Record -Service 'frontend'
    if ($record) {
        $recPid = 0
        [void][int]::TryParse((Get-Prop $record 'pid'), [ref]$recPid)
        if (Test-ProcessAlive -TargetPid $recPid) {
            Write-Line ('frontend 已在运行（pid {0}，端口 {1}），换端口请先 stop。' -f $recPid, (Get-Prop $record 'port'))
            return
        }
    }
    $running = @((Analyze-Port -Port $Port -OwnPids $ownPids).Managed | Where-Object { Test-ProcessAlive -TargetPid $_.Pid })
    if ($running.Count -gt 0) {
        Write-Line ('frontend 已在运行（pid {0}，端口 {1}），本次不重复启动。' -f $running[0].Pid, $Port)
        return
    }
    # Vite 会自动顺延，所以配置端口被外部占用不拦启动，只说明清楚
    $analysis = Analyze-Port -Port $Port -OwnPids $ownPids
    if ($analysis.Verdict -eq 'foreign') {
        Show-PortBlock -Title 'frontend' -Analysis $analysis
        Write-Line 'Vite 未设 strictPort，会自动顺延到下一个空闲端口，继续启动。'
    }

    New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
    $logPath = Join-Path $LogDir 'frontend.log'
    $errPath = Join-Path $LogDir 'frontend.err.log'
    Set-Content -LiteralPath $logPath -Value '' -Encoding UTF8 -NoNewline
    Set-Content -LiteralPath $errPath -Value '' -Encoding UTF8 -NoNewline

    $node = Get-Command node -ErrorAction SilentlyContinue
    if (-not $node) { throw 'PATH 里找不到 node。' }
    # 括号不能省：-f 的优先级低于逗号，不括起来整条数组会被当成格式化参数，
    # 结果是 --port 根本没传给 Vite。
    $viteArgs = @(('"{0}"' -f $ViteEntry), '--port', [string]$Port)
    # 请求无彩色输出，日志文件里就不会夹 ANSI 转义码（NO_COLOR 约定）
    $savedNoColor = $env:NO_COLOR
    $env:NO_COLOR = '1'
    try {
        $p = Start-Process -FilePath $node.Source -ArgumentList $viteArgs -WorkingDirectory $ViteDir `
            -WindowStyle Hidden -RedirectStandardOutput $logPath -RedirectStandardError $errPath -PassThru
    } finally {
        if ($null -eq $savedNoColor) { Remove-Item Env:\NO_COLOR -ErrorAction SilentlyContinue }
        else { $env:NO_COLOR = $savedNoColor }
    }

    $actual = 0
    for ($i = 0; $i -lt 80; $i++) {
        if (-not (Test-ProcessAlive -TargetPid $p.Id)) { break }
        $actual = Find-OwnListenPort -BasePort $Port -OwnPid $p.Id -Span $ViteProbeCount
        # 表里查不到监听行（:: 行不可见的环境）就从它自己的日志里认端口
        if ($actual -le 0) { $actual = Get-VitePortFromLog -LogPath $logPath }
        if ($actual -gt 0) { break }
        Start-Sleep -Milliseconds 250
    }
    if ($actual -le 0) {
        $why = if (-not (Test-ProcessAlive -TargetPid $p.Id)) { '进程已退出' } else { ('20 秒内没有在 {0}-{1} 这段上监听' -f $Port, ($Port + $ViteProbeCount - 1)) }
        throw ('frontend 启动失败（{0}），见日志 {1}' -f $why, $logPath)
    }
    Write-Record -Service 'frontend' -Fields @{
        service   = 'frontend'
        pid       = $p.Id
        command   = $node.Source
        args      = ($viteArgs -join ' ')
        port      = $actual
        url       = ('http://localhost:{0}' -f $actual)
        dashboard = $false
        startedAt = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss')
        log       = $logPath
    }
    Write-Key 'frontend 启动' ('pid {0}  端口 {1}  http://localhost:{1}' -f $p.Id, $actual) 'Green'
    if ($actual -ne $Port) { Write-Line ('（请求端口 {0}，实际监听 {1}）' -f $Port, $actual) }
}

function Get-ManagedPids {
    param([string]$Service)
    $out = @()
    $services = @('backend', 'frontend')
    if ($Service) { $services = @($Service) }
    foreach ($svc in $services) {
        $r = Read-Record -Service $svc
        if (-not $r) { continue }
        $procId = 0
        if ([int]::TryParse((Get-Prop $r 'pid'), [ref]$procId) -and (Test-ProcessAlive -TargetPid $procId)) { $out += $procId }
    }
    return $out
}

# ---------------------------------------------------------------- 停止

function Stop-Service {
    param([string]$Service)
    $record = Read-Record -Service $Service
    if (-not $record) {
        Write-Line ('{0} 没有运行记录（.dev\{0}.json 不存在），无需停止。' -f $Service)
        return
    }
    $procId = 0
    [void][int]::TryParse((Get-Prop $record 'pid'), [ref]$procId)
    if (-not (Test-ProcessAlive -TargetPid $procId)) {
        Write-Key ($Service + ' 进程') ('pid {0} 已不存在，清理运行记录' -f $procId) 'DarkYellow'
        Remove-Record -Service $Service
        return
    }
    $code = Send-BreakToConsoleOwner -TargetPid $procId
    if ($code -ne 0) {
        Write-Line ('{0}（pid {1}）的 CTRL_BREAK 没能送达（helper 退出码 {2}）。' -f $Service, $procId, $code)
        Write-Line '脚本不代为强杀：请在该进程的隐藏 console 里 Ctrl+C，或手工结束进程后重跑 stop 清理记录。'
        return
    }
    $deadline = (Get-Date).AddSeconds($StopTimeout)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-ProcessAlive -TargetPid $procId)) {
            Write-Key ($Service + ' 进程') ('已优雅退出（pid {0}）' -f $procId) 'Green'
            Remove-Record -Service $Service
            return
        }
        Start-Sleep -Milliseconds 250
    }
    throw ('{0}（pid {1}）在 {2} 秒内没退出，优雅关停没走完；脚本不代为强杀。可加大 -StopTimeout。' -f $Service, $procId, $StopTimeout)
}

# ---------------------------------------------------------------- 子命令

function Show-Status {
    Write-Head 'godelayq 开发环境状态'
    $backendPort = Get-ConfiguredBackendPort -Override $BackendPort
    $frontendPort = Get-ConfiguredFrontendPort -Override $FrontendPort
    Write-Key '仓库根' $RepoRoot
    Write-Key '后端配置' $(if (Test-Path -LiteralPath $ConfigFile) { $ConfigFile } else { 'configs\config.yaml 不存在，进程用代码默认值' })
    Write-Key '配置端口' ('backend {0} / frontend {1}' -f $backendPort, $frontendPort)
    if ($Dashboard) { Write-Key '后端形态' '-tags dashboard 单二进制' }

    Write-Host ''
    Write-Line '配置端口上的监听与连通：'
    foreach ($pair in @(@('backend', $backendPort), @('frontend', $frontendPort))) {
        $an = Analyze-Port -Port ([int]$pair[1]) -OwnPids @(Get-ManagedPids -Service $pair[0])
        Write-Line ('  {0,-9} {1}  {2}  127.0.0.1={3}' -f $pair[0], $pair[1], $an.Stack, $(if ($an.V4) { '通' } else { '空' }))
    }

    foreach ($svc in @('backend', 'frontend')) {
        $record = Read-Record -Service $svc
        Write-Host ''
        if (-not $record) {
            Write-Key $svc '未运行（无运行记录）' 'DarkYellow'
            $expect = $backendPort
            if ($svc -eq 'frontend') { $expect = $frontendPort }
            Show-PortBlock -Title ($svc + ' 目标端口') -Analysis (Analyze-Port -Port $expect)
            continue
        }
        $procId = 0
        [void][int]::TryParse((Get-Prop $record 'pid'), [ref]$procId)
        $port = 0
        [void][int]::TryParse((Get-Prop $record 'port'), [ref]$port)
        if (Test-ProcessAlive -TargetPid $procId) {
            Write-Key $svc ('运行中  pid {0}  端口 {1}  起于 {2}' -f $procId, $port, (Get-Prop $record 'startedAt')) 'Green'
            Write-Line ('访问   ' + (Get-Prop $record 'url'))
            Write-Line ('命令   ' + (Get-Prop $record 'command') + ' ' + (Get-Prop $record 'args'))
            Write-Line ('日志   ' + (Get-Prop $record 'log'))
            if ($svc -eq 'backend') {
                $probe = Invoke-HealthProbe -Port $port
                if ($probe.Code -eq 200) { Write-Line ($HealthPath + ' -> 200') }
                elseif ($probe.Code -gt 0) { Write-Line ($HealthPath + ' -> ' + $probe.Code + '（服务在应答，只是需要凭据）') }
                else { Write-Line ($HealthPath + ' 探测失败：' + $probe.Detail) }
            }
            Show-PortBlock -Title ($svc + ' 实际端口') -Analysis (Analyze-Port -Port $port -OwnPids @($procId))
        } else {
            Write-Key $svc ('已停止（pid {0} 不在了，运行记录仍在）' -f $procId) 'DarkYellow'
        }
    }
    Write-Host ''
}

function Get-ProjectListeners {
    # 找出 1024-9999 上属于本项目但没被记录管理的监听（上次没记上、手工另开的实例）
    $tracked = @(Get-ManagedPids -Service '')
    $names = @()
    if (Test-Path -LiteralPath $ServerBin) { $names += (Split-Path $ServerBin -Leaf).ToLower() }
    if (Test-Path -LiteralPath $ConsolePath) { $names += (Split-Path $ConsolePath -Leaf).ToLower() }
    $rows = @(Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue | Where-Object { $_.LocalPort -ge 1024 -and $_.LocalPort -le 9999 })
    $seen = @{}
    $out = @()
    foreach ($r in $rows) {
        $procId = [int]$r.OwningProcess
        if ($tracked -contains $procId) { continue }
        $info = Describe-Process -TargetPid $procId
        $exe = if ($info.Path) { (Split-Path $info.Path -Leaf).ToLower() } else { $info.Name.ToLower() }
        # 只剧本仓库的进程：本机常有别的项目的 vite，命令行里没有本仓库路径就不是我们的事
        $inRepo = ($info.Cmd -match [regex]::Escape($RepoRoot)) -or ($info.Cmd -match [regex]::Escape($ViteDir))
        $hit = ($names -contains $exe) -or ($exe -eq 'node.exe' -and $inRepo)
        if (-not $hit) { continue }
        $key = ('{0}|{1}' -f $procId, $r.LocalPort)
        if ($seen.ContainsKey($key)) { continue }
        $seen[$key] = $true
        $out += [pscustomobject]@{ Port = [int]$r.LocalPort; Pid = $procId; Name = $info.Name; Cmd = $info.Cmd }
    }
    return $out
}

function Show-Ports {
    $backendPort = Get-ConfiguredBackendPort -Override $BackendPort
    $frontendPort = Get-ConfiguredFrontendPort -Override $FrontendPort
    Write-Head '端口占用分析'
    Write-Line '看两件事：监听表里绑的是哪个地址（:: 是双栈，::1/127.0.0.1 只有一栈），以及 127.0.0.1 能不能连进去。'

    Show-PortBlock -Title 'backend' -Analysis (Analyze-Port -Port $backendPort -OwnPids @(Get-ManagedPids -Service 'backend'))
    Show-PortBlock -Title 'frontend' -Analysis (Analyze-Port -Port $frontendPort -OwnPids @(Get-ManagedPids -Service 'frontend'))

    $f = Read-Record -Service 'frontend'
    if ($f) {
        $actual = 0
        [void][int]::TryParse((Get-Prop $f 'port'), [ref]$actual)
        $procId = 0
        [void][int]::TryParse((Get-Prop $f 'pid'), [ref]$procId)
        if ($actual -gt 0 -and $actual -ne $frontendPort -and (Test-ProcessAlive -TargetPid $procId)) {
            Write-Line 'frontend 实际顺延到了：'
            Show-PortBlock -Title 'frontend 实际' -Analysis (Analyze-Port -Port $actual -OwnPids @($procId))
        }
    }

    Write-Host ''
    Write-Line ('Vite 顺延观测区间 {0}-{1}：' -f $frontendPort, ($frontendPort + $ViteProbeCount - 1))
    foreach ($p in ($frontendPort..($frontendPort + $ViteProbeCount - 1))) {
        $rows = @(Get-ListenRows -Port $p)
        $v4 = Test-TcpPort -Port $p
        $tag = '空闲'
        if ($rows.Count -gt 0 -or $v4) {
            $pids = @($rows | ForEach-Object { $_.Pid } | Sort-Object -Unique)
            $tag = '占用'
            if ($pids.Count -gt 0) { $tag += ' pid ' + ($pids -join ',') }
            if ($rows.Count -eq 0) { $tag += '（能连上但没有 Listen 表项）' }
        }
        Write-Line ('    {0}  {1,-16} {2}  127.0.0.1={3}' -f $p, $tag, (Get-StackNote -Rows $rows), $(if ($v4) { '通' } else { '空' }))
    }

    Write-Host ''
    Write-Line '未纳管的本项目监听进程（1024-9999）：'
    $suspects = @(Get-ProjectListeners)
    if ($suspects.Count -eq 0) { Write-Line '    （无）' }
    foreach ($item in $suspects) {
        Write-Line ('    端口 {0}  pid {1}  {2}' -f $item.Port, $item.Pid, $item.Name)
        Write-Line ('        ' + (Format-Command -Cmd $item.Cmd -Width 80))
    }
    Write-Host ''
}

function Show-List {
    Write-Head '运行记录与实际监听'
    foreach ($svc in @('backend', 'frontend')) {
        $record = Read-Record -Service $svc
        if (-not $record) {
            Write-Line ('{0} : 无记录' -f $svc)
            continue
        }
        $procId = 0
        [void][int]::TryParse((Get-Prop $record 'pid'), [ref]$procId)
        $port = 0
        [void][int]::TryParse((Get-Prop $record 'port'), [ref]$port)
        $alive = Test-ProcessAlive -TargetPid $procId
        $state = if ($alive) { 'running' } else { 'dead   ' }
        Write-Line ('{0,-9} pid={1,-7} {2} port={3,-6} 起于 {4}' -f $svc, $procId, $state, $port, (Get-Prop $record 'startedAt'))
        Write-Line ('           命令  ' + (Get-Prop $record 'command') + ' ' + (Get-Prop $record 'args'))
        Write-Line ('           日志  ' + (Get-Prop $record 'log'))
        if ($alive) {
            foreach ($row in @(Get-ListenRows -Port $port)) {
                Write-Line ('           监听  {0}:{1} pid={2}  {3}' -f $row.Address, $row.Port, $row.Pid, (Get-StackNote -Rows @($row)))
            }
        }
    }
    Write-Host ''
    Write-Line '记录文件：'
    $files = @(Get-ChildItem -LiteralPath $DevDir -Filter '*.json' -ErrorAction SilentlyContinue)
    if ($files.Count -eq 0) { Write-Line '    （无）' }
    foreach ($f in $files) { Write-Line ('    ' + $f.Name + '  改于 ' + $f.LastWriteTime.ToString('yyyy-MM-dd HH:mm:ss')) }
    Write-Line '日志文件：'
    $logs = @(Get-ChildItem -LiteralPath $LogDir -Filter '*.log' -ErrorAction SilentlyContinue)
    if ($logs.Count -eq 0) { Write-Line '    （无）' }
    foreach ($l in $logs) {
        $size = if ($l.Length -lt 1024) { "$($l.Length) B" } else { ([math]::Round($l.Length / 1024, 1)).ToString() + ' KB' }
        Write-Line ('    ' + $l.Name + '  ' + $size + '  改于 ' + $l.LastWriteTime.ToString('HH:mm:ss'))
    }
    Write-Host ''
    Write-Line '未纳管的本项目监听进程：'
    $suspects = @(Get-ProjectListeners)
    if ($suspects.Count -eq 0) { Write-Line '    （无）' }
    foreach ($item in $suspects) {
        Write-Line ('    端口 {0}  pid {1}  {2}  {3}' -f $item.Port, $item.Pid, $item.Name, (Format-Command -Cmd $item.Cmd -Width 64))
    }
    Write-Host ''
}

function Show-Logs {
    param([string]$Svc)
    $names = @('backend', 'frontend')
    if ($Svc -eq 'backend' -or $Svc -eq 'frontend') { $names = @($Svc) }
    foreach ($n in $names) {
        $logPath = Join-Path $LogDir ($n + '.log')
        if (-not (Test-Path -LiteralPath $logPath)) {
            Write-Line ($n + ' 还没有日志文件：' + $logPath)
            continue
        }
        Write-Head ($n + ' 日志尾（' + $logPath + '）')
        Get-Content -LiteralPath $logPath -Tail $Tail | ForEach-Object { Write-Host (Remove-Ansi -Text $_) }
        $errPath = Join-Path $LogDir ($n + '.err.log')
        if ((Test-Path -LiteralPath $errPath) -and ((Get-Item -LiteralPath $errPath).Length -gt 0)) {
            Write-Head ($n + ' stderr 尾')
            Get-Content -LiteralPath $errPath -Tail $Tail | ForEach-Object { Write-Host (Remove-Ansi -Text $_) }
        }
    }
    if ($Follow) {
        Write-Head ('跟随 ' + $names[0] + ' 日志（Ctrl+C 退出）')
        Get-Content -LiteralPath (Join-Path $LogDir ($names[0] + '.log')) -Wait -Tail 5
    }
}

function Resolve-Targets {
    param([string]$Svc)
    $lower = ($Svc + '').ToLower()
    if ($lower -eq 'all' -or $lower -eq '') {
        if ($Dashboard) { return @('backend') }
        return @('backend', 'frontend')
    }
    if ($lower -ne 'backend' -and $lower -ne 'frontend') {
        throw ('未知的目标 "{0}"，可用：backend | frontend | all' -f $Svc)
    }
    return @($lower)
}

function Write-Usage {
    @(
        '用法：powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev.ps1 <command> [target] [开关]',
        '',
        'command',
        '  start   [backend|frontend|all]  启动。all = 后端进程 + Vite 进程两个',
        '  stop    [backend|frontend|all]  优雅停止（CTRL_BREAK -> 等进程自己退出，不强杀）',
        '  restart [backend|frontend|all]  先停再起',
        '  status                          进程 / 实际端口 / TCP 探测 / /api/v1/health',
        '  list                            运行记录、监听表项、日志大小、未纳管实例',
        '  ports                           端口占用分析 + Vite 顺延区间（含绑定地址是单栈还是双栈）',
        '  logs    [backend|frontend|all]  看日志尾（-Tail n、-Follow）',
        '  help',
        '',
        '开关',
        '  -BackendPort 9090    只影响本次启动：环境变量 GODELAYQ_SERVER_PORT',
        '  -FrontendPort 5199   只影响本次启动：传给 vite --port',
        '  -Dashboard           后端改用 -tags dashboard 单二进制（先 cd web; npm run build），此时不再起 Vite',
        '  -SkipBuild           复用 .dev\bin 里的产物',
        '  -StopTimeout 20      优雅退出等待秒数；超时只报错',
        '',
        '端口来源（优先级高到低）：开关 > 环境变量 > 配置文件 > 默认值',
        ('  backend  ：configs\config.yaml 的 server: port（默认 ' + $DefaultBackendPort + '）'),
        ('  frontend ：web\vite.config.ts 的 server.port（默认 ' + $DefaultFrontendPort + '，占用时 Vite 自行顺延）'),
        '',
        '产物与状态（.dev 整目录不进 git）',
        '  .dev\bin\godelayq-server.exe / godelayq-console.exe',
        '  .dev\backend.json / .dev\frontend.json',
        '  .dev\logs\{backend,frontend}.log 与 .err.log',
        '',
        '注意',
        '  - 后端工作目录固定在仓库根：配置里的 ./data、./exec-workspace 是相对路径。',
        '  - 优雅停止要求进程有自己那个隐藏 console，所以不要用别的方式起后端又期望 stop 能收尾。',
        ('  - configs\config.yaml 缺失时后端用代码默认值，端口仍按默认 {0} 分析。' -f $DefaultBackendPort)
    ) | ForEach-Object { Write-Host $_ }
}

# ---------------------------------------------------------------- 主流程

if ($SignalPid -gt 0) {
    # 这一次运行只是替 stop 去发信号，不做任何启动/查询动作
    exit (Send-BreakInThisProcess -TargetPid $SignalPid)
}

New-Item -ItemType Directory -Force -Path $DevDir, $LogDir | Out-Null
# .dev 里全是本机进程状态与日志，让它自己把自己忽略，不进 git status
$devIgnore = Join-Path $DevDir '.gitignore'
if (-not (Test-Path -LiteralPath $devIgnore)) {
    Set-Content -LiteralPath $devIgnore -Value "*`n" -Encoding ASCII -NoNewline
}

try {
    switch ($Command.ToLower()) {
    'help' { Write-Usage }
    'status' { Show-Status }
    'list' { Show-List }
    'ports' { Show-Ports }
    'logs' { Show-Logs -Svc $Target }
    'start' {
        $targets = Resolve-Targets -Svc $Target
        if ($targets -contains 'backend') {
            Start-Backend -Port (Get-ConfiguredBackendPort -Override $BackendPort) -UseDashboard:$Dashboard -OverridePort $BackendPort
        }
        if ($targets -contains 'frontend') {
            if ($Dashboard) { Write-Line '单二进制形态只有一个端口，不需要 Vite；跳过 frontend。' }
            else { Start-Frontend -Port (Get-ConfiguredFrontendPort -Override $FrontendPort) }
        }
        Show-Status
    }
    'stop' {
        $targets = @(Resolve-Targets -Svc $Target)
        [array]::Reverse($targets)
        foreach ($svc in $targets) { Stop-Service -Service $svc }
        Show-Status
    }
    'restart' {
        $targets = @(Resolve-Targets -Svc $Target)
        $back = @($targets)
        [array]::Reverse($back)
        foreach ($svc in $back) { Stop-Service -Service $svc }
        if ($targets -contains 'backend') {
            Start-Backend -Port (Get-ConfiguredBackendPort -Override $BackendPort) -UseDashboard:$Dashboard -OverridePort $BackendPort
        }
        if ($targets -contains 'frontend' -and -not $Dashboard) {
            Start-Frontend -Port (Get-ConfiguredFrontendPort -Override $FrontendPort)
        }
        Show-Status
    }
    }
} catch {
    # 脚本内部的失败只报这一行：PowerShell 默认的 throw 回显会把堆栈一起摊出来
    Write-Host ''
    Write-Host ('  失败：' + $_.Exception.Message) -ForegroundColor Red
    exit 1
}
