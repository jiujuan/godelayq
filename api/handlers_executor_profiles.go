package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/core"
	"godelayq/executor"
)

// 档位的在线管理（TASK-W06）。设计依据 docs/design/web-profile-design.md §6.6、§6.7。
//
// 这三个端点合起来等于"往执行面注入一条可执行的命令"，所以门槛比删组更高（ops 档起步，D10），
// 每一步的顺序也都是规则而不是风格：校验与探测用的是启动那一份规则（I1），
// 文件先写成功才动注册表与调度器（I2）。

// 删除档位的两种作业处置策略（决策 D6）。默认 pause：把该类型待执行的任务钉住，
// 但一条正在跑的执行都不中止——中止属于 admin 档的 force-pause。
const (
	profileDeleteStrategyPause = "pause"
	profileDeleteStrategyBlock = "block"
)

// executorProfileStore 是端点用到的档位文件读写口，由 core.JSONFileExecutorProfileStore 实现。
//
// 声明在消费方（与分组、事件读取方同一手法）：api 只需要这四个方法，
// 存储换成别的实现不该牵动这里。
type executorProfileStore interface {
	List() ([]core.ExecutorProfileRecord, error)
	Get(name string) (core.ExecutorProfileRecord, bool, error)
	Save(record core.ExecutorProfileRecord) error
	Delete(name string) error
}

// executorProfileApplier 是"把档位文件同步进登记表与调度器"那一步，由 *executor.Applier 实现。
//
// 拆成两个方法而不是一个 Save 的副作用：Validate 给的是"这条写法对不对、这台机器跑不跑得动"，
// Apply 给的是"整份文件重新生效"，端点需要在这两步之间插冲突判定与落盘。
type executorProfileApplier interface {
	Validate(cmd core.ExecutorCommand) (*executor.Profile, executor.ProbeResult, error)
	Apply() (executor.ApplyResult, error)
}

// ExecutorProfileRequest 是 POST/PUT 的请求体。
//
// 字段直接就是存储记录的那一份形状，不另抄一遍 24 个字段：页面上能写的就是文件里能存的，
// 抄一份只会多出"两边字段不一样"这种漂移。created_at/updated_at 即使传进来也会被存储覆盖
// （时间戳是服务端事实，不接受客户端填写）。
type ExecutorProfileRequest struct {
	core.ExecutorProfileRecord
}

// ExecutorProfileDeleteResponse DELETE 的响应：删除本身之外还要说清影响了多少条任务。
type ExecutorProfileDeleteResponse struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// PausedJobs 是本次新钉住的**待执行**任务数（TASK-W04 的 PauseByHandlerKey 返回值口径）。
	PausedJobs int `json:"paused_jobs"`
	// RunningJobs 是该类型此刻仍在执行、因此一条都没被动的任务数（D6：不强杀）。
	RunningJobs int `json:"running_jobs"`
	// AlreadyPausedJobs 是删除之前就已经在 paused 上的条数，它们不在上面的计数里。
	AlreadyPausedJobs int `json:"already_paused_jobs"`
}

// ExecutorProfileRecordResponse 是档位文件里那条记录本身（TASK-W08 的编辑表单取数口）。
//
// 为什么要有它：`GET /executors` 的每一行说的是"这条档位现在的处境"（来源、能不能改、
// 这台机器跑不跑得动），档位的**定义**字段一个都不在里面——于是页面做不出"编辑一条已有档位"，
// 只能新建（TASK-W07 §10.5 登记的 D-0702）。这里把定义给回来，边界与写端点同档：
// web_enabled 打开 + ops 身份（记录里有脚本路径、固定参数、请求头这些配置内容）。
//
// env 的固定取值仍然不外露，只给键名（EnvKeys）：表单拿不到值，保存时自然就不会带 env 这个键，
// 而 PUT 把"没带 env"解释成"不改"（见 UpdateExecutorProfile），两段合起来才守得住
// "页面上的编辑不会悄悄抹掉配置里的凭据"。
type ExecutorProfileRecordResponse struct {
	core.ExecutorProfileRecord

	// EnvKeys 是这条档位固定的环境变量名，按字典序。值一律不外露。
	EnvKeys []string `json:"env_keys"`
}

