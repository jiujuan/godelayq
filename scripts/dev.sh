#!/usr/bin/env bash
# godelayq 本地开发环境的前后端进程管理脚本（Linux，bash 4+）。
# 与 scripts\dev.ps1（Windows 版）同构：命令、目标、开关、.dev 运行记录与日志布局全部一致，
# 两个脚本可以在同一份工作区交替使用（记录文件格式互通）。
#
# 管理的两个进程：
#   backend  —— go build 出 .dev/bin/godelayq-server[.exe]，子 shell 里 cd 到仓库根再启动。
#               configs/config.yaml 里的 ./data、./exec-workspace 都是相对路径，
#               工作目录不对就会在别处凭空建一套 data 目录。
#   frontend —— node web/node_modules/vite/bin/vite.js，等价于 web 目录下的 npm run dev。
#
# 运行记录（进程号、实际端口、启动时间、日志路径）写在 .dev/<service>.json，
# 日志写在 .dev/logs/。整个 .dev 目录由 .dev/.gitignore 忽略。
#
# 与 Windows 版的实现差异（行为对齐，机制不同）：
#   优雅停止 —— Go 侧 cmd/server/main.go 的 signal.Notify(SIGINT/SIGTERM) 在 Linux 上
#               直接收得到 SIGTERM，所以 stop 就是 kill -TERM 后等它自己退出，
#               不需要 Windows 那套 AttachConsole + CTRL_BREAK 的独立 helper 进程。
#               等待超时同样只报错、保留记录，不代为强杀。
#   端口分析 —— Get-NetTCPConnection 换成 ss（iproute2，发行版基本都有），退化用 lsof；
#               两者都没有时跳过占用预检并明确提示，bind 冲突会由启动失败兜底。
#   TCP 探测 —— 用 bash 内建 /dev/tcp（无需外部工具），等价 ps1 的 TcpClient 探测。
#   Vite 端口 —— 优先按 pid 从 ss 拿实际监听端口；没有 ss 时解析它自己打印的
#               "Local:" 一行（Vite 占用时会自动顺延，实际端口不一定是配置值）。
#   NO_COLOR —— 与 ps1 相同：请求无彩色输出，日志文件里不夹 ANSI 转义码。
#
# 用法：
#   ./scripts/dev.sh start  [backend|frontend|all] [--backend-port N] [--frontend-port N]
#   ./scripts/dev.sh stop   [backend|frontend|all]
#   ./scripts/dev.sh restart [backend|frontend|all]
#   ./scripts/dev.sh status | list | ports | logs [backend|frontend|all] | help
#
# 说明：脚本里所有路径都是 POSIX 形式；只有在 MSYS/Git Bash 里跑（给原生 exe 传参）时
# 才用 cygpath 把 -config 与 vite 入口转成 Windows 形式，Linux 上无 cygpath、原样传递。

set -u

# ---------------------------------------------------------------- 路径与常量

SCRIPT_PATH="${BASH_SOURCE[0]}"
REPO_ROOT="$(cd "$(dirname "$SCRIPT_PATH")/.." && pwd)"
DEV_DIR="$REPO_ROOT/.dev"
LOG_DIR="$DEV_DIR/logs"
BIN_DIR="$DEV_DIR/bin"

case "$(uname -s)" in
*_NT* | CYGWIN* | MSYS* | MINGW*) EXE_SUFFIX=.exe ;;
*) EXE_SUFFIX='' ;;
esac

SERVER_BIN="$BIN_DIR/godelayq-server$EXE_SUFFIX"
CONSOLE_BIN="$BIN_DIR/godelayq-console$EXE_SUFFIX"
CONFIG_FILE="$REPO_ROOT/configs/config.yaml"
VITE_DIR="$REPO_ROOT/web"
VITE_ENTRY="$VITE_DIR/node_modules/vite/bin/vite.js"

DEFAULT_BACKEND_PORT=8080
DEFAULT_FRONTEND_PORT=5177
VITE_SPAN=8
HEALTH_PATH=/api/v1/health

BACKEND_PORT_FLAG=0
FRONTEND_PORT_FLAG=0
DASHBOARD=0
SKIP_BUILD=0
STOP_TIMEOUT=20
FOLLOW=0
TAIL=40
CMD='help'
TARGET='all'

# 端口分析工具：ss 优先（现代发行版标配），退化 lsof，都没有则只能做 TCP 探测
if command -v ss >/dev/null 2>&1; then
  PORT_TOOL=ss
elif command -v lsof >/dev/null 2>&1; then
  PORT_TOOL=lsof
else
  PORT_TOOL=none
fi

# ---------------------------------------------------------------- 输出助手

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_HEAD=$'\033[36m'
  C_OK=$'\033[32m'
  C_WARN=$'\033[33m'
  C_ERR=$'\033[31m'
  C_OFF=$'\033[0m'
