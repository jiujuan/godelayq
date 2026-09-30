package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/core"
	"godelayq/executor"
)

// 结果端点的两个读取量上限。
const (
	// maxResultReadBytes 是单次请求能读出的正文上限，与配置无关。
	// 依据见 TASK-E07 §9：`executors.output.max_bytes` 可以被配到几十 MB，
	// 而读取发生在请求协程里，所以端点自己也要有一个不随配置变大的上限；
	// 请求超过它不是"少读一点"，直接拒绝并把上限说清楚更诚实。
	maxResultReadBytes = 8 << 20
	// resultDefaultReadFactor 是没带 max_bytes 时的读取量：预览上限的 4 倍。
	// 结果面板默认展示尾部，4 倍够看清出错前的上下文，又不会一次把整份输出拉进内存。
	resultDefaultReadFactor = 4
)

// JobResultResponse GET /api/v1/jobs/:id/result。
//
// 摘要（Meta）与正文（Content）在同一个响应里，但来源不同：摘要来自任务快照，
// 正文来自产物文件。文件被清理时摘要照旧给出，只是 found=false、content 为空。
type JobResultResponse struct {
	JobID   string `json:"job_id"`
	Attempt int    `json:"attempt"`
	Stream  string `json:"stream"`
	// Found 表示这次尝试的产物文件在不在。false 不等于"输出是空的"，
	// 也不等于"任务没跑过"：三者由 status code、meta 是否为 null 与 found 共同区分。
	Found         bool           `json:"found"`
	SizeBytes     int64          `json:"size_bytes"`
	ReturnedBytes int            `json:"returned_bytes"`
	Truncated     bool           `json:"truncated"`
	Meta          *core.ExecMeta `json:"meta"`
	Content       string         `json:"content"`
}

// GetJobResult GET /api/v1/jobs/:id/result
// 读的是产物文件，不进任何内存缓冲：几十 KB 的输出一旦被事件带走，
// 实时通道和事件历史都会按输出体积失控。
//
// 档位：本卡一律 reader（viewer 及以上）。TASK-E16 的判档点就在取到快照之后——
// 那次判断要看档位是否声明了 secret 参数，而档位名在快照摘要里。
func (s *Server) GetJobResult(c *gin.Context) {
	id := c.Param("id")

	snapshot, found, err := s.snapshotOf(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to load jobs",
			Details: err.Error(),
		})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, ErrorResponse{Code: http.StatusNotFound, Message: "job not found"})
		return
	}

	stream := c.Query("stream")
	if stream == "" {
		stream = "out"
	}
	if stream != "out" && stream != "err" {
		s.respondBadParam(c, "stream", stream, "use out or err")
		return
	}

	from := c.Query("from")
	if from == "" {
		from = "tail"
	}
	if from != "tail" && from != "head" {
		s.respondBadParam(c, "from", from, "use tail or head")
		return
	}

	attempt, err := parseResultAttempt(c.Query("attempt"), snapshot.Attempts)
	if err != nil {
		s.respondBadParam(c, "attempt", c.Query("attempt"), err.Error())
		return
	}

	maxBytes, err := s.parseResultMaxBytes(c.Query("max_bytes"))
	if err != nil {
		s.respondBadParam(c, "max_bytes", c.Query("max_bytes"), err.Error())
		return
	}

	// 输出正文可能是几百 KB，也可能是脚本回显的业务数据，因此这条响应不缓存。
	c.Header("Cache-Control", "no-store")

	// 正文读不出来时的响应：摘要照旧给出，content 为空，并把快照里的产物状态改成 purged。
	respondMissing := func() {
		response := JobResultResponse{
			JobID:   id,
			Attempt: attempt,
			Stream:  stream,
			Meta:    s.markArtifactPurged(snapshot),
			Found:   false,
			Content: "",
		}
		c.JSON(http.StatusOK, response)
	}

	size, statErr := s.artifacts.Stat(id, attempt, stream)
	if statErr != nil {
		if errors.Is(statErr, executor.ErrArtifactMissing) {
			if snapshot.Exec == nil {
				// 既没有摘要也没有文件：这不是执行器任务，或者它还没跑过。
				c.JSON(http.StatusNotFound, ErrorResponse{
					Code:    http.StatusNotFound,
					Message: "no execution result for this job",
				})
				return
			}
			respondMissing()
			return
		}
		s.respondArtifactError(c, statErr)
		return
	}

	var content []byte
	var truncated bool
	if from == "tail" {
		content, truncated, err = s.artifacts.Tail(id, attempt, stream, int64(maxBytes))
	} else {
		content, truncated, err = s.artifacts.Read(id, attempt, stream, int64(maxBytes))
	}
	if err != nil {
		if errors.Is(err, executor.ErrArtifactMissing) {
			// 文件在这次请求中间被清理协程删掉了：按同一套降级回答，不报 500。
			respondMissing()
			return
		}
		s.respondArtifactError(c, err)
		return
	}

	c.JSON(http.StatusOK, JobResultResponse{
		JobID:         id,
		Attempt:       attempt,
		Stream:        stream,
		Found:         true,
		SizeBytes:     size,
		ReturnedBytes: len(content),
		Truncated:     truncated,
		Meta:          s.execForResponse(snapshot.Exec),
		Content:       string(content),
	})
}

