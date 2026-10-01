package executor

import (
	"strings"
	"testing"

	"godelayq/core"
)

// 档位名的规则在两个包里各有一份：core 存页面上建出来的档位，executor 校验并注册它们，
// 而 core 不许 import executor（executor 依赖 core）。
// 这条用例把两边钉在一起，让"改了一边忘了另一边"在测试阶段就炸，
// 而不是等到页面上存进一条启动时被拒收的档位——那时中间还夹着一次重启。
func TestProfileNameRulesMatchCore(t *testing.T) {
	samples := []string{
		"a", "A", "0", "nightly_report", "ping-2", "a_b-c9",
		strings.Repeat("x", 64),
		"", " ", "with space", "a/b", `a\b`, "a.b", "a:b", "a$b", "日-志", "点",
		strings.Repeat("x", 65), "_", "-lead", "exec.hello", "a\tb", "a\nb",
	}

	for _, name := range samples {
		matched := profileNamePattern.MatchString(name)
		err := core.ValidateProfileName(name)

		if matched != (err == nil) {
			t.Errorf("档位名 %q：executor 侧接受 %v，core 侧接受 %v（err=%v），两边必须一致",
				name, matched, err == nil, err)
		}
	}
}