else
  C_HEAD=''; C_OK=''; C_WARN=''; C_ERR=''; C_OFF=''
fi

head_line() { printf '\n%s== %s%s\n' "$C_HEAD" "$1" "$C_OFF"; }
line() { printf '   %s\n' "$1"; }
warn_line() { printf '   %s%s%s\n' "$C_WARN" "$1" "$C_OFF"; }

# kv <名字> <值> [ok|warn|err]：与 ps1 的 Write-Key 对齐
kv() {
  local name="$1" value="$2" color="${3:-}" c=''
  case "$color" in
  ok) c=$C_OK ;;
  warn) c=$C_WARN ;;
  err) c=$C_ERR ;;
  esac
  printf '   %-16s%s%s%s\n' "$name" "$c" "$value" "$C_OFF"
}

fail() {
  printf '\n%s  失败：%s%s\n' "$C_ERR" "$1" "$C_OFF" >&2
  exit 1
}

# 传给原生程序（go 出的 exe、node.exe）的路径参数：MSYS 下转成 Windows 形式
native_path() {
  if command -v cygpath >/dev/null 2>&1; then
    # -m：正斜杠混合形式，Go/node 都认，且不引入反斜杠转义问题
    cygpath -m "$1"
  else
    printf '%s\n' "$1"
  fi
}

strip_ansi() { sed 's/\x1b\[[0-9;?]*[ -/]*[@-~]//g'; }

# ---------------------------------------------------------------- 运行记录

record_path() { printf '%s/%s.json' "$DEV_DIR" "$1"; }

# 字符串字段（只信任本脚本自己写出的格式，键一行一个）
record_str() {
  local file="$1" key="$2"
  [ -f "$file" ] || return 0
  sed -n 's/.*"'"$key"'": *"\([^"]*\)".*/\1/p' "$file" | head -n1
}

# 数值字段
record_num() {
  local file="$1" key="$2"
  [ -f "$file" ] || return 0
  sed -n 's/.*"'"$key"'": *\([0-9][0-9]*\).*/\1/p' "$file" | head -n1
}

write_record() {
  # write_record <service> <pid> <command> <args> <port> <dashboard> <log>
  local service="$1" pid="$2" command="$3" args="$4" port="$5" dashboard="$6" log="$7"
  mkdir -p "$DEV_DIR"
  cat >"$(record_path "$service")" <<EOF
{
  "service": "$service",
  "pid": $pid,
  "command": "$command",
  "args": "$args",
  "port": $port,
  "url": "http://localhost:$port",
  "dashboard": $dashboard,
  "startedAt": "$(date '+%Y-%m-%d %H:%M:%S')",
  "log": "$log"
}
EOF
}

remove_record() { rm -f "$(record_path "$1")"; }

is_alive() {
  local pid="${1:-}"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  [ "$pid" -gt 0 ] || return 1
  kill -0 "$pid" 2>/dev/null
}

# 当前仍活着的、由本脚本纳管的 pid（backend + frontend 的记录）
managed_pids() {
  local svc pid
  for svc in backend frontend; do
    pid=$(record_num "$(record_path "$svc")" pid)
    if is_alive "$pid"; then printf '%s\n' "$pid"; fi
  done
}

# ---------------------------------------------------------------- 端口分析

# 全部 TCP LISTEN 行，每行：port addr pid exe（拿不到进程信息时 pid=0、exe 空）
listen_rows() {
  case "$PORT_TOOL" in
  ss)
    ss -ltnH 2>/dev/null | while read -r _st _rq _sq addr _peer rest; do
      local port="${addr##*:}" pid=0 exe=''
      if [[ "$rest" == *'pid='* ]]; then
        pid=$(printf '%s\n' "$rest" | grep -oE 'pid=[0-9]+' | head -n1 | cut -d= -f2)
        exe=$(printf '%s\n' "$rest" | sed -nE 's/.*users:\(\("([^"]+)".*/\1/p')
      fi
      printf '%s %s %s %s\n' "$port" "$addr" "$pid" "$exe"
    done
    ;;
  lsof)
    lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null |
      awk 'NR>1 { pid=$2; split($9,a,":"); print a[length(a)], $9, pid, $1 }' | sort -u
    ;;
  esac
}

# 某端口上的监听者 pid 列表
port_pids() {
  listen_rows | awk -v p="$1" '$1+0==p+0 {print $3}' | sort -un
}

# 某 pid 监听的全部端口
own_listen_ports() {
  listen_rows | awk -v p="$1" '$3+0==p+0 {print $1}' | sort -un
}

# TCP 回环探测：bash 内建 /dev/tcp，无需外部工具
tcp_probe() {
  timeout 1 bash -c ": </dev/tcp/127.0.0.1/$1" 2>/dev/null
}

http_code() {
  curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$1$HEALTH_PATH" 2>/dev/null
}

