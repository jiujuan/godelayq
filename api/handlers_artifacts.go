package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/executor"
)

// ArtifactEntry 是产物列表里的一次执行。字段就是索引表的列，
// 不含输出正文——正文另有 GET /jobs/:id/result，那道门槛比这里严（见 handler 注释）。
type ArtifactEntry struct {
	Attempt   int       `json:"attempt" example:"1"`
	Kind      string    `json:"kind" example:"script"`
	Profile   string    `json:"profile" example:"hello"`
	OutBytes  int64     `json:"out_bytes" example:"42"`
	ErrBytes  int64     `json:"err_bytes" example:"512"`
	Truncated bool      `json:"truncated"`
	State     string    `json:"state" example:"available"`
	CreatedAt time.Time `json:"created_at"`
}

// JobArtifactsResponse GET /api/v1/jobs/:id/artifacts 的响应体。
// Count 是本次返回的条数，与该任务的索引行数相同（这里没有分页）。
type JobArtifactsResponse struct {
	JobID string          `json:"job_id" example:"0198a2e3-7d4f-7abc-9def-0123456789ab"`
	Count int             `json:"count" example:"2"`
	Items []ArtifactEntry `json:"items"`
}

// artifactIndex 取这次部署挂上的产物索引，没挂时是 nil。
// 索引挂在产物存储上而不是服务端字段上：写入方（执行器）与读取方（这里）
// 拿的是同一份可选依赖，装配点只有一处。
func (s *Server) artifactIndex() executor.ArtifactIndexer {
	return s.artifacts.Index()
}

// requireIndex 在没挂产物索引的部署里挡住列表端点。
//
// 必须是 503 而不是"回一份空列表"：空列表说的是"这个任务没有产物记录"，
// 而真实情况是这台服务器压根没在记——两者在界面上长得一样，排查方向却完全不同。
// 与 requireArtifacts 同一取向，两个守卫并列放在这里，判定条件差一层：
// 产物存储是文件读写的依赖，索引只是它上面的一层可选记录。
func (s *Server) requireIndex() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.artifactIndex() == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
				Code:    http.StatusServiceUnavailable,
				Message: "artifact index is not configured",
				Details: "start the server with executors.enabled and observability.artifacts.enabled to enable /api/v1/jobs/:id/artifacts",
			})
			return
		}
		c.Next()
	}
}

// ListJobArtifacts GET /api/v1/jobs/:id/artifacts
//
// 档位是 reader（viewer 及以上）：这里出去的是元信息——第几次尝试、多大、有没有截断、
// 状态如何，不含输出正文。正文端点的门槛更严（档位声明了 secret 参数时会把读取门槛
// 提到提交档位，见 GetJobResult 里的 resultGuard），所以不要拿这个端点当读的捷径。
//
// 任务没有任何索引行时是 200 + 空列表，不是 404：与事件端点同一口径，
// "没有记录"是正常状态。本端点不查任务是否存在（那要读任务存储，与这张表无关）。
func (s *Server) ListJobArtifacts(c *gin.Context) {
	jobID := c.Param("id")
	if err := executor.CheckArtifactJobID(jobID); err != nil {
		// 这个 ID 连产物目录名都当不了，任务存储里也不会有它：与其让库那一侧的
		// 同一套校验把请求打成 500（写着"服务器坏了"），不如当场说清是哪一段不对。
		s.respondBadParam(c, "id", jobID, err.Error())
		return
	}

	rows, err := s.artifactIndex().List(jobID)
	if err != nil {
		// 与事件端点的库读失败同一口径：报错，不静默退回"扫目录"或空列表
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to load artifact records",
			Details: err.Error(),
		})
		return
	}

	items := make([]ArtifactEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, ArtifactEntry{
			Attempt:   row.Attempt,
			Kind:      row.Kind,
			Profile:   row.Profile,
			OutBytes:  row.Info.OutBytes,
			ErrBytes:  row.Info.ErrBytes,
			Truncated: row.Info.Truncated,
			State:     row.State,
			CreatedAt: row.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, JobArtifactsResponse{
		JobID: jobID,
		Count: len(items),
		Items: items,
	})
}

// markArtifactIndexed 在产物状态被回写成 purged 之后，同步标注索引。
//
// 标注的是这一次读取的 attempt 而不是整个任务：其他尝试的文件可能还在，
// 全标 purged 是假话（同一任务的重试链常常一次有输出、一次被截断或为空）。
// 索引没挂时不做任何事；标注失败只记日志——快照那份结论已经写成功了，
// 索引慢一拍不影响正文读不到这个事实。
func (s *Server) markArtifactIndexed(jobID string, attempt int) {
	index := s.artifactIndex()
	if index == nil {
		return
	}
	if err := index.MarkPurged(jobID, attempt); err != nil {
		s.logger.Warn("failed to mark the artifact index row as purged",
			"job_id", jobID, "attempt", attempt, "error", err)
	}
}
