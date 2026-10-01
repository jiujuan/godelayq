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
	// RedactionNote 只在档位声明了 secret 参数时出现，提醒读的人：
	// 正文由脚本或对端产生，框架层的参数掩码管不到它把值打印出来。
	// 这条说明不是"已防护"的声明，卡片 §9 明确要求把它当成提醒写。
	RedactionNote string `json:"redaction_note,omitempty"`
}

// GetJobResult GET /api/v1/jobs/:id/result
// 读的是产物文件，不进任何内存缓冲：几十 KB 的输出一旦被事件带走，
// 实时通道和事件历史都会按输出体积失控。
//
// 档位：路由上是 reader（viewer 及以上）；档位声明了 secret 参数时，取到快照之后再升一级
// 到 executors.required_role（TASK-E16 §3.3 第 2 条，判档函数是下面的 resultGuard）。
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

	// E07 预留的判档点在这里：档位声明了 secret 参数时，读取门槛从 reader 升到提交档位，
	// 并给响应带一句说明（TASK-E16 §3.3 第 2 条）。判档在参数解析之前，
	// 越权的连接不该从 400 里读出"这个任务的产物存在且文件名是什么"。
	note, denied := s.resultGuard(c, snapshot.HandlerKey())
	if denied != nil {
		c.JSON(denied.Code, *denied)
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
			JobID:         id,
			Attempt:       attempt,
			Stream:        stream,
			Meta:          s.markArtifactPurged(snapshot, attempt),
			Found:         false,
			Content:       "",
			RedactionNote: note,
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
		Meta:          s.execForResponse(snapshot.Name, snapshot.Payload, snapshot.Exec),
		Content:       string(content),
		RedactionNote: note,
	})
}