# 进程的简短描述：名字 + 命令行（Linux 的 /proc；拿不到就如实说）
describe_pid() {
  local pid="$1" name cmd
  name=$(cat "/proc/$pid/comm" 2>/dev/null || ps -p "$pid" -o comm= 2>/dev/null)
  cmd=$(tr '\0' '' </proc/"$pid"/cmdline 2>/dev/null)
  if [ -n "$name" ]; then printf '%s' "${name%%[[:space:]]*}"
  else printf '?'; fi
  [ -n "$cmd" ] && printf '  <- %s' "$cmd"
  printf '\n'
}

# 一行摘要：端口上都有谁在监听
port_listeners_desc() {
  local port="$1" rows
  rows=$(listen_rows | awk -v p="$port" '$1+0==p+0')
  if [ -z "$rows" ]; then
    if tcp_probe "$port"; then printf '无监听表项，但 127.0.0.1 能连上'
    else printf '无监听'; fi
    return
  fi
  printf '%s\n' "$rows" | awk '{
    d = sep $2 " pid " $3
    if ($4 != "") d = d "(" $4 ")"
    printf "%s", d; sep = ", "
  }'
}

# 端口块：与 ps1 的 Show-PortBlock 对齐的精简版（空闲 / 能连上没表项 / 谁在监听）
print_port_block() {
  local title="$1" port="$2" rows own foreign
  rows=$(listen_rows | awk -v p="$port" '$1+0==p+0')
  if [ -z "$rows" ]; then
    if tcp_probe "$port"; then
      line "$(printf '%-12s 端口 %-6s [无监听表项，但能连上]' "$title" "$port")"
    else
      line "$(printf '%-12s 端口 %-6s [空闲]' "$title" "$port")"
    fi
    return
  fi
  own=$(managed_pids)
  foreign=0
  while read -r _port _addr pid _exe; do
    if [ "$pid" -gt 0 ] && ! printf '%s\n' $own | grep -qx "$pid"; then foreign=1; fi
  done <<<"$rows"
  if [ "$foreign" = 1 ]; then
    line "$(printf '%-12s 端口 %-6s [被外部进程占用]' "$title" "$port")"
  else
    line "$(printf '%-12s 端口 %-6s [本项目进程]' "$title" "$port")"
  fi
  while read -r _port addr pid exe; do
    line "监听 $addr  pid=$pid  ${exe:-（进程信息不可见）}"
    if [ "$pid" -gt 0 ]; then
      line "      $(describe_pid "$pid")"
    fi
  done <<<"$rows"
}

# 未纳管的本项目监听进程（上次没记上、手工另开的实例）：1024-9999
find_unmanaged() {
  if [ "$PORT_TOOL" = none ]; then
    printf 'NO_TOOL\n'
    return
  fi
  local tracked rows port pid exe name cmd cwd hit
  tracked=$(managed_pids)
  rows=$(listen_rows)
  while read -r port _addr pid exe; do
    [ -z "$port" ] && continue
    { [ "$port" -ge 1024 ] 2>/dev/null && [ "$port" -le 9999 ] 2>/dev/null; } || continue
    [ "$pid" -gt 0 ] 2>/dev/null || continue
    printf '%s\n' $tracked | grep -qx "$pid" && continue
    name="${exe}"
    if [ -z "$name" ]; then
      name=$(cat "/proc/$pid/comm" 2>/dev/null || ps -p "$pid" -o comm= 2>/dev/null)
    fi
    name="${name##*/}"
    cmd=$(tr '\0' ' ' </proc/"$pid"/cmdline 2>/dev/null)
    hit=0
    case "$name" in
    godelayq-server | godelayq-console) hit=1 ;;
    node)
      cwd=$(readlink "/proc/$pid/cwd" 2>/dev/null)
      # 只算本仓库的 node：本机常有别的项目的 vite，命令行与工作目录都不沾边就不是我们的事
      if [[ "$cmd" == *"$REPO_ROOT"* ]] || [[ "$cwd" == "$REPO_ROOT"* ]] || [[ "$cwd" == "$VITE_DIR"* ]]; then
        hit=1
      fi
      ;;
    esac
    [ "$hit" = 1 ] && printf '%s %s %s %s\n' "$port" "$pid" "$name" "$cmd"
  done <<<"$rows"
}

# ---------------------------------------------------------------- 配置读取

configured_backend_port() {
  if [ "$BACKEND_PORT_FLAG" -gt 0 ]; then printf '%s\n' "$BACKEND_PORT_FLAG"; return; fi
  if [[ "${GODELAYQ_SERVER_PORT:-}" =~ ^[0-9]+$ ]]; then printf '%s\n' "$GODELAYQ_SERVER_PORT"; return; fi
  if [ -f "$CONFIG_FILE" ]; then
    local p
    # 只取 server: 块里的 port（到下一个顶层键为止），避免抓到别节的同名键
    p=$(sed -n '/^server:/,/^[^[:space:]#]/p' "$CONFIG_FILE" |
      sed -nE 's/^[[:space:]]*port:[[:space:]]*"?([0-9]+)"?.*/\1/p' | head -n1)
    if [ -n "$p" ]; then printf '%s\n' "$p"; return; fi
  fi
  printf '%s\n' "$DEFAULT_BACKEND_PORT"
}

