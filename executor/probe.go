package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ProbeResult 是一次可用性探测的结论。
//
// 探测回答的是"这台机器现在能不能跑这个档位"，与 E02 的"这条配置写得对不对"是两件事：
// 配置合法但机器上缺程序，档位仍然保留在登记表里并带上原因，
// 这样接口能显示"已声明但不可用"，提交时也能给出明确拒绝，而不是等任务触发后在日志里找原因。
type ProbeResult struct {
	// Available 表示该档位当前可以执行
	Available bool
	// Path 是找到的可执行文件绝对路径（脚本档位是解释器的路径）。不可用时为空。
	// 这个字段只给服务端内部使用，对外展示请用 Profile.ProgramDisplay()。
	Path string
	// Reason 是不可用的原因，写法可直接显示给用户；其中的路径一律是相对 workspace 的写法。
	Reason string
	// Version 恒为空：探测阶段不执行任何外部程序。
	// 采集版本要真的跑 `<program> --version`，那会在启动时执行未审过的程序，
	// 也可能被程序自身的副作用影响（有些解释器 --version 会读配置文件）。
	Version string
}

// windowsExecutableExts 是 Windows 上能直接执行的扩展名集合。
// 不在集合里的文件（例如 workspace 里一个叫 etl 的产物）交给系统执行一定会失败，
// 与其等到任务失败，不如在探测阶段就说明。
var windowsExecutableExts = map[string]bool{
	".exe": true, ".com": true, ".bat": true, ".cmd": true,
}

// lookPath 是给测试留的注入点，默认就是 exec.LookPath。
var lookPath = exec.LookPath

// Probe 判断单个档位当前是否可执行。不启动任何进程，只查 PATH 与文件系统。
func Probe(p *Profile) ProbeResult {
	switch p.Kind {
	case KindHTTP:
		// 不访问网络：启动被外部依赖卡住是不可接受的，连通性属于执行期（TASK-E15）
		return ProbeResult{Available: true}

	case KindScript:
		return probeScript(p)

	case KindBinary:
		if p.ProgramName != "" {
			// program 写的是 runtime_allow 里的程序名，按 PATH 查找
			return probeProgramName(p)
		}
		return probeWorkspaceProgram(p)

	default:
		return ProbeResult{Reason: fmt.Sprintf("profile %q has an unknown kind %q", p.Name, p.Kind)}
	}
}

// probeScript 检查解释器与脚本文件。
//
// 脚本文件只要求"存在且是普通文件"，不要求执行位：它由解释器读取（node app.mjs），
// 而不是被系统直接执行。给脚本加执行位要求会误伤一批合法配置。
func probeScript(p *Profile) ProbeResult {
	resolved, err := lookPath(p.Runtime)
	if err != nil {
		return ProbeResult{Reason: fmt.Sprintf("runtime %q not found in PATH", p.Runtime)}
	}
	if reason, ok := fileCheckReason("script", p.ScriptPath, p.ScriptRel); !ok {
		return ProbeResult{Reason: reason}
	}
	return ProbeResult{Available: true, Path: resolved}
}

func probeProgramName(p *Profile) ProbeResult {
	resolved, err := lookPath(p.ProgramName)
	if err != nil {
		return ProbeResult{Reason: fmt.Sprintf("program %q not found in PATH", p.ProgramName)}
	}
	return ProbeResult{Available: true, Path: resolved}
}

// probeWorkspaceProgram 检查产物文件本身。
//
// 与脚本不同，这个文件是要直接执行的，所以按平台检查"能不能执行"：
// Unix 看执行位，Windows 看扩展名。
func probeWorkspaceProgram(p *Profile) ProbeResult {
	reason, ok := fileCheckReason("program", p.ProgramPath, p.ProgramRel)
	if !ok {
		return ProbeResult{Reason: reason}
	}
	info, err := os.Stat(p.ProgramPath)
	if err != nil {
		return ProbeResult{Reason: fmt.Sprintf("program %q cannot be read: %v", displayOf(p.ProgramRel, p.ProgramPath), err)}
	}
	if runtime.GOOS == "windows" {
		ext := strings.ToLower(filepath.Ext(p.ProgramPath))
		if !windowsExecutableExts[ext] {
			return ProbeResult{Reason: fmt.Sprintf("program %q is not executable on Windows, the extension must be one of %s",
				displayOf(p.ProgramRel, p.ProgramPath), strings.Join(sortedExtensions(), ", "))}
		}
	} else if info.Mode()&0o111 == 0 {
		return ProbeResult{Reason: fmt.Sprintf("program %q is not executable (no execute bit)", displayOf(p.ProgramRel, p.ProgramPath))}
	}
	return ProbeResult{Available: true, Path: p.ProgramPath}
}

// fileCheckReason 判断目标文件是否可用，返回不可用的原因。
// 原因里的路径一律用相对 workspace 的写法（见 displayOf）。
func fileCheckReason(label, absolute, relative string) (string, bool) {
	if absolute == "" {
		return fmt.Sprintf("profile has no %s file configured", label), false
	}
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("%s file %q does not exist", label, displayOf(relative, absolute)), false
		}
		return fmt.Sprintf("%s file %q cannot be read: %v", label, displayOf(relative, absolute), err), false
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("%s file %q is not a regular file", label, displayOf(relative, absolute)), false
	}
	return "", true
}

func sortedExtensions() []string {
	allowed := make([]string, 0, len(windowsExecutableExts))
	for ext := range windowsExecutableExts {
		allowed = append(allowed, ext)
	}
	sort.Strings(allowed)
	return allowed
}

// displayOf 优先使用相对 workspace 的写法，避免把服务器目录结构写进给用户看的原因里。
func displayOf(relative, absolute string) string {
	if relative != "" {
		return relative
	}
	return filepath.Base(absolute)
}