// markArtifactPurged 返回要对外给出的摘要，并在产物已缺失时把状态回写成 purged。
//
// 这是结果端点读路径上唯一一处写，后来者请不要再往这个函数里加写操作。
// 理由：带摘要却没有文件的任务会被反复查询（详情页与事件时间线都指向它），
// 每次都重新撞一次"文件不存在"再重新解释一遍不值得，回写一次就把结论存下来。
// 已经是 purged 时不再写快照，但索引那一侧仍要标一次：两者可能来自不同的部署，
// 快照早被标过而索引还没跟上（比如索引是这次重启才挂上的）是可能出现的状态。
//
// attempt 是本次读取的那一次尝试，不是 snapshot.Attempts：默认选取的尝试与调用方
// 显式指定的可能不同，标错那一条会让一个还有文件的尝试在列表里显示成已清理。
func (s *Server) markArtifactPurged(snapshot core.JobSnapshot, attempt int) *core.ExecMeta {
	if snapshot.Exec == nil {
		return nil
	}
	if snapshot.Exec.Artifact == core.ArtifactPurged {
		s.markArtifactIndexed(snapshot.ID, attempt)
		return s.execForResponse(snapshot.Name, snapshot.Payload, snapshot.Exec)
	}

	// 复制一份再改：LoadAll 返回的快照与存储内部共用同一个摘要指针，
	// 直接改它会和落盘协程的读法撞在一起。
	summary := *snapshot.Exec
	summary.Artifact = core.ArtifactPurged
	updated := snapshot
	updated.Exec = &summary

	if err := s.store.Update(updated); err != nil {
		s.logger.Warn("failed to record that the execution output is gone",
			"job_id", snapshot.ID, "attempt", attempt, "error", err)
	}
	// 快照与索引两侧同一时刻变成同一个结论，否则列表说 available、正文说没有
	s.markArtifactIndexed(snapshot.ID, attempt)
	return s.execForResponse(snapshot.Name, snapshot.Payload, &summary)
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

// ExecutorPositionalResponse 是档位的位置参数规则（args 里保留键 _positional 的取值规则）。
// 档位没声明位置参数时整个键不出现，前端不必区分"没有"与"上限 0"。
type ExecutorPositionalResponse struct {
	Max     int    `json:"max"`
	Pattern string `json:"pattern"`
}

// ExecutorProfileResponse 是一个档位的对外形状。
//
// 刻意不含的东西：env 的固定值（那是配置里的凭据）、以及任何解释器的安装路径。
// 路径一项在 TASK-W07 之后改了口径：workspace 之内的档位仍然只给相对写法，
// 而页面建的档位可以指向 workspace 之外（设计文档 D5），那一条的绝对路径会原样出现在
// path_display 里——reader 档也读得到，遮蔽方案登记为 S-3，不在本卡。
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
	MaxParallel int                         `json:"max_parallel"`
	Args        []ExecutorArgResponse       `json:"args"`
	Positional  *ExecutorPositionalResponse `json:"positional,omitempty"`
	EnvAllow    []string                    `json:"env_allow"`
	// HasSecretArgs 表示这个档位声明了至少一个 secret 参数：
	// 它的 payload 在读取接口里会被掩码，结果端点的读取门槛也升到提交档位（TASK-E16 §3.4）。
	// 前端据此显示"这个档位的参数不会回显"，不必自己复制一份 args[].secret 的判断。
	HasSecretArgs bool `json:"has_secret_args"`
	// PreferredResultDirection 是读这个档位输出时的建议起点（head | tail），
	// 取值来自 executor.PreferredResultDirection，与 GET /jobs/:id/result 的 from 参数同一套词。
	// 放在响应里而不是让前端按 kind 判：http 的正文开头才是结论，进程档位的末尾才是结论，
	// 这条规则改一次就要改两处（TASK-E18 §3.2 第 2 条要求二选一，这里选"后端给"）。
	PreferredResultDirection string `json:"preferred_result_direction"`
	// 下面三项只在 http 档位出现，给的是"表单能填什么"，不是"这次填了什么"。
	// Method 是档位声明的请求方法。
	Method string `json:"method,omitempty"`
	// HeaderAllow 是 payload 可以覆盖的请求头名；档位自己的固定头不在这里，
	// 那部分本来就是配置内容，不需要表单参与。
	HeaderAllow []string `json:"header_allow,omitempty"`
	// BodyMode 是档位接受的请求体形态：json | raw | none。none 时表单不该给 body 输入区。
	BodyMode string `json:"body_mode,omitempty"`
	// URL 只在 http 档位出现，给的是模板原文（含 {占位符}），不是渲染后的地址。
	URL string `json:"url,omitempty"`

	// 下面四项是 TASK-W07 加的"这条档位的处境"，全部由 executor.Registry 算好后透传，
	// api 层不再判一遍（同一份规则长两处是这个系列一直在防的事）。
	// Source 是来源：config 来自 executors.commands（这里只读），store 来自档位文件。
	Source string `json:"source"`
	// Editable 是"能不能在页面上改它"：web_enabled 且来源是 store 且它没被降级。
	// 它只决定按钮显不显示，写请求的边界仍是 ops 档判定（隐藏按钮从来不是安全边界）。
	Editable bool `json:"editable"`
	// Degraded 为真表示这条档位与 executors.commands 里的同名档位撞上了：
	// 它看得见但没生效，reason 给的是那句冲突说明（不是"文件不在"那种探测结论）。
	// 同一个 key 因此可能出现在两行里——生效那条与降级那条，靠这个字段区分。
	Degraded bool `json:"degraded"`
	// PathDisplay 是这个档位指向的本机文件写法：workspace 内给相对写法、之外给绝对路径，
	// http 档位与"program 写成 PATH 程序名"的 binary 档位没有路径可给，整个键省略。
	PathDisplay string `json:"path_display,omitempty"`
}

// ListExecutorsResponse GET /api/v1/executors。
type ListExecutorsResponse struct {
	Enabled  bool                      `json:"enabled"`
	Profiles []ExecutorProfileResponse `json:"profiles"`
	// RequiredRole 是提交执行器任务所需的最低档位名（TASK-E16 §3.4 第 1 条）。
	// 执行器关闭时是 null：那时没有任何档位可提交，"要什么档位"这个问题不成立，
	// 给一个配置里的取值反而会让前端以为存在这道门槛。
	RequiredRole *string `json:"required_role"`
	// MaxTimeout 是 payload 的 timeout 能填的上限（executors.max_timeout，已归一化为取值），
	// 只在 enabled=true 时给出。提交表单用它给"超时"输入框划区间：超过它的值在提交期就被拒
	// （TASK-E16 §3.2 第 2 条），不该让人填出注定失败的取值；而"能不能提交执行器任务"这个问题
	// 只在执行器开着的时候成立，所以关闭时不给这个键，前端也不会去显示一个没人用的区间。
	MaxTimeout string `json:"max_timeout,omitempty"`
	// WebEnabled 是 executors.web_enabled（档位的在线管理开没开）。
	// 关闭时这个键照样给出并回 false（TASK-W07 §3.2）：它是前端"能不能改档位"的唯一判据，
	// 省略就等于让前端去猜"没这个键"是"关着"还是"这份后端还不认识档位管理"。
	WebEnabled bool `json:"web_enabled"`
	// RuntimeAllow 是 executors.runtime_allow 实际生效的那份名单，给 W08 的"解释器"下拉用。
	// 它与上面 required_role / max_timeout 的"关闭时不给"故意不一致：那两项说的是
	// "现在能不能提交执行任务"，这一项是一份配置事实。enabled=false 时它照样给出。
	// 没装配登记表的部署给 []。
	RuntimeAllow []string `json:"runtime_allow"`
}