configured_frontend_port() {
  if [ "$FRONTEND_PORT_FLAG" -gt 0 ]; then printf '%s\n' "$FRONTEND_PORT_FLAG"; return; fi
  if [[ "${DEV_FRONTEND_PORT:-}" =~ ^[0-9]+$ ]]; then printf '%s\n' "$DEV_FRONTEND_PORT"; return; fi
  if [ -f "$VITE_DIR/vite.config.ts" ]; then
    local p
    p=$(sed -nE 's/^[[:space:]]*port:[[:space:]]*([0-9]+).*/\1/p' "$VITE_DIR/vite.config.ts" | head -n1)
    if [ -n "$p" ]; then printf '%s\n' "$p"; return; fi
  fi
  printf '%s\n' "$DEFAULT_FRONTEND_PORT"
}

# ---------------------------------------------------------------- 启动

start_backend() {
  local bin="$SERVER_BIN"
  [ "$DASHBOARD" = 1 ] && bin="$CONSOLE_BIN"
  local requested
  requested=$(configured_backend_port)
  local log="$LOG_DIR/backend.log" errlog="$LOG_DIR/backend.err.log"
  mkdir -p "$BIN_DIR" "$LOG_DIR"

  # 运行记录优先于端口表项：一个在跑的 backend 无论端口对错都占着同一份 ./data，
  # 再起一个只会让两个进程互相踩快照
  local rec_file rec_pid rec_cmd rec_port
  rec_file=$(record_path backend)
  rec_pid=$(record_num "$rec_file" pid)
  rec_cmd=$(record_str "$rec_file" command)
  if is_alive "$rec_pid"; then
    if [ "$rec_cmd" != "$bin" ]; then
      fail "backend 已在运行（pid $rec_pid，形态 $rec_cmd，端口 $(record_num "$rec_file" port)），换端口或换 --dashboard 形态请先 stop。"
    fi
    line "backend 已在运行（pid $rec_pid，端口 $(record_num "$rec_file" port)），本次不重复启动。"
    return 0
  fi

  # 端口占用预检：端口表上任何不是本项目纳管 pid 的监听者都算外部进程
  local own foreign p
  own=$(managed_pids)
  if [ "$PORT_TOOL" = none ]; then
    warn_line "缺少 ss / lsof，跳过端口 $requested 的占用预检；若真被占用，backend 会在 bind 时失败退出（见日志）。"
  else
    foreign=''
    for p in $(port_pids "$requested"); do
      printf '%s\n' $own | grep -qx "$p" || foreign="$foreign $p"
    done
    if [ -n "$foreign" ]; then
      print_port_block backend "$requested"
      fail "backend 需要的端口 $requested 被外部进程占用（pid:$foreign）：先处理它，或用 --backend-port 换一个端口。"
    fi
  fi

  if [ "$SKIP_BUILD" = 1 ]; then
    [ -x "$bin" ] || fail "找不到后端产物 $bin，去掉 --skip-build 重新 start。"
  else
    command -v go >/dev/null 2>&1 || fail "PATH 里找不到 go。"
    local -a build_args=(build)
    [ "$DASHBOARD" = 1 ] && build_args+=(-tags dashboard)
    build_args+=(-o "$bin" ./cmd/server)
    line "go ${build_args[*]}"
    (cd "$REPO_ROOT" && go "${build_args[@]}") >"$LOG_DIR/build.log" 2>&1 ||
      { cat "$LOG_DIR/build.log"; fail "go build 失败，详见 $LOG_DIR/build.log"; }
  fi

  local -a proc_args=()
  if [ -f "$CONFIG_FILE" ]; then
    proc_args+=("-config=$(native_path "$CONFIG_FILE")")
  fi

  : >"$log"
  : >"$errlog"
  local pid
  (
    cd "$REPO_ROOT" || exit 1
    # 只在显式给了 --backend-port 时用环境变量覆盖；否则交给配置文件
    if [ "$BACKEND_PORT_FLAG" -gt 0 ]; then
      exec nohup env GODELAYQ_SERVER_PORT="$BACKEND_PORT_FLAG" "$bin" ${proc_args[@]+"${proc_args[@]}"}
    else
      exec nohup "$bin" ${proc_args[@]+"${proc_args[@]}"}
    fi
  ) >"$log" 2>"$errlog" </dev/null &
  pid=$!
  disown

  local i actual=0
  for i in $(seq 1 20); do
    if ! is_alive "$pid"; then
      fail "backend（pid $pid）启动后退出，见日志 $log"
    fi
    if tcp_probe "$requested"; then actual=$requested; break; fi
    sleep 0.3
  done
  [ "$actual" -gt 0 ] || fail "backend（pid $pid）6 秒内没有监听端口 $requested，见日志 $log"

  # 有 ss 时核对实际端口：进程活着却绑在别的端口（配置与环境变量各说各话）要如实指出
  if [ "$PORT_TOOL" != none ]; then
    local ports_now
    ports_now=$(own_listen_ports "$pid")
    if [ -n "$ports_now" ] && ! printf '%s\n' "$ports_now" | grep -qx "$requested"; then
      actual=$(printf '%s\n' "$ports_now" | head -n1)
      line "（请求端口 $requested，实际监听 $actual：看是 configs/config.yaml 的 server.port 还是 GODELAYQ_SERVER_PORT 在生效）"
    fi
  fi

  write_record backend "$pid" "$bin" "${proc_args[*]:-}" "$actual" "$DASHBOARD" "$log"
  kv 'backend 启动' "pid $pid  端口 $actual  http://localhost:$actual" ok
}

