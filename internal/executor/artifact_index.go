package executor

import (
	"os"

	"github.com/jiujuan/godelayq/internal/core"
)

// ArtifactIndexer 是产物索引的读写能力，由 store/observe 的产物索引实现。
//
// 它是可选依赖：nil 表示不维护索引，产物的写入、读取、清理路径与本能力存在之前完全一致。
// 定位是加速器而不是账本（设计文档 D8）：文件才是权威，索引里缺一行只意味着
// GET /jobs/:id/artifacts 看不见它，正文照样能读。所以本接口的错误一律由调用方记日志，
// 不参与执行结果的判定。
//
// 新增一种 Kind 时必须同时接索引：写入点现在是两个（进程执行器与 HTTP 执行器各一处），
// 漏掉的那一档会在列表里整个消失，而结果端点仍然能读到它的产物，看起来像数据丢了。
type ArtifactIndexer interface {
	// Record 登记一次执行。同一 (job_id, attempt) 再次 Record 是覆盖：清理后再读、
	// 或者同一个任务被重复跑到同一个 attempt 都会二次触达这一行。
	Record(rec core.ArtifactIndexRecord) error
	// MarkPurged 把某一次尝试标成已清理。只标这一条：同一任务的其他尝试的产物可能还在，
	// 全标是假话。
	MarkPurged(jobID string, attempt int) error
	// MarkAllPurged 把一个任务的所有行标成已清理，供启动期对账使用（见 IndexReconciler）。
	MarkAllPurged(jobID string) error
	// DeleteByJob 删除一个任务的全部行，在产物目录删除成功之后调用。
	DeleteByJob(jobID string) error
	// List 按 attempt 升序返回一个任务的全部行。
	List(jobID string) ([]core.ArtifactIndexRecord, error)
	// Exists 判断索引里有没有这一次尝试。
	Exists(jobID string, attempt int) (bool, error)
}

// IndexReconciler 是启动期对账能力，ArtifactIndexer 的实现可以选择支持它。
//
// 需要它的原因是两侧会在重启之间脱节：本能力上线之前落盘的产物没有索引行，
// 而本能力之后被手工删掉的目录会留下说"文件还在"的行。只有启动那一次能同时看到两边，
// 所以目录是否存在由产物存储告诉实现（实现不去猜文件系统布局）。
//
// 对账只标注、不删除：文件被手工放回时行还在，本期不做"把 purged 纠正回 available"。
type IndexReconciler interface {
	ReconcileMissing(dirExists func(jobID string) bool) (int, error)
}

// SetIndex 挂上产物索引。必须在 Start 与执行器注册之前调用：
// Start 的第一轮扫描要用它做对账，之后各 goroutine 只会读这个字段。
// 传 nil 与不调用等价：不维护索引。
func (a *ArtifactStore) SetIndex(idx ArtifactIndexer) { a.index = idx }

// Index 返回挂上的产物索引，没挂时是 nil。调用方判空之后再读。
func (a *ArtifactStore) Index() ArtifactIndexer {
	if a == nil {
		return nil
	}
	return a.index
}

// recordIndex 把一次执行的输出登记进索引。没挂索引时什么都不做。
//
// 登记失败只记一条 warn：文件已经落盘，摘要也已经写进任务快照，
// 索引缺一行的后果只是列表端点看不见它（D8）。让任务因为索引写不进去而失败，
// 等于把加速器升级成了执行路径上的依赖。
func (a *ArtifactStore) recordIndex(kind Kind, profileName string, info core.ArtifactInfo) {
	if a == nil || a.index == nil {
		return
	}
	if err := a.index.Record(core.ArtifactIndexRecord{
		JobID:   info.JobID,
		Attempt: info.Attempt,
		Kind:    string(kind),
		Profile: profileName,
		Info:    info,
	}); err != nil {
		a.logger.Warn("executor output was not added to the artifact index",
			"job_id", info.JobID, "attempt", info.Attempt, "kind", kind, "error", err)
	}
}

// deleteIndexRows 在产物目录删除成功之后清掉它的全部索引行。
//
// 调用时机固定在"目录已经删掉"之后：反过来会留下"索引说没有、文件还在"的状态，
// 而文件还在却看不见比残留行更难解释（残留行会被结果端点的 purged 标注纠正）。
// 删除行失败同样只记 warn：目录已经没了，列表里多一行残留会被读成已清理。
func (a *ArtifactStore) deleteIndexRows(jobID string) {
	if a == nil || a.index == nil {
		return
	}
	if err := a.index.DeleteByJob(jobID); err != nil {
		a.logger.Warn("artifact index rows were not deleted", "job_id", jobID, "error", err)
	}
}

// reconcileIndex 是启动对账：把"索引里有、目录已经不在"的行标成已清理。
//
// 实现没有支持 IndexReconciler（比如只做了写入的那一半）时这里什么都不做，
// 也不报错：对账修的是历史脱节，不是本次执行的正确性。
func (a *ArtifactStore) reconcileIndex() {
	if a == nil || a.index == nil {
		return
	}
	reconciler, ok := a.index.(IndexReconciler)
	if !ok {
		return
	}

	marked, err := reconciler.ReconcileMissing(a.dirExists)
	if err != nil {
		a.logger.Warn("artifact index reconciliation failed", "error", err)
		return
	}
	if marked > 0 {
		a.logger.Info("artifact index rows marked as purged",
			"count", marked, "reason", "the output directory is gone")
	}
}

// dirExists 判断某个任务的产物目录在不在。ID 不可信时按"不在"处理：
// 那种目录名本来也进不了对账（jobDirs 会跳过它），这里只回答一次 stat。
func (a *ArtifactStore) dirExists(jobID string) bool {
	dir, err := a.jobDir(jobID)
	if err != nil {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return info.IsDir()
}
