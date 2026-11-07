package core

import (
	"errors"
	"fmt"
	"time"
)

// ArtifactInfo 是一次执行产物的落盘结论，由产物写入器的 Close 返回给执行侧填进 ExecMeta。
// 类型放在 core 是共享词汇的惯例：executor 负责产出它，
// store/observe 的产物索引与 api 的结果端点都要读它的形状，互相不 import。
type ArtifactInfo struct {
	JobID     string `json:"job_id"`
	Attempt   int    `json:"attempt"`
	OutPath   string `json:"out_path"`
	ErrPath   string `json:"err_path"`
	OutBytes  int64  `json:"out_bytes"`
	ErrBytes  int64  `json:"err_bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ArtifactIndexRecord 是一次执行的输出登记项，一行对应一个 (job_id, attempt)。
// 消费方接口（ArtifactIndexer / IndexReconciler）定义在 executor 侧，
// 索引的持久化在 store/observe；两边只认这里的行形状，互相不 import。
//
// Kind 与 Profile 由执行器给出：ArtifactStore 只管文件，不认识档位，
// 而列表端点要回答的"这是哪条档位跑出来的"只有执行侧知道。
// 字节数与截断标记来自 ArtifactInfo，也就是 writer.Close() 那份结论——
// 索引与任务摘要必须同源，不能各算一套。
type ArtifactIndexRecord struct {
	JobID   string
	Attempt int
	Kind    string // script|binary|http
	Profile string // 档位名，不含 exec. 前缀
	Info    ArtifactInfo

	// State 与 CreatedAt 只有读的时候才需要由实现填写：写入时状态恒为
	// ArtifactAvailable（文件刚刚写完），时间取实现自己的时间源。
	// 调用方传进来的这两个字段在 Record 里被忽略。
	State     string
	CreatedAt time.Time

	// OutRel 与 ErrRel 同理，只在读取结果里有意义：库里存的是相对产物根目录的写法，
	// 绝对路径不入库（与 Profile.ProgramDisplay() 不把服务器目录结构透给前端同一取向）。
	// Info.OutPath/ErrPath 保持为空——那两个是要交给 os 打开的路径，
	// 拿相对写法去开文件会指向错误的目录。
	OutRel string
	ErrRel string
}

// CheckArtifactJobID 拒绝任何可能被当成路径写法的任务 ID。产物目录的拼装（executor）
// 与产物索引的写库、按 ID 查询（store/observe）用同一个判断，不另写一套规则。
//
// 现实里的 ID 是 UUIDv7 文本形式，只含小写十六进制与连字符；但目录名来自调用方
// （自行接入的程序、测试、以及未来任何写入路径），所以这里不依赖"上游一定安全"：
// 分隔符、点号（含 `..`）、绝对路径前缀与任何非 ASCII 都挡掉。
func CheckArtifactJobID(jobID string) error {
	if jobID == "" {
		return errors.New("artifact: job id is empty")
	}
	if len(jobID) > 128 {
		return fmt.Errorf("artifact: job id too long (%d characters)", len(jobID))
	}
	for _, r := range jobID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			// 允许字母、数字与连字符，其余（`.` `/` `\` `:` 等）一律拒绝
		default:
			return fmt.Errorf("artifact: job id %q contains an unusable character %q", jobID, r)
		}
	}
	return nil
}