start_frontend() {
  local requested
  requested=$(configured_frontend_port)
  local log="$LOG_DIR/frontend.log" errlog="$LOG_DIR/frontend.err.log"
  mkdir -p "$LOG_DIR"

  [ -f "$VITE_ENTRY" ] || fail "找不到 $VITE_ENTRY，先在 web 目录执行 npm install。"
  command -v node >/dev/null 2>&1 || fail "PATH 里找不到 node。"

  local rec_file rec_pid
  rec_file=$(record_path frontend)
  rec_pid=$(record_num "$rec_file" pid)
  if is_alive "$rec_pid"; then
    line "frontend 已在运行（pid $rec_pid，端口 $(record_num "$rec_file" port)），换端口请先 stop。"
    return 0
  fi

  # 端口表上已是本项目纳管 pid 在听 → 不重复启动
  local own p
  own=$(managed_pids)
  if [ "$PORT_TOOL" != none ]; then
    for p in $(port_pids "$requested"); do
      if printf '%s\n' $own | grep -qx "$p"; then
        line "frontend 已在运行（pid $p，端口 $requested），本次不重复启动。"
        return 0
      fi
    done
    # Vite 会自动顺延，所以配置端口被外部占用不拦启动，只说明清楚
    for p in $(port_pids "$requested"); do
      print_port_block frontend "$requested"
      warn_line "端口 $requested 被外部进程（pid $p）占用；Vite 未设 strictPort，会自动顺延到下一个空闲端口，继续启动。"
      break
    done
  fi

  : >"$log"
  : >"$errlog"
  local pid
  (
    cd "$VITE_DIR" || exit 1
    # 请求无彩色输出，日志文件里就不会夹 ANSI 转义码（NO_COLOR 约定）
    exec nohup env NO_COLOR=1 node "$(native_path "$VITE_ENTRY")" --port "$requested"
  ) >"$log" 2>"$errlog" </dev/null &
  pid=$!
  disown

  local i actual=0
  for i in $(seq 1 80); do
    if ! is_alive "$pid"; then break; fi
    if [ "$PORT_TOOL" != none ]; then
      actual=$(own_listen_ports "$pid" |
        awk -v b="$requested" -v s="$VITE_SPAN" '$1+0>=b+0 && $1+0<b+s {print $1; exit}')
      [ -z "$actual" ] && actual=0
    fi
    if [ "$actual" = 0 ]; then
      # 退化路径：Vite 自己打印的 Local: 行就是实际端口（NO_COLOR=1 时无 ANSI，仍防御性去一遍）
      actual=$(strip_ansi <"$log" | sed -nE 's/.*Local:.*localhost:([0-9]+).*/\1/p' | head -n1)
      [ -z "$actual" ] && actual=0
    fi
    [ "$actual" -gt 0 ] && break
    sleep 0.25
  done

  if [ "$actual" = 0 ]; then
    if ! is_alive "$pid"; then
      fail "frontend 启动失败（进程已退出），见日志 $log"
    fi
    fail "frontend 启动失败（20 秒内没有在 $requested-$((requested + VITE_SPAN - 1)) 这段上监听），见日志 $log"
  fi

  write_record frontend "$pid" "$(command -v node)" "vite.js --port $actual" "$actual" 0 "$log"
  kv 'frontend 启动' "pid $pid  端口 $actual  http://localhost:$actual" ok
  if [ "$actual" -ne "$requested" ]; then
    line "（请求端口 $requested，实际监听 $actual）"
  fi
}

# ---------------------------------------------------------------- 停止