// markArtifactPurged 返回要对外给出的摘要，并在产物已缺失时把状态回写成 purged。
//
// 这是结果端点读路径上唯一一处写，后来者请不要再往这个函数里加写操作。
// 理由：带摘要却没有文件的任务会被反复查询（详情页与事件时间线都指向它），
// 每次都重新撞一次"文件不存在"再重新解释一遍不值得，回写一次就把结论存下来。
// 已经是 purged 时不再写，所以每个任务每个尝试最多写一次。
func (s *Server) markArtifactPurged(snapshot core.JobSnapshot) *core.ExecMeta {
	if snapshot.Exec == nil {
		return nil
	}
	if snapshot.Exec.Artifact == core.ArtifactPurged {
		return s.execForResponse(snapshot.Exec)
	}

	// 复制一份再改：LoadAll 返回的快照与存储内部共用同一个摘要指针，
	// 直接改它会和落盘协程的读法撞在一起。
	summary := *snapshot.Exec
	summary.Artifact = core.ArtifactPurged
	updated := snapshot
	updated.Exec = &summary

	if err := s.store.Update(updated); err != nil {
		s.logger.Warn("failed to record that the execution output is gone",
			"job_id", snapshot.ID, "attempt", snapshot.Attempts, "error", err)
	}
	return s.execForResponse(&summary)
}

// respondArtifactError 把产物存储的读取失败说成一句能用的话：
// 原始错误带目录结构，只进日志与 details，不进 message。
func (s *Server) respondArtifactError(c *gin.Context, err error) {
	s.logger.Error("failed to read the execution output", "error", err)
	c.JSON(http.StatusInternalServerError, ErrorResponse{
		Code:    http.StatusInternalServerError,
		Message: "failed to read the execution output",
		Details: err.Error(),
	})
}

func (s *Server) respondBadParam(c *gin.Context, name, got, reason string) {
	c.JSON(http.StatusBadRequest, ErrorResponse{
		Code:    http.StatusBadRequest,
		Message: "invalid " + name,
		Details: fmt.Sprintf("got %q: %s", got, reason),
	})
}

// parseResultAttempt 解析 attempt：0 或省略表示最近一次已结束的尝试（取快照的 Attempts）。
// 显式传值必须落在 1..Attempts：产物按尝试分文件，越界就是读了个不存在的编号。
func parseResultAttempt(raw string, attempts int) (int, error) {
	if raw == "" {
		return attempts, nil
	}

	attempt, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("attempt must be a number, got %q", raw)
	}
	if attempt == 0 {
		return attempts, nil
	}
	if attempt < 1 || attempt > attempts {
		return 0, fmt.Errorf("attempt %d is out of range, this job has %d attempt(s)", attempt, attempts)
	}
	return attempt, nil
}

// parseResultMaxBytes 解析 max_bytes。
//
// 三层：省略时取预览上限的 4 倍；显式取值必须是正数且不超过端点硬上限；
// 最后夹到落盘上限（文件不可能比 executors.output.max_bytes 更大，多要的部分不存在）。
func (s *Server) parseResultMaxBytes(raw string) (int, error) {
	ceiling := s.artifacts.MaxBytes()
	if maxResultReadBytes < ceiling {
		ceiling = maxResultReadBytes
	}

	if raw == "" {
		value := s.execPreviewLimit() * resultDefaultReadFactor
		if value > ceiling {
			value = ceiling
		}
		if value < 0 {
			value = ceiling
		}
		return value, nil
	}

	requested, err := strconv.Atoi(raw)
	if err != nil || requested <= 0 {
		return 0, fmt.Errorf("max_bytes must be a positive number, got %q", raw)
	}
	if requested > maxResultReadBytes {
		return 0, fmt.Errorf("max_bytes %d is above the %d byte per-request limit of this endpoint",
			requested, maxResultReadBytes)
	}
	if requested > ceiling {
		return ceiling, nil
	}
	return requested, nil
}

// ExecutorArgResponse 是档位的一个参数声明，供提交表单与提交期校验使用。
type ExecutorArgResponse struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Default  string `json:"default"`
	Pattern  string `json:"pattern"`
	Secret   bool   `json:"secret"`
}

