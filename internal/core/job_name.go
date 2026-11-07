package core

import (
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// MaxJobNameRunes 是任务名称的字符数上限，与档位名、分组名的上限对齐
// （profileNamePattern 见 executor_profile_store.go:22，groupNamePattern 见 group_store.go）。
// 按字符而不是按字节计：一个汉字占 3 个字节，按字节判会让三个字的名称就贴上上限。
const MaxJobNameRunes = 64

// jobNamePattern 是任务名称的合法字符集：汉字、英文字母、数字，长度 1..64 个字符。
//
// 任务名称是给人看的标签，不再参与处理函数的查找（查找键是 Job.HandlerKey，见 job.go），
// 但它会出现在这些地方，所以不能收任意字符串：
// 列表页与详情页的标题、事件时间线与实时推送的 job_name、日志、以及错误响应的 details。
// 因此空格、制表与换行、_ - . / \ : @ % 引号尖括号、全角空格与全角标点一律拒绝：
// 带这些字符的名字复制到命令行里会断开，带换行的名字会把日志与台账的行结构搅乱。
//
// 汉字用 \p{Han} 而不是硬编码区间：区间表要长期维护，且窄区间会漏扩展区的字。
var jobNamePattern = regexp.MustCompile(`^[\p{Han}A-Za-z0-9]{1,64}$`)

// ErrJobNameInvalid 表示任务名称不合规。
// 单独一个错误值是给调用方判"这是取值问题而不是内部错误"用的，与 ErrGroupNameInvalid 同一体例。
var ErrJobNameInvalid = errors.New("invalid job name")

// ValidateJobName 校验任务名称（给人看的标签）。
//
// 规则只有一份，放在 core：HTTP 层在写盘与入队之前调它，前端只是把同一条规则提前提示一次，
// 拒绝权在服务端。
//
// 前后空白不做"trim 之后放行"：带空格本身就不在这个字符集里，
// 而"看着合法只是多了个空格"的输入如果被静默收下，存进去的名字与用户输入就不是同一个字符串。
// （core/job.go 的 IsExecHandlerKey 对注册键容忍空白，那是另一件事：键在执行侧本来就会被 trim。）
func ValidateJobName(name string) error {
	if jobNamePattern.MatchString(name) {
		return nil
	}
	if name == "" {
		return fmt.Errorf("%w: must not be empty", ErrJobNameInvalid)
	}
	if count := utf8.RuneCountInString(name); count > MaxJobNameRunes {
		return fmt.Errorf("%w: %d characters, at most %d", ErrJobNameInvalid, count, MaxJobNameRunes)
	}

	// 走到这里长度是够的，问题在字符：只点名第一处非法字符，不回显整个名称
	// （名称里可能出现内部系统的名字，而这条错误文本会进响应体与日志）。
	r, index := firstInvalidNameRune(name)
	return fmt.Errorf("%w: character %q at position %d is not allowed; a job name may only contain "+
		"Chinese characters, letters and digits, %d-%d of them",
		ErrJobNameInvalid, string(r), index, 1, MaxJobNameRunes)
}

// firstInvalidNameRune 返回第一个不在合法字符集里的字符及其下标（按字符计，从 0 开始）。
//
// 判定条件与 jobNamePattern 必须一致，否则会出现"正则说不合法、这里却找不到非法字符"。
// 真找不到时返回 0：调用方已经先跑过正则，那种情况属于两条实现分叉，
// 由 job_name_test.go 的用例把边界钉住。
func firstInvalidNameRune(name string) (rune, int) {
	index := 0
	for _, r := range name {
		if isAllowedNameRune(r) {
			index++
			continue
		}
		return r, index
	}
	return 0, index
}

func isAllowedNameRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	}
	return unicode.Is(unicode.Han, r)
}