// ListExecutors GET /api/v1/executors
//
// 没有 503 守卫：执行器默认关闭，"没装配登记表"是默认状态而不是错误状态
// （口径见 docs/design/executor-design.md §6.1）。
//
// profiles 里同时有生效与降级两批（TASK-W07 §3.1）：撞名的那条档位看得见但跑不了，
// 静默丢掉只会让"页面上明明建过、重启后不见了"变成无解之谜。
func (s *Server) ListExecutors(c *gin.Context) {
	response := ListExecutorsResponse{
		Enabled:      false,
		Profiles:     []ExecutorProfileResponse{},
		RuntimeAllow: []string{},
	}
	if s.executors == nil {
		c.JSON(http.StatusOK, response)
		return
	}

	response.Enabled = s.executors.Enabled()
	response.WebEnabled = s.executors.WebEnabled()
	response.RuntimeAllow = s.executors.RuntimeAllow()
	if response.Enabled {
		role := s.executors.RequiredRole()
		response.RequiredRole = &role
		response.MaxTimeout = s.executors.MaxTimeout().String()
	}
	for _, item := range s.executors.List() {
		response.Profiles = append(response.Profiles, toExecutorProfile(item))
	}
	c.JSON(http.StatusOK, response)
}

// toExecutorProfile 把登记表的一行换成对外形状。
//
// 可用性结论在这里分两种来源（同一条 "runtime_ok + reason" 的形状）：
// 降级那条给的是"与 executors.commands 撞名、没注册"那句冲突说明（RuntimeOK 恒 false，
// 它确实跑不了），其余条目给它自己的探测结论。两条各自成立的事实里，
// "为什么这条没生效"是读者更要紧的那一件。
func toExecutorProfile(view executor.ListedProfile) ExecutorProfileResponse {
	profile := view.Profile
	reason, runtimeOK := view.Probe.Reason, view.Probe.Available
	if view.Degraded {
		reason, runtimeOK = view.Reason, false
	}

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
		Key:           profile.HandlerKey(),
		Name:          profile.Name,
		Kind:          string(profile.Kind),
		RuntimeOK:     runtimeOK,
		Reason:        reason,
		Timeout:       profile.Timeout.String(),
		MaxParallel:   profile.MaxParallel,
		Args:          args,
		EnvAllow:      envAllow,
		HasSecretArgs: profile.HasSecretArgs(),
		// 起点建议由执行器包给出（http 读开头、进程读末尾），前端只照它设置初始标签页，
		// 于是"两处各判一次导致默认读取方向不一致"这种漂移没有机会出现（TASK-E18 §3.2 第 2 条）。
		PreferredResultDirection: executor.PreferredResultDirection(profile),
		// 四项"这条档位的处境"全部照抄登记表：来源、能不能改、有没有被降级、指向的文件写法。
		// api 层不重算 editable 的规则，那份判断只在 executor.Registry.List 里有一份。
		Source:      string(view.Source),
		Editable:    view.Editable,
		Degraded:    view.Degraded,
		PathDisplay: profile.PathDisplay(),
	}
	if profile.Positional != nil {
		item.Positional = &ExecutorPositionalResponse{
			Max:     profile.Positional.Max,
			Pattern: profile.Positional.PatternText,
		}
	}
	if profile.Kind == executor.KindHTTP {
		item.URL = profile.URLTemplate
		item.Method = profile.Method
		// BodyMode 给的是 json | raw | none 三个词之一：配置里的空写法在这里归一为 none，
		// 因为执行侧对"没写 body"与"写了 none"的判断就是同一条（args.go 的 checkHTTPBody），
		// 让前端再各判一次空串等于把这条规则抄两遍。
		item.BodyMode = profile.Body
		if item.BodyMode == "" {
			item.BodyMode = "none"
		}
		// 与 env_allow 同一口径：空表给 []，前端不必区分"没声明"与"声明了但不允许任何头"。
		item.HeaderAllow = make([]string, 0, len(profile.HeaderAllow))
		item.HeaderAllow = append(item.HeaderAllow, profile.HeaderAllow...)
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

// gateExecutorSubmissionRole 只判"这个身份能不能碰这个档位的任务"（TASK-E16 §3.1）。
//
// 单独拆出来是因为 PUT 需要它出现在状态检查之前：档位不够的连接不该从 409 里读出
// "这条执行器任务跑完了没有"（与 GetJobResult 的 resultGuard 同一条口径）。
func (s *Server) gateExecutorSubmissionRole(c *gin.Context, profile *executor.Profile) *ErrorResponse {
	if !s.sec.authEnabled() {
		// 没有凭据的部署里 allowRole 恒为真，所以这条判定等于没做事——
		// 但它是"执行器默认关闭"那条告警的运行时补充：每次启动后第一次提交留一行 warn。
		s.warnAuthDisabledOnce()
	}

	role := s.executorRole()
	if !s.allowRole(c, role) {
		principal, _ := PrincipalFrom(c)
		s.logAccessRejection(principal, "executor job submission", role)
		// 台账的结论与上面那行日志同处、同源：不放进 logAccessRejection 是因为它同时被
		// 路由档位与结果端点共用，而那些拒绝不该带执行器列（本卡 §3.5 的取舍见 §10）。
		stashAuditExecutor(c, auditExecRoleDenied, role.String(), profile)
		return &ErrorResponse{
			Code:    http.StatusForbidden,
			Message: "insufficient role",
			Details: fmt.Sprintf("job type %q is an executor profile; submitting it requires role %s (executors.required_role)",
				profile.HandlerKey(), role.String()),
		}
	}
	return nil
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

	if failure := s.gateExecutorSubmissionRole(c, profile); failure != nil {
		return failure
	}

	if reason, available := s.executors.Available(name); !available {
		// 与"类型未注册"分开写：那条说这个名字不存在，这条说名字对但这台机器现在跑不了
		// （脚本没部署、程序不在 PATH 里）。运维需要的是后一种的改正方向。
		stashAuditExecutor(c, auditExecProfileUnavailable, auditReasonProfile, profile)
		return &ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "executor profile is not available on this server",
			Details: reason,
		}
	}

	if requestedTimeout > profile.Timeout {
		stashAuditExecutor(c, auditExecTimeoutRejected, auditReasonTimeout, profile)
		return &ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid timeout",
			Details: fmt.Sprintf("timeout %v exceeds the %v allowed by profile %q",
				requestedTimeout, profile.Timeout, profile.Name),
		}
	}

	sub, err := executor.ValidateSubmission(profile, job.Payload)
	if err != nil {
		// 错误原文只进响应与日志，不进台账：里面可能出现参数取值（设计文档 D7）
		stashAuditExecutor(c, auditExecPayloadRejected, auditReasonPayload, profile)
		return &ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid executor payload",
			Details: err.Error(),
		}
	}

	// 生效超时在这里算一次并写进任务：执行侧用的是同一个合成函数（Registry.EffectiveTimeout），
	// 所以任务详情显示的就是实际会断的那一个，不会出现"显示 5m、30s 就超时"。
	job.Timeout = s.executors.EffectiveTimeout(profile, sub.TimeoutValue)
	stashAuditExecutor(c, auditExecAccepted, auditReasonAccepted, profile)
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