stop_service() {
  local svc="$1" file pid
  file=$(record_path "$svc")
  if [ ! -f "$file" ]; then
    line "$svc 没有运行记录（.dev/$svc.json 不存在），无需停止。"
    return 0
  fi
  pid=$(record_num "$file" pid)
  if ! is_alive "$pid"; then
    kv "$svc 进程" "pid ${pid:-0} 已不存在，清理运行记录" warn
    remove_record "$svc"
    return 0
  fi
  # Go 侧 signal.Notify(SIGTERM) 会走优雅关停：调度器与 store 的收尾都能执行
  kill -TERM "$pid" 2>/dev/null ||
    fail "向 $svc（pid $pid）发送 SIGTERM 失败。"
  local i
  for i in $(seq 1 $((STOP_TIMEOUT * 4))); do
    if ! is_alive "$pid"; then
      kv "$svc 进程" "已优雅退出（pid $pid）" ok
      remove_record "$svc"
      return 0
    fi
    sleep 0.25
  done
  fail "$svc（pid $pid）在 $STOP_TIMEOUT 秒内没退出，优雅关停没走完；脚本不代为强杀，可加大 --stop-timeout。"
}

# ---------------------------------------------------------------- 子命令

resolve_targets() {
  case "$1" in
  all)
    if [ "$DASHBOARD" = 1 ]; then printf 'backend\n'
    else printf 'backend\nfrontend\n'; fi
    ;;
  backend | frontend) printf '%s\n' "$1" ;;
  *) fail "未知的目标 \"$1\"，可用：backend | frontend | all" ;;
  esac
}

show_status() {
  local backend_port frontend_port
  backend_port=$(configured_backend_port)
  frontend_port=$(configured_frontend_port)
  head_line 'godelayq 开发环境状态'
  kv '仓库根' "$REPO_ROOT"
  if [ -f "$CONFIG_FILE" ]; then kv '后端配置' "$CONFIG_FILE"
  else kv '后端配置' 'configs/config.yaml 不存在，进程用代码默认值' warn; fi
  kv '配置端口' "backend $backend_port / frontend $frontend_port"
  [ "$DASHBOARD" = 1 ] && kv '后端形态' '-tags dashboard 单二进制'
  [ "$PORT_TOOL" = none ] && warn_line "（缺少 ss / lsof：只能做 TCP 连通探测，列不出监听进程）"

  line '配置端口上的监听与连通：'
  line "  $(printf '%-9s %-6s %s  127.0.0.1=%s' backend "$backend_port" \
    "$(port_listeners_desc "$backend_port")" "$(tcp_probe "$backend_port" && echo 通 || echo 空)")"
  line "  $(printf '%-9s %-6s %s  127.0.0.1=%s' frontend "$frontend_port" \
    "$(port_listeners_desc "$frontend_port")" "$(tcp_probe "$frontend_port" && echo 通 || echo 空)")"

  local svc file pid port started
  for svc in backend frontend; do
    file=$(record_path "$svc")
    pid=$(record_num "$file" pid)
    port=$(record_num "$file" port)
    started=$(record_str "$file" startedAt)
    if [ ! -f "$file" ]; then
      kv "$svc" '未运行（无运行记录）' warn
      continue
    fi
    if is_alive "$pid"; then
      kv "$svc" "运行中  pid $pid  端口 $port  起于 $started" ok
      line "访问   $(record_str "$file" url)"
      line "命令   $(record_str "$file" command) $(record_str "$file" args)"
      line "日志   $(record_str "$file" log)"
      if [ "$svc" = backend ]; then
        local code
        code=$(http_code "$port")
        if [ "$code" = 200 ]; then line "$HEALTH_PATH -> 200"
        elif [ "$code" = 000 ]; then line "$HEALTH_PATH 探测失败（连不上）" warn
        else line "$HEALTH_PATH -> $code（服务在应答，只是需要凭据）"; fi
      fi
      line "监听   $(port_listeners_desc "$port")"
    else
      kv "$svc" "已停止（pid $pid 不在了，运行记录仍在）" warn
    fi
  done
  printf '\n'
}