// ExecutorProfileResponse 是一个档位的对外形状。
//
// 刻意不含的东西：env 的固定值（那是配置里的凭据）、脚本与产物的绝对路径
// （只给相对 workspace 的写法）、以及任何解释器的安装路径。
type ExecutorProfileResponse struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// RuntimeOK 是这台机器现在的探测结论：程序在不在 PATH、文件在不在。
	// false 时提交在 TASK-E16 §3.2 就被拒掉，前端可以提前把这条档位标灰。
	RuntimeOK bool   `json:"runtime_ok"`
	Reason    string `json:"reason"`
	Timeout   string `json:"timeout"`
	// MaxParallel 是该档位同时最多跑几个。0 已在加载时归一为 1。
	MaxParallel int                   `json:"max_parallel"`
	Args        []ExecutorArgResponse `json:"args"`
	EnvAllow    []string              `json:"env_allow"`
	// URL 只在 http 档位出现，给的是模板原文（含 {占位符}），不是渲染后的地址。
	URL string `json:"url,omitempty"`
}

// ListExecutorsResponse GET /api/v1/executors。
type ListExecutorsResponse struct {
	Enabled  bool                      `json:"enabled"`
	Profiles []ExecutorProfileResponse `json:"profiles"`
	// RequiredRole 是提交执行器任务所需的最低档位名。本卡固定为 null：
	// 取值已经在 executor.Registry.RequiredRole() 里备好，但把它接进提交路径
	// 属于 TASK-E16（那一卡同时要做参数校验与掩码）。在这里写明去向，避免两张卡都以为对方做了。
	RequiredRole *string `json:"required_role"`
}

// ListExecutors GET /api/v1/executors
//
// 没有 503 守卫：执行器默认关闭，"没装配登记表"是默认状态而不是错误状态
// （口径见 docs/design/executor-design.md §6.1）。
func (s *Server) ListExecutors(c *gin.Context) {
	response := ListExecutorsResponse{
		Enabled:  false,
		Profiles: []ExecutorProfileResponse{},
	}
	if s.executors == nil {
		c.JSON(http.StatusOK, response)
		return
	}

	response.Enabled = s.executors.Enabled()
	for _, profile := range s.executors.Profiles() {
		reason, ok := s.executors.Available(profile.HandlerKey())
		response.Profiles = append(response.Profiles, toExecutorProfile(profile, reason, ok))
	}
	c.JSON(http.StatusOK, response)
}

func toExecutorProfile(profile *executor.Profile, reason string, runtimeOK bool) ExecutorProfileResponse {
	args := make([]ExecutorArgResponse, 0, len(profile.Args))
	for _, arg := range profile.Args {
		args = append(args, ExecutorArgResponse{
			Name:     arg.Name,
			Required: arg.Required,
			Default:  arg.Default,
			Pattern:  arg.PatternText,
			Secret:   arg.Secret,
		})
	}

	// EnvAllow 是变量名白名单，取值一律不外露；空表也要给 []，
	// 让前端不必区分"没有这项"与"这项是空的"。
	envAllow := make([]string, 0, len(profile.EnvAllow))
	envAllow = append(envAllow, profile.EnvAllow...)

	item := ExecutorProfileResponse{
		Key:         profile.HandlerKey(),
		Name:        profile.Name,
		Kind:        string(profile.Kind),
		RuntimeOK:   runtimeOK,
		Reason:      reason,
		Timeout:     profile.Timeout.String(),
		MaxParallel: profile.MaxParallel,
		Args:        args,
		EnvAllow:    envAllow,
	}
	if profile.Kind == executor.KindHTTP {
		item.URL = profile.URLTemplate
	}
	return item
}

// snapshotOf 按 ID 取一份任务快照，取法与 GetJob 相同：存储是一张 map，只能整份读。
func (s *Server) snapshotOf(jobID string) (core.JobSnapshot, bool, error) {
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return core.JobSnapshot{}, false, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == jobID {
			return snapshot, true, nil
		}
	}
	return core.JobSnapshot{}, false, nil
}

// requireArtifacts 在没装配产物存储的部署里挡住结果端点。
//
// 这里必须是 503 而不是"回一份空正文"：任务的摘要可能还在快照里，
// 把"读不到正文"说成"正文是空的"会让排障的人往错的方向查。
func (s *Server) requireArtifacts() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.artifacts == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
				Code:    http.StatusServiceUnavailable,
				Message: "execution output storage is not configured",
				Details: "start the server with api.WithArtifacts to enable /api/v1/jobs/:id/result",
			})
			return
		}
		c.Next()
	}
}

// ---- 提交期判定与敏感参数输出（TASK-E16）----

