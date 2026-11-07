package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestValidateJobName 逐条钉住设计文档 §3.2 的判据：卡面每条对应一个表项或一个子测试，
// 断言消息里写明它守的是哪一条，方便返工时对着卡片找用例。
func TestValidateJobName(t *testing.T) {
	// §3.2 第 1 条：字符集是汉字、字母、数字。
	allowed := []struct {
		desc string
		name string
	}{
		{"纯中文", "每晚对账"},
		{"纯小写字母", "nightlyreconcile"},
		{"纯大写字母", "ABC"},
		{"纯数字", "20261006"},
		{"三者混合", "对账Job01"},
		{"单个字符", "A"},
		// §3.2 第 2 条：按字符计，三个汉字（9 字节）必须通过。
		{"三个汉字", "对账组"},
	}
	for _, item := range allowed {
		t.Run("通过/"+item.desc, func(t *testing.T) {
			if err := ValidateJobName(item.name); err != nil {
				t.Errorf("ValidateJobName(%q) = %v, want nil", item.name, err)
			}
		})
	}

	// §3.2 第 4 条：拒绝清单里的写法逐个都要被拒，且各自是独立的一条取值
	// （合并成一条字符串会让"哪个字符被拒"看不出来）。
	rejected := []struct {
		desc string
		name string
	}{
		{"空串", ""},
		{"含空格", "a b"},
		{"前导空格", " 白天巡检"},
		{"尾随空格", "白天巡检 "},
		{"含制表符", "a\tb"},
		{"含换行", "a\nb"},
		{"下划线", "payment_check"},
		{"连字符", "demo-run"},
		{"点", "exec.php"},
		{"正斜杠", "a/b"},
		{"反斜杠", `a\b`},
		{"冒号", "a:b"},
		{"at", "a@b"},
		{"百分号", "a%b"},
		{"双引号", `a"b`},
		{"尖括号", "<script>"},
		{"全角空格", "a　b"},
		{"全角标点", "对账，完成"},
		{"emoji", "对账🙂"},
		{"假名不在汉字区", "あ"},
	}
	for _, item := range rejected {
		t.Run("拒绝/"+item.desc, func(t *testing.T) {
			err := ValidateJobName(item.name)
			if err == nil {
				t.Fatalf("ValidateJobName(%q) = nil, want error", item.name)
			}
			// §3.3：调用方要能用 errors.Is 判出"这是取值问题"。
			if !errors.Is(err, ErrJobNameInvalid) {
				t.Errorf("error %v does not wrap ErrJobNameInvalid", err)
			}
		})
	}

	// §3.2 第 2 条：长度按字符计而不是按字节计。
	t.Run("长度按字符计", func(t *testing.T) {
		if err := ValidateJobName(strings.Repeat("汉", MaxJobNameRunes)); err != nil {
			t.Errorf("64 个汉字应当合法，实际 %v", err)
		}
		err := ValidateJobName(strings.Repeat("汉", MaxJobNameRunes+1))
		if err == nil {
			t.Fatal("65 个汉字必须被拒")
		}
		if !strings.Contains(err.Error(), fmt.Sprint(MaxJobNameRunes+1)) {
			t.Errorf("错误文本要给出实际字符数（按字节会算出 %d），实际 %q",
				MaxJobNameRunes+1, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprint(MaxJobNameRunes)) {
			t.Errorf("错误文本要给出上限，实际 %q", err)
		}
	})

	// §3.2 第 4 条的另一半：点名第一处非法字符，但不回显整个名称。
	t.Run("错误文本只点名第一处", func(t *testing.T) {
		name := "对账脚本" + "_" + "A" + "%"
		err := ValidateJobName(name)
		if err == nil {
			t.Fatal("含下划线与百分号的名称必须被拒")
		}
		if !strings.Contains(err.Error(), `"_"`) {
			t.Errorf("错误文本应点名第一处非法字符 _，实际 %q", err)
		}
		if strings.Contains(err.Error(), "%") {
			t.Errorf("错误文本不该出现后面那个字符（只点名第一处），实际 %q", err)
		}
		if strings.Contains(err.Error(), "对账脚本") {
			t.Errorf("错误文本不该回显完整名称，实际 %q", err)
		}
		if !strings.Contains(err.Error(), "position 4") {
			t.Errorf("错误文本应给出被拒字符的下标（按字符计，_ 在第 5 个即下标 4），实际 %q", err)
		}
	})

	// §3.2 第 6 条：本函数不管 exec. 前缀语义，execphp 是一个合法标签。
	t.Run("标签与注册键互不相干", func(t *testing.T) {
		if err := ValidateJobName("execphp"); err != nil {
			t.Errorf("execphp 是合法标签，实际 %v", err)
		}
	})
}

// TestJobNameCharsetAgreesWithPattern 钉住 firstInvalidNameRune 与正则的字符集一致性：
// 两者分叉会让错误文本退化成"位置 0 的 NUL"这种没人能读的说法
// （长度规则不在本用例范围内，所以下面每个样本都落在 1..64 个字符里，
// 那时"正则通过"与"全部字符合法"是同一件事）。
func TestJobNameCharsetAgreesWithPattern(t *testing.T) {
	samples := []string{" ", "a", "对", "a b", "a_b", "a\tb", "对账 组", "a。b", "a🙂b", "ABC123对"}
	for _, name := range samples {
		patternOK := jobNamePattern.MatchString(name)

		allRunesOK := true
		for _, r := range name {
			if !isAllowedNameRune(r) {
				allRunesOK = false
				break
			}
		}

		if patternOK != allRunesOK {
			t.Errorf("正则判 %q 为 %v，逐字符判为 %v：两份实现必须一致", name, patternOK, allRunesOK)
		}
	}
}
