package executor

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"godelayq/core"
)

// minimumEnvKeys 是无论白名单怎么写都要透给子进程的变量。
//
// 后三个是 Windows 上进程能否启动的前提：缺 SystemRoot 时子进程里的系统调用会失败，
// 缺 COMSPEC 时 cmd.exe 之类的程序找不到自己的 shell，缺 PATHEXT 时带扩展名的程序名解析不了。
// 它们不属于凭据，也不因平台而异地影响 Unix（那里根本没有这几个键），
// 所以这张表是平台无关的；在 Unix 上顶多白捡几个不存在的键。
//
// 结论：把它们写进 executors.env_allow 是正常配置，不是安全漏洞。
var minimumEnvKeys = []string{"PATH", "SystemRoot", "COMSPEC", "PATHEXT"}

// envVar 是一条环境变量的最终形态：名字用写入方给的写法，值随它一起。
type envVar struct {
	name  string
	value string
}

// BuildEnv 重建子进程的环境变量。
//
// 这里返回的一定是非 nil 切片，哪怕是空档位：os/exec 把 nil 解释成"继承当前进程的全部环境变量"，
// 那等于把服务凭据交给子进程（本包的口径：进程环境里有 GODELAYQ_ 前缀的密钥与 token）。
//
// 三层来源，后一层覆盖前一层：
//  1. 当前进程的环境变量，只保留 executors.env_allow 与最低必要集里的键；
//  2. 档位固定的 env（配置里写死的值）；
//  3. payload 注入的 env（已在 E08 过档位的 env_allow，且不允许覆盖第 2 层的键）。
//
// 输出按键名字典序排列且同名只留一条：os/exec 对重复键的行为依赖平台，
// 顺序稳定也让测试与人工比对（"这台机器到底传了什么进去"）成为可能。
func BuildEnv(cfg core.ExecutorsConfig, p *Profile, sub *Submission) []string {
	inherited := allowSet(cfg.EnvAllow)
	merged := make(map[string]envVar, len(p.Env)+len(inherited))

	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if hasCredentialPrefix(name) {
			// 配置写错也不该把服务凭据传下去，所以这一层排除不看白名单
			continue
		}
		if !inherited[envKeyFold(name)] && !inMinimumEnvKeys(name) {
			continue
		}
		merged[envKeyFold(name)] = envVar{name: name, value: value}
	}

	// 档位固定值：键名在 E02 已过大写校验，这里只做同样的凭据排除
	for name, value := range p.Env {
		if hasCredentialPrefix(name) {
			continue
		}
		merged[envKeyFold(name)] = envVar{name: name, value: value}
	}

	if sub != nil {
		for name, value := range sub.Env {
			if hasCredentialPrefix(name) {
				continue
			}
			merged[envKeyFold(name)] = envVar{name: name, value: value}
		}
	}

	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		item := merged[key]
		env = append(env, fmt.Sprintf("%s=%s", item.name, item.value))
	}
	return env
}

// allowSet 把白名单折成查找表。
func allowSet(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, key := range list {
		set[envKeyFold(key)] = true
	}
	return set
}

func inMinimumEnvKeys(name string) bool {
	folded := envKeyFold(name)
	for _, key := range minimumEnvKeys {
		if envKeyFold(key) == folded {
			return true
		}
	}
	return false
}

// envKeyFold 给出用于比较与去重的键名形态。
//
// Windows 的环境变量名不区分大小写（os.Environ() 里是 SystemRoot，配置里可能写 SYSTEMROOT），
// 比较时统一折成小写；Unix 区分大小写，PATH 与 Path 是两个变量，所以保持原样。
func envKeyFold(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}