// submissionRejected 是提交期判定在 core 的回调里给出的拒绝结论。
//
// UpdatePending 的 apply 只能返回 error，而这里要返回的是带状态码与文案的 ErrorResponse，
// 所以用一个类型把那个结论带出来，处理器侧用 errors.As 认它（见 api/handlers.go 的 UpdateJob）。
type submissionRejected struct {
	failure *ErrorResponse
}

func (e *submissionRejected) Error() string {
	if e.failure.Details == "" {
		return e.failure.Message
	}
	return e.failure.Message + ": " + e.failure.Details
}

// executorRole 返回提交执行器任务所需的最低档位（executors.required_role）。
//
// 认不出的取值按 admin 处理：这一处判的是"要不要把执行能力开给这个身份"，
// 判不准时关门比开门便宜，而 core.Config.Validate 本来也不接受别的取值。
func (s *Server) executorRole() core.Role {
	if s.executors == nil {
		return core.RoleAdmin
	}
	if role, ok := core.ParseRole(s.executors.RequiredRole()); ok {
		return role
	}
	return core.RoleAdmin
}

// executorProfile 按任务名取档位；没注入登记表或这个名字不是档位时返回 false。
//
// 判据是登记表的键而不是 exec. 前缀：前缀只是档位的命名规则，表里有没有这个名字
// 才决定"要不要按执行器任务对待"。表里没有的 exec. 名字走既有的"类型未注册"分支。
func (s *Server) executorProfile(name string) (*executor.Profile, bool) {
	if s.executors == nil {
		return nil, false
	}
	return s.executors.Lookup(name)
}

// gateExecutorSubmission 把执行器任务的提交期检查集中在这一个函数里：
// 身份档位、这台机器能不能跑、payload 合不合法、生效超时是多少。
//
// 返回 nil 表示放行，此时生效超时已经写进 job.Timeout；不是执行器任务时同样返回 nil
// 且不改 job.Timeout（普通任务不受本卡影响）。
// **必须在 scheduler.Schedule 之前调用**：一旦入队，非法 payload 也会真的被执行一次。
// requestedTimeout 是请求体顶层的 timeout，档位任务对它的上限与 payload 里那个 timeout 同一条规则。
func (s *Server) gateExecutorSubmission(c *gin.Context, job *core.Job,
	requestedTimeout time.Duration) *ErrorResponse {

	profile, ok := s.executorProfile(job.Name)
	if !ok {
		return nil
	}
	name := job.Name

	if !s.sec.authEnabled() {
		// 没有凭据的部署里 allowRole 恒为真，所以这条判定等于没做事——
		// 但它是"执行器默认关闭"那条告警的运行时补充：每次启动后第一次提交留一行 warn。
		s.warnAuthDisabledOnce()
	}

	role := s.executorRole()
	if !s.allowRole(c, role) {
		principal, _ := PrincipalFrom(c)
		s.logAccessRejection(principal, "executor job submission", role)
		return &ErrorResponse{
			Code:    http.StatusForbidden,
			Message: "insufficient role",
			Details: fmt.Sprintf("job type %q is an executor profile; submitting it requires role %s (executors.required_role)",
				name, role.String()),
		}
	}

	if reason, available := s.executors.Available(name); !available {
		// 与"类型未注册"分开写：那条说这个名字不存在，这条说名字对但这台机器现在跑不了
		// （脚本没部署、程序不在 PATH 里）。运维需要的是后一种的改正方向。
		return &ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "executor profile is not available on this server",
			Details: reason,
		}
	}

	if requestedTimeout > profile.Timeout {
		return &ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid timeout",
			Details: fmt.Sprintf("timeout %v exceeds the %v allowed by profile %q",
				requestedTimeout, profile.Timeout, profile.Name),
		}
	}

	sub, err := executor.ValidateSubmission(profile, job.Payload)
	if err != nil {
		return &ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid executor payload",
			Details: err.Error(),
		}
	}

	// 生效超时在这里算一次并写进任务：执行侧用的是同一个合成函数（Registry.EffectiveTimeout），
	// 所以任务详情显示的就是实际会断的那一个，不会出现"显示 5m、30s 就超时"。
	job.Timeout = s.executors.EffectiveTimeout(profile, sub.TimeoutValue)
	return nil
}

// warnAuthDisabledOnce 在未启用鉴权的部署里，为第一次执行器提交记一条 warn。
//
// 只记一次：这类部署可能就在本机跑批，每条任务都打一行会让其他日志看不到。
func (s *Server) warnAuthDisabledOnce() {
	s.executorAuthWarn.Do(func() {
		s.logger.Warn("executor job submitted while authentication is disabled",
			"hint", "configure server.auth.token or server.auth.users before enabling executors")
	})
}