// GetExecutorProfile GET /api/v1/executors/profiles/:name
//
// 只有档位文件里的那条记录有这个端点：配置侧档位没有存储记录，
// 它在这个端点上是 409（与 PUT/DELETE 同一条判据与文案，界面上只需要认一种情况）。
// 读请求不进写操作台账（api/audit.go 的中间件只记 POST/PUT/DELETE），这里不需要动作词。
func (s *Server) GetExecutorProfile(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if err := core.ValidateProfileName(name); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid profile name", Details: err.Error(),
		})
		return
	}

	record, found, err := s.profileStore.Get(name)
	if err != nil {
		s.respondProfileStoreError(c, err, name)
		return
	}
	if !found {
		if _, ok := s.executors.Lookup(profileKey(name)); ok {
			s.respondKeyTaken(c, profileKey(name))
			return
		}
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code:    http.StatusNotFound,
			Message: "profile not found",
			Details: fmt.Sprintf("no stored profile %q", name),
		})
		return
	}

	keys := make([]string, 0, len(record.Env))
	for key := range record.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// 值不外露，键名走 EnvKeys：置空之后这个键在 JSON 里整个消失（omitempty）
	record.Env = nil

	c.JSON(http.StatusOK, ExecutorProfileRecordResponse{
		ExecutorProfileRecord: record,
		EnvKeys:               keys,
	})
}

// jobTallyByHandlerKey 数出某个注册键下的任务分布。
// 返回 false 表示读任务存储失败且响应已写好，调用方直接返回。
func (s *Server) jobTallyByHandlerKey(c *gin.Context, handlerKey string) (pending, running, paused int, ok bool) {
	snapshots, err := s.store.LoadAll()
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to load jobs",
			Details: err.Error(),
		})
		return 0, 0, 0, false
	}
	for _, snap := range snapshots {
		if snap.HandlerKey() != handlerKey {
			continue
		}
		switch core.JobStatus(snap.Status) {
		case core.StatusPending:
			pending++
		case core.StatusRunning:
			running++
		case core.StatusPaused:
			paused++
		}
	}
	return pending, running, paused, true
}

// profileKey 按档位名给出注册键。名字取存储里的那份写法，键位因此与启动路径一致。
func profileKey(name string) string { return executor.HandlerKeyPrefix + name }

// CreateExecutorProfile POST /api/v1/executors/profiles
//
// 顺序即 DoD：解码 → 校验 → 探测 → 冲突 → 落盘 → 生效 → 响应。
// 探测结论是"不可用"时也照样保存（设计文档 §5.3）：一条指向还没部署的脚本的档位
// 是运维想要留着的东西，把它拒在门外只会让人改用配置文件，绕过这套审计。
func (s *Server) CreateExecutorProfile(c *gin.Context) {
	var req ExecutorProfileRequest
	if !s.bindProfileRequest(c, &req) {
		return
	}
	record := req.ExecutorProfileRecord
	record.CreatedAt = time.Time{}
	record.UpdatedAt = time.Time{}

	name := strings.TrimSpace(record.Name)
	if err := core.ValidateProfileName(name); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid profile name", Details: err.Error(),
		})
		return
	}
	record.Name = name

	cmd, ok := s.profileCommand(c, record)
	if !ok {
		return
	}
	_, found, err := s.profileStore.Get(name)
	if err != nil {
		s.respondProfileStoreError(c, err, name)
		return
	}
	if found {
		c.JSON(http.StatusConflict, ErrorResponse{
			Code:    409,
			Message: "profile already exists",
			Details: fmt.Sprintf("profile %q is already in the profile store; use PUT to change it", name),
		})
		return
	}
	key := profileKey(name)
	if _, found := s.executors.Lookup(key); found {
		s.respondKeyTaken(c, key)
		return
	}

	if _, _, err := s.profiles.Validate(cmd); err != nil {
		// 原文回给调用方：这条错误的全部内容是字段组合与路径写法，不含取值
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid executor profile", Details: err.Error(),
		})
		return
	}

	if err := s.profileStore.Save(record); err != nil {
		s.respondProfileStoreError(c, err, name)
		return
	}

	result, err := s.profiles.Apply()
	if err != nil {
		// 已经落盘却没生效：把刚写的那条删回去，回到请求之前的状态
		if rollbackErr := s.profileStore.Delete(name); rollbackErr != nil {
			s.logger.Error("profile stored but not applied and rollback failed",
				"profile", name, "error", err, "rollback_error", rollbackErr)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Code:    http.StatusInternalServerError,
				Message: "profile saved but not registered, and the file could not be restored",
				Details: fmt.Sprintf("%v; the profile file and the running process disagree, restart realigns them", err),
			})
			return
		}
		s.logger.Error("profile stored but not applied, rolled back", "profile", name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "profile saved but not registered",
			Details: fmt.Sprintf("%v; the stored profile was rolled back", err),
		})
		return
	}
	s.logProfileWarnings(result.Warnings)

	s.respondProfile(c, http.StatusCreated, key)
}