// payloadForResponse 按档位声明决定要不要掩码 payload 里的凭据。
//
// 掩码只发生在这里（响应），不改存储也不改执行输入——卡片 §3.3 第 3 条要求把这句话
// 写在代码里，避免后来者把响应上的 *** 当成"参数已经加密保存"。
func (s *Server) payloadForResponse(name string, payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	profile, ok := s.executorProfile(name)
	if !ok || !profile.HasSecretArgs() {
		return payload
	}
	return profile.MaskPayload(payload)
}

// resultGuard 判断这次结果读取的档位够不够，并给出响应里的说明文本。
//
// 卡片 §3.3 第 2 条的收严只针对含 secret 参数的档位：输出正文是脚本或对端产生的，
// 框架层掩不住它回显的凭据，所以读取门槛跟着升到提交档位，并在响应里留一句说明。
// 返回的 denied 非 nil 时调用方直接回 403。
func (s *Server) resultGuard(c *gin.Context, name string) (note string, denied *ErrorResponse) {
	profile, ok := s.executorProfile(name)
	if !ok || !profile.HasSecretArgs() {
		return "", nil
	}

	role := s.executorRole()
	if !s.allowRole(c, role) {
		principal, _ := PrincipalFrom(c)
		s.logAccessRejection(principal, "execution output of a profile with secret arguments", role)
		return "", &ErrorResponse{
			Code:    http.StatusForbidden,
			Message: "insufficient role",
			Details: fmt.Sprintf("profile %q declares secret arguments; reading its execution output requires role %s",
				profile.Name, role.String()),
		}
	}

	return redactionNote, nil
}

// redactionNote 是给含 secret 参数档位的读取者的一句提醒。
const redactionNote = "output is produced by the script or the remote endpoint and may contain the values of secret arguments"