show_list() {
  head_line '运行记录与实际监听'
  local svc file pid port alive
  for svc in backend frontend; do
    file=$(record_path "$svc")
    if [ ! -f "$file" ]; then
      line "$svc : 无记录"
      continue
    fi
    pid=$(record_num "$file" pid)
    port=$(record_num "$file" port)
    if is_alive "$pid"; then alive='running'; else alive='dead'; fi
    line "$(printf '%-9s pid=%-7s %-8s port=%-6s 起于 %s' "$svc" "$pid" "$alive" "$port" "$(record_str "$file" startedAt)")"
    line "           命令  $(record_str "$file" command) $(record_str "$file" args)"
    line "           日志  $(record_str "$file" log)"
    if is_alive "$pid" && [ "$PORT_TOOL" != none ]; then
      local addr p2 p3 p4
      own_listen_ports "$pid" >/dev/null 2>&1
      while read -r p2 p3 p4; do
        line "           监听  $p3 pid=$p2 $p4"
      done <<<"$(listen_rows | awk -v q="$pid" '$3+0==q+0')"
    fi
  done
  printf '\n'
  line '记录文件：'
  local f
  for f in "$DEV_DIR"/*.json; do
    [ -e "$f" ] || { line '    （无）'; break; }
    line "    $(basename "$f")  改于 $(date -r "$f" '+%Y-%m-%d %H:%M:%S')"
  done
  line '日志文件：'
  for f in "$LOG_DIR"/*.log; do
    [ -e "$f" ] || { line '    （无）'; break; }
    line "    $(basename "$f")  $(du -h "$f" | cut -f1)  改于 $(date -r "$f" '+%H:%M:%S')"
  done
  printf '\n'
  line '未纳管的本项目监听进程（1024-9999）：'
  local row found=0
  while read -r row; do
    [ "$row" = NO_TOOL ] && { warn_line '    （缺少 ss / lsof，跳过扫描）'; found=1; break; }
    [ -z "$row" ] && continue
    line "    端口 ${row%% *}  pid $(echo "$row" | awk '{print $2}')  $(echo "$row" | cut -d' ' -f3-)"
    found=1
  done <<<"$(find_unmanaged)"
  [ "$found" = 0 ] && line '    （无）'
  printf '\n'
}

show_ports() {
  local backend_port frontend_port
  backend_port=$(configured_backend_port)
  frontend_port=$(configured_frontend_port)
  head_line '端口占用分析'
  if [ "$PORT_TOOL" = none ]; then
    warn_line '缺少 ss / lsof：下列判断只基于 TCP 探测，列不出监听进程。'
  fi
  print_port_block backend "$backend_port"
  print_port_block frontend "$frontend_port"

  local p
  line "Vite 顺延观测区间 $frontend_port-$((frontend_port + VITE_SPAN - 1))："
  for p in $(seq "$frontend_port" $((frontend_port + VITE_SPAN - 1))); do
    local rows tag
    rows=$(listen_rows | awk -v q="$p" '$1+0==q+0')
    if [ -n "$rows" ]; then
      tag="占用 $(printf '%s\n' "$rows" | awk '{printf "%spid %s", s, $3; s=","}')"
    elif tcp_probe "$p"; then
      tag='占用（能连上但没有监听表项）'
    else
      tag='空闲'
    fi
    line "    $p  $tag"
  done

  printf '\n'
  line '未纳管的本项目监听进程（1024-9999）：'
  local row found=0
  while read -r row; do
    [ "$row" = NO_TOOL ] && { warn_line '    （缺少 ss / lsof，跳过扫描）'; found=1; break; }
    [ -z "$row" ] && continue
    line "    端口 ${row%% *}  pid $(echo "$row" | awk '{print $2}')  $(echo "$row" | cut -d' ' -f3-)"
    found=1
  done <<<"$(find_unmanaged)"
  [ "$found" = 0 ] && line '    （无）'
  printf '\n'
}

show_logs() {
  local names
  case "$TARGET" in
  backend | frontend) names=("$TARGET") ;;
  *) names=(backend frontend) ;;
  esac
  local n log errlog
  for n in "${names[@]}"; do
    log="$LOG_DIR/$n.log"
    if [ ! -f "$log" ]; then
      line "$n 还没有日志文件：$log"
      continue
    fi
    head_line "$n 日志尾（$log）"
    tail -n "$TAIL" "$log" | strip_ansi
    errlog="$LOG_DIR/$n.err.log"
    if [ -f "$errlog" ] && [ -s "$errlog" ]; then
      head_line "$n stderr 尾"
      tail -n "$TAIL" "$errlog" | strip_ansi
    fi
  done
  if [ "$FOLLOW" = 1 ]; then
    head_line "跟随 ${names[0]} 日志（Ctrl+C 退出）"
    tail -f "$LOG_DIR/${names[0]}.log"
  fi
}

write_usage() {
  cat <<EOF
用法：./scripts/dev.sh <command> [target] [选项]

command
  start    [backend|frontend|all]  启动。all = 后端进程 + Vite 进程两个
  stop     [backend|frontend|all]  优雅停止（SIGTERM -> 等进程自己退出，不强杀）
  restart  [backend|frontend|all]  先停再起
  status                           进程 / 实际端口 / TCP 探测 / /api/v1/health
  list                             运行记录、监听表项、日志大小、未纳管实例
  ports                            端口占用分析 + Vite 顺延区间
  logs     [backend|frontend|all]  看日志尾（--tail n、--follow）
  help

选项
  --backend-port 9090    只影响本次启动：环境变量 GODELAYQ_SERVER_PORT
  --frontend-port 5199   只影响本次启动：传给 vite --port
  --dashboard            后端改用 -tags dashboard 单二进制（先 cd web; npm run build），此时不再起 Vite
  --skip-build           复用 .dev/bin 里的产物
  --stop-timeout 20      优雅退出等待秒数；超时只报错
  --tail 40              logs 子命令的行数
  --follow               logs 子命令跟随输出

端口来源（优先级高到低）：选项 > 环境变量 > 配置文件 > 默认值
  backend  ：configs/config.yaml 的 server: port（默认 $DEFAULT_BACKEND_PORT）
  frontend ：web/vite.config.ts 的 server.port（默认 $DEFAULT_FRONTEND_PORT，占用时 Vite 自行顺延）

产物与状态（.dev 整目录不进 git）
  .dev/bin/godelayq-server[.exe] / godelayq-console[.exe]
  .dev/backend.json / .dev/frontend.json
  .dev/logs/{backend,frontend}.log 与 .err.log

注意
  - 后端工作目录固定在仓库根：配置里的 ./data、./exec-workspace 是相对路径。
  - 优雅停止靠 SIGTERM（Go 侧 signal.Notify 已处理）；超时不强杀。
  - 端口分析依赖 ss（或 lsof）；都没有时跳过占用预检，bind 冲突由启动失败兜底。
  - configs/config.yaml 缺失时后端用代码默认值，端口仍按默认 $DEFAULT_BACKEND_PORT 分析。
EOF
}

# ---------------------------------------------------------------- 参数解析

while [ $# -gt 0 ]; do
  case "$1" in
  start | stop | restart | status | list | ports | logs | help) CMD="$1" ;;
  backend | frontend | all) TARGET="$1" ;;
  --backend-port)
    [ $# -ge 2 ] || fail "--backend-port 需要一个数值参数"
    [[ "$2" =~ ^[0-9]+$ ]] || fail "--backend-port 需要 0-65535 的数字，收到 \"$2\""
    BACKEND_PORT_FLAG=$2
    shift
    ;;
  --frontend-port)
    [ $# -ge 2 ] || fail "--frontend-port 需要一个数值参数"
    [[ "$2" =~ ^[0-9]+$ ]] || fail "--frontend-port 需要 0-65535 的数字，收到 \"$2\""
    FRONTEND_PORT_FLAG=$2
    shift
    ;;
  --dashboard) DASHBOARD=1 ;;
  --skip-build) SKIP_BUILD=1 ;;
  --stop-timeout)
    [ $# -ge 2 ] || fail "--stop-timeout 需要一个数值参数"
    [[ "$2" =~ ^[0-9]+$ ]] || fail "--stop-timeout 需要数字，收到 \"$2\""
    STOP_TIMEOUT=$2
    shift
    ;;
  --tail)
    [ $# -ge 2 ] || fail "--tail 需要一个数值参数"
    [[ "$2" =~ ^[0-9]+$ ]] || fail "--tail 需要数字，收到 \"$2\""
    TAIL=$2
    shift
    ;;
  --follow) FOLLOW=1 ;;
  -h | --help) CMD=help ;;
  *)
    fail "未知参数 \"$1\"（见 ./scripts/dev.sh help）"
    ;;
  esac
  shift
done

# ---------------------------------------------------------------- 主流程

mkdir -p "$DEV_DIR" "$LOG_DIR"
# .dev 里全是本机进程状态与日志，让它自己把自己忽略，不进 git status
if [ ! -f "$DEV_DIR/.gitignore" ]; then
  printf '*\n' >"$DEV_DIR/.gitignore"
fi

case "$CMD" in
help)
  write_usage
  ;;
status)
  show_status
  ;;
list)
  show_list
  ;;
ports)
  show_ports
  ;;
logs)
  show_logs
  ;;
start)
  for t in $(resolve_targets "$TARGET"); do
    if [ "$t" = backend ]; then
      start_backend
    else
      if [ "$DASHBOARD" = 1 ]; then
        line '单二进制形态只有一个端口，不需要 Vite；跳过 frontend。'
      else
        start_frontend
      fi
    fi
  done
  show_status
  ;;
stop)
  # 与 ps1 相同：先停 frontend 再停 backend（顺序反过来收尾干净）
  mapfile -t targets < <(resolve_targets "$TARGET")
  for ((idx = ${#targets[@]} - 1; idx >= 0; idx--)); do
    stop_service "${targets[$idx]}"
  done
  show_status
  ;;
restart)
  mapfile -t targets < <(resolve_targets "$TARGET")
  for ((idx = ${#targets[@]} - 1; idx >= 0; idx--)); do
    stop_service "${targets[$idx]}"
  done
  for t in "${targets[@]}"; do
    if [ "$t" = backend ]; then
      start_backend
    elif [ "$DASHBOARD" = 1 ]; then
      line '单二进制形态只有一个端口，不需要 Vite；跳过 frontend。'
    else
      start_frontend
    fi
  done
  show_status
  ;;
esac