// UpdateExecutorProfile PUT /api/v1/executors/profiles/:name
//
// 三条前置判定按"存储里有没有"→"是不是配置档位"→"有没有改禁改字段"的顺序走：
// 只有第一条件成立才谈得上更新，第二条件解释为什么这条改不了，第三条件给出出路（删了重建）。
func (s *Server) UpdateExecutorProfile(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if err := core.ValidateProfileName(name); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid profile name", Details: err.Error(),
		})
		return
	}

	var req ExecutorProfileRequest
	if !s.bindProfileRequest(c, &req) {
		return
	}
	record := req.ExecutorProfileRecord
	record.CreatedAt = time.Time{}
	record.UpdatedAt = time.Time{}

	// 档位名是主键，也是注册键的一部分，所以改名等于换一条档位：本端点不做改名。
	// 大小写按存储的主键口径忽略（它一直按小写存），其余不一致一律拒。
	if bodyName := strings.TrimSpace(record.Name); bodyName != "" &&
		!strings.EqualFold(bodyName, name) {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    400,
			Message: "profile name mismatch",
			Details: fmt.Sprintf("path says %q but the body says %q; renaming a profile means deleting and recreating it",
				name, bodyName),
		})
		return
	}
	record.Name = name

	existing, found, err := s.profileStore.Get(name)
	if err != nil {
		s.respondProfileStoreError(c, err, name)
		return
	}
	if !found {
		if _, ok := s.executors.Lookup(profileKey(name)); ok {
			s.respondKeyTaken(c, profileKey(name))
			return
		}
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code:    http.StatusNotFound,
			Message: "profile not found",
			Details: fmt.Sprintf("no stored profile %q; profiles declared in executors.commands are read only here", name),
		})
		return
	}

	// D7：kind / script / program 三条决定"这条档位是什么"，改它们请删了重建。
	// 判据放在这里而不是交给校验函数报错：那种写法完全合法，
	// 只是不能对一条已存在的档位做——换内核不是改参数。
	if failure := immutableFieldChange(existing, record); failure != "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    400,
			Message: "field cannot be changed",
			Details: fmt.Sprintf("%s cannot be changed on an existing profile (%q); delete it and create a new one",
				failure, name),
		})
		return
	}

	// env 的固定取值不回显（见 ExecutorProfileRecordResponse），所以页面保存时通常不带这个键。
	// 不带就解释成"不改"：存着的取值原样留着；显式写 "env": {} 才是清空。
	// 没有这条规则的话，一次只想改超时的编辑会把档位里的固定环境变量抹掉——
	// 而那件事在界面上看不见、在响应里也不可见，是最难被发现的一类数据丢失。
	if record.Env == nil {
		record.Env = existing.Env
	}

	cmd, ok := s.profileCommand(c, record)
	if !ok {
		return
	}
	if _, _, err := s.profiles.Validate(cmd); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid executor profile", Details: err.Error(),
		})
		return
	}

	record.CreatedAt = existing.CreatedAt
	if err := s.profileStore.Save(record); err != nil {
		s.respondProfileStoreError(c, err, name)
		return
	}

	result, err := s.profiles.Apply()
	if err != nil {
		if rollbackErr := s.profileStore.Save(existing); rollbackErr != nil {
			s.logger.Error("profile updated but not applied and rollback failed",
				"profile", name, "error", err, "rollback_error", rollbackErr)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Code:    http.StatusInternalServerError,
				Message: "profile saved but not registered, and the file could not be restored",
				Details: fmt.Sprintf("%v; the profile file and the running process disagree, restart realigns them", err),
			})
			return
		}
		s.logger.Error("profile updated but not applied, rolled back", "profile", name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "profile saved but not registered",
			Details: fmt.Sprintf("%v; the previous profile was restored in the file", err),
		})
		return
	}
	s.logProfileWarnings(result.Warnings)

	s.respondProfile(c, http.StatusOK, profileKey(name))
}

