package executor

import (
	"godelayq/core"
)

// Result 是一次执行在内存里的完整形态：结论摘要加上两条输出流。
//
// 它属于执行协程的局部对象：填完之后把 Meta 交给 Job.Exec，输出交给产物写入器（TASK-E06）。
// 本文件的两个方法只在内存里成型，"边写边截断"是 E06 写入器的职责。
type Result struct {
	// Meta 是随任务快照落盘的摘要，字段含义见 core.ExecMeta
	Meta core.ExecMeta
	// Stdout / Stderr 是采集到的输出内容（已受 max_bytes 约束）
	Stdout []byte
	Stderr []byte
}

// NewResult 造一个只填了 kind 与档位名的结果。
// 摘要里的这两个字段不该由调用方手写：写错一处，接口显示的就是别的档位。
func NewResult(profile *Profile) *Result {
	return &Result{
		Meta: core.ExecMeta{
			Kind:    string(profile.Kind),
			Profile: profile.Name,
		},
	}
}

// SetPreview 把输出尾部（最多 limit 字节）写进 Meta.Preview。
//
// 取哪条流：stderr 优先，它为空时才用 stdout。理由见 ChoosePreview。
// limit <= 0 表示不带预览，Preview 置空。
// 裁剪规则（含字符边界处理）见 core.TrimExecPreview。
func (r *Result) SetPreview(limit int) {
	r.Meta.Preview = ChoosePreview(r.Stdout, r.Stderr, limit)
}

// ChoosePreview 在两条流的尾部文本之间做选择并裁到 limit 字节。
//
// 执行器采集到的两条流都会完整落盘，摘要里只放得下一条，而"这一行字要解释为什么失败"：
// 脚本失败时最有用的信息几乎都在 stderr，所以它优先，stdout 只在 stderr 为空时补位。
// 进程根本没跑起来时两条都为空，预览也就为空——错误文本在 ExitError 里，不在这里。
//
// 入参是已经取好的尾部（执行侧从产物文件反向读 limit 字节），本函数只负责选一条再裁一次。
// limit <= 0 返回空串。
func ChoosePreview(stdout, stderr []byte, limit int) string {
	if limit <= 0 {
		return ""
	}

	source := stderr
	if len(source) == 0 {
		source = stdout
	}
	return core.TrimExecPreview(string(source), limit)
}

// TrimPreview 返回 text 尾部最多 limit 字节，起点落在字符边界上；limit <= 0 返回空串。
//
// 规则本身在 core（core.TrimExecPreview）：执行侧写预览、事件侧再裁一次、接口侧透出去之前
// 还要再裁一次，三处必须算出同样长的字符串，各写一遍迟早分叉。
// 这里保留导出名，api 层照旧调它。
func TrimPreview(text string, limit int) string {
	return core.TrimExecPreview(text, limit)
}

// Truncate 把两条流各自裁剪到 maxBytes 并记账：
// OutBytes / ErrBytes 写入的是裁剪后的长度，与产物文件里的内容一致，
// 任何一条被裁短则 Meta.Truncated 置真，表示"还有输出没采到"。
//
// 保留开头、丢掉结尾，与 E06 写入器的行为一致（到达上限即停止写入）。
// maxBytes <= 0 表示不裁剪，只记账。
func (r *Result) Truncate(maxBytes int) {
	if maxBytes > 0 {
		if len(r.Stdout) > maxBytes {
			r.Stdout = r.Stdout[:maxBytes]
			r.Meta.Truncated = true
		}
		if len(r.Stderr) > maxBytes {
			r.Stderr = r.Stderr[:maxBytes]
			r.Meta.Truncated = true
		}
	}

	r.Meta.OutBytes = int64(len(r.Stdout))
	r.Meta.ErrBytes = int64(len(r.Stderr))
}