// DeleteExecutorProfile DELETE /api/v1/executors/profiles/:name?jobs=pause|block
//
// 三步的次序：先钉住该类型的待执行任务，再删记录，最后让整张表重新生效（这一步才摘 handler）。
// 中间任何一步失败都不会留下"档位在、跑不了"或"档位没了、还能提交"这两种半状态：
// 钉任务失败就直接报错、文件不动；文件删不掉则任务只是被钉住，重试删除即可（幂等）。
// 反过来（先摘 handler 再删文件失败）会留下"表里有这条、进程里跑不了"，那才是 I2 要防的方向。
func (s *Server) DeleteExecutorProfile(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if err := core.ValidateProfileName(name); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid profile name", Details: err.Error(),
		})
		return
	}

	strategy := c.Query("jobs")
	if strategy == "" {
		strategy = profileDeleteStrategyPause
	}
	if strategy != profileDeleteStrategyPause && strategy != profileDeleteStrategyBlock {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    400,
			Message: "unsupported jobs strategy",
			Details: fmt.Sprintf("got %q, expected pause or block", strategy),
		})
		return
	}

	existing, found, err := s.profileStore.Get(name)
	if err != nil {
		s.respondProfileStoreError(c, err, name)
		return
	}
	if !found {
		key := profileKey(name)
		if _, ok := s.executors.Lookup(key); ok {
			s.respondKeyTaken(c, key)
			return
		}
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code: http.StatusNotFound, Message: "profile not found",
			Details: fmt.Sprintf("no stored profile %q", name),
		})
		return
	}

	key := profileKey(existing.Name)
	pending, running, alreadyPaused, ok := s.jobTallyByHandlerKey(c, key)
	if !ok {
		return
	}

	if strategy == profileDeleteStrategyBlock && pending+running+alreadyPaused > 0 {
		c.JSON(http.StatusConflict, ErrorResponse{
			Code:    http.StatusConflict,
			Message: "profile still has jobs",
			Details: fmt.Sprintf("%d pending, %d running and %d paused job(s) still use %q; "+
				"let them finish, pause them, or drop the jobs param", pending, running, alreadyPaused, existing.Name),
		})
		return
	}

	paused := 0
	if pending > 0 {
		// 只钉待执行的那批：正在执行的由 D6 明确不动，已经暂停的不重复计
		count, err := s.scheduler.PauseByHandlerKey(key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Code:    http.StatusInternalServerError,
				Message: "failed to pause jobs of this profile",
				Details: err.Error(),
			})
			return
		}
		paused = count
	}

	if err := s.profileStore.Delete(existing.Name); err != nil {
		s.respondProfileStoreError(c, err, existing.Name)
		return
	}

	result, err := s.profiles.Apply()
	if err != nil {
		// 删除已落盘却没生效：把记录写回去，让文件与调度器重新对上
		if rollbackErr := s.profileStore.Save(existing); rollbackErr != nil {
			s.logger.Error("profile deleted but not applied and rollback failed",
				"profile", existing.Name, "error", err, "rollback_error", rollbackErr)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Code:    http.StatusInternalServerError,
				Message: "profile deleted but still registered, and the file could not be restored",
				Details: fmt.Sprintf("%v; the profile file and the running process disagree, restart realigns them", err),
			})
			return
		}
		s.logger.Error("profile deleted but not applied, restored the record",
			"profile", existing.Name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "profile deleted but still registered",
			Details: fmt.Sprintf("%v; the profile record was restored in the file", err),
		})
		return
	}
	s.logProfileWarnings(result.Warnings)

	s.logger.Info("executor profile deleted",
		"profile", existing.Name, "handler_key", key,
		"paused_jobs", paused, "running_jobs", running)
	c.JSON(http.StatusOK, ExecutorProfileDeleteResponse{
		Key:               key,
		Name:              existing.Name,
		PausedJobs:        paused,
		RunningJobs:       running,
		AlreadyPausedJobs: alreadyPaused,
	})
}

// bindProfileRequest 解码请求体并拒绝未知键（与配置侧 UnmarshalExact 同一条口径：
// 键名拼错的档位不该"保存成功但那条没生效"）。返回 false 表示响应已经写好。
func (s *Server) bindProfileRequest(c *gin.Context, req *ExecutorProfileRequest) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid request body", Details: err.Error(),
		})
		return false
	}
	return true
}

// profileCommand 把存储记录换成执行器那一层的档位定义。
// 走到这里只有 timeout 写法可能失败（存储只管名字与重名）。
func (s *Server) profileCommand(c *gin.Context, record core.ExecutorProfileRecord) (core.ExecutorCommand, bool) {
	cmd, err := record.Command()
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid executor profile", Details: err.Error(),
		})
		return core.ExecutorCommand{}, false
	}
	return cmd, true
}

// immutableFieldChange 返回第一条被改动的禁改字段名，没有改动返回空串。
// 判据比的是**写法原文**：kind 只有三个取值，script/program 的相对与绝对两种写法
// 虽然可能指向同一个文件，但那是"换了一种写法"，按 D7 一样要删了重建。
func immutableFieldChange(existing, updated core.ExecutorProfileRecord) string {
	switch {
	case strings.TrimSpace(existing.Kind) != strings.TrimSpace(updated.Kind):
		return "kind"
	case strings.TrimSpace(existing.Script) != strings.TrimSpace(updated.Script):
		return "script"
	case strings.TrimSpace(existing.Program) != strings.TrimSpace(updated.Program):
		return "program"
	}
	return ""
}

// respondKeyTaken 说明这个注册键被配置侧档位占着。
//
// 三种走到这里的写法（POST 同名、PUT 改配置档位、DELETE 删配置档位）后果都是"这里改不动它"，
// 所以文案只讲这一件事实：要改就改 yaml 并重启（设计文档 §8：配置侧不开放在线写）。
func (s *Server) respondKeyTaken(c *gin.Context, key string) {
	c.JSON(http.StatusConflict, ErrorResponse{
		Code:    http.StatusConflict,
		Message: "profile name belongs to a configuration profile",
		Details: fmt.Sprintf("handler key %q comes from executors.commands; configuration profiles are read only here, edit the yaml and restart", key),
	})
}

// respondProfileStoreError 把存储的错误映射成状态码。
func (s *Server) respondProfileStoreError(c *gin.Context, err error, name string) {
	switch {
	case errors.Is(err, core.ErrProfileNameInvalid):
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code: 400, Message: "invalid profile name", Details: err.Error(),
		})
	case errors.Is(err, core.ErrProfileNotFound):
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code:    http.StatusNotFound,
			Message: "profile not found",
			Details: fmt.Sprintf("no stored profile %q", name),
		})
	case errors.Is(err, core.ErrProfileDuplicate):
		c.JSON(http.StatusConflict, ErrorResponse{
			Code: 409, Message: "profile already exists", Details: err.Error(),
		})
	default:
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to write the profile store",
			Details: err.Error(),
		})
	}
}

// respondProfile 从登记表里读出这条档位现在的样子并回出去。
//
// 探测结论取登记表而不是请求体：生效之后那张表才是"这台机器现在能不能跑"的权威。
// 来源/可编辑/路径这几项也从同一张表取，而且只读一次（TASK-W07 的 Registry.List）：
// 写响应与列表端点说的必须是同一份处境，分几个访问器读就可能读到换表前后的两份内容。
// 降级那一条跳过：刚存进去的档位要么生效要么整个请求已经失败，
// 同一个键出现两行只在"与 executors.commands 撞名"时才有，而那种写入在落盘前就被 409 挡住了。
// 查不到属于不该发生的组合（刚 Apply 完就没了），按 500 报出来而不是回一个空对象。
func (s *Server) respondProfile(c *gin.Context, status int, key string) {
	for _, item := range s.executors.List() {
		if item.Profile.HandlerKey() != key || item.Degraded {
			continue
		}
		c.JSON(status, toExecutorProfile(item))
		return
	}
	c.JSON(http.StatusInternalServerError, ErrorResponse{
		Code:    http.StatusInternalServerError,
		Message: "profile applied but missing from the registry",
		Details: fmt.Sprintf("handler key %q is not in the registry after applying the profile store", key),
	})
}

// logProfileWarnings 把"文件里有、没能进表"的记录逐条记进日志。
// Applier 自己也记一条：这里的用途是让失败的那次请求在服务日志里留下请求侧的痕迹。
func (s *Server) logProfileWarnings(warnings []executor.ProfileWarning) {
	for _, warning := range warnings {
		s.logger.Warn("stored executor profile is not in effect",
			"profile", warning.Name, "reason", warning.Reason)
	}
}

// requireExecutorProfiles 在两种情况下挡住档位写端点，且文案可区分：
//
//  1. 没打开（executors.web_enabled=false）：先判它，没打开的部署不该从状态码里
//     读出"这台装配了什么"。
//  2. 打开了却没装配依赖：那是装配方的 bug，宁可 503 说清楚，
//     也不要让请求走到处理器里再取空指针。
func (s *Server) requireExecutorProfiles() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.executors == nil || !s.executors.WebEnabled() {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
				Code:    http.StatusServiceUnavailable,
				Message: "executor profile management is not enabled",
				Details: "set executors.web_enabled=true (it requires executors.enabled=true) to manage profiles over HTTP",
			})
			return
		}
		if s.profileStore == nil || s.profiles == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
				Code:    http.StatusServiceUnavailable,
				Message: "executor profile management is not configured",
				Details: "start the server with api.WithExecutorProfileStore and api.WithExecutorProfileApplier to enable /api/v1/executors/profiles",
			})
			return
		}
		c.Next()
	}
}
