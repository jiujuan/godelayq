package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// 删除分组的两种策略（决策 D5，见 docs/design/web-console-design.md §5.5）。
// 默认 detach：任何角色都不会通过一个 API 参数删掉别人的任务。
const (
	groupDeleteStrategyDetach = "detach"
	groupDeleteStrategyBlock  = "block"
)

// GroupResponse 分组元数据加上"现在真的挂着多少任务"的计数。
// 计数来自任务快照而不是注册表：组名可以不带注册就写在任务上（§5.3），
// 只有按实际任务数才能回答"删了这个组会影响多少条"。
type GroupResponse struct {
	core.Group

	JobCount    int `json:"job_count" example:"12"`
	PausedCount int `json:"paused_count" example:"1"`
	// Registered 为 false 表示这个组名只出现在任务标签上，注册表里没有对应条目。
	// UI 需要看得见它，否则改组名半途失败留下的"半个旧组"无处下手。
	Registered bool `json:"registered" example:"true"`
}

// CreateGroupRequest POST /api/v1/groups
type CreateGroupRequest struct {
	Name        string `json:"name" binding:"required" example:"nightly"`
	Description string `json:"description,omitempty" example:"夜间批处理"`
	Color       string `json:"color,omitempty" example:"#2563eb"`
}

// UpdateGroupRequest PUT /api/v1/groups/:name
// 指针字段区分"没传"与"传了空串"：描述与颜色要能被清空。
type UpdateGroupRequest struct {
	Name        *string `json:"name,omitempty" example:"nightly-batch"`
	Description *string `json:"description,omitempty"`
	Color       *string `json:"color,omitempty"`
}

// groupTally 是一个组名（小写键）当前的挂载情况。
type groupTally struct {
	named  string // 任务里实际写到的形态，保留用户书写的大小写
	jobs   int
	paused int
}

// ListGroups GET /api/v1/groups
func (s *Server) ListGroups(c *gin.Context) {
	tallies, ok := s.tallyGroups(c)
	if !ok {
		return
	}

	registered, err := s.groups.List()
	if err != nil {
		c.JSON(500, ErrorResponse{Code: 500, Message: "failed to load groups", Details: err.Error()})
		return
	}

	items := make([]GroupResponse, 0, len(registered)+len(tallies))
	seen := make(map[string]bool, len(registered))
	for _, group := range registered {
		key := strings.ToLower(group.Name)
		seen[key] = true
		items = append(items, s.toGroupResponse(group, tallies[key], true))
	}

	// 只存在于任务标签上的组：按名字典序补在后面，注册表条目优先
	orphans := make([]string, 0, len(tallies))
	for key, tally := range tallies {
		if !seen[key] && tally.jobs > 0 {
			orphans = append(orphans, key)
		}
	}
	sort.Strings(orphans)
	for _, key := range orphans {
		tally := tallies[key]
		items = append(items, s.toGroupResponse(core.Group{Name: tally.named}, tally, false))
	}

	// 注册条目与"只挂在任务标签上的组"合成一份按名称排序的列表：
	// 前端只有一个分组下拉，分成两段反而看不出谁是新的。
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	c.JSON(http.StatusOK, items)
}

func (s *Server) toGroupResponse(group core.Group, tally groupTally, registered bool) GroupResponse {
	return GroupResponse{
		Group:       group,
		JobCount:    tally.jobs,
		PausedCount: tally.paused,
		Registered:  registered,
	}
}

// tallyGroups 扫一遍任务快照统计各分组的挂载数。
// 返回 false 表示读存储失败且响应已经写好，调用方直接返回。
func (s *Server) tallyGroups(c *gin.Context) (map[string]groupTally, bool) {
	snapshots, err := s.store.LoadAll()
	if err != nil {
		c.JSON(500, ErrorResponse{Code: 500, Message: "failed to load jobs", Details: err.Error()})
		return nil, false
	}

	tallies := make(map[string]groupTally)
	for _, snap := range snapshots {
		if snap.Group == "" {
			continue
		}
		key := strings.ToLower(snap.Group)
		tally := tallies[key]
		if tally.named == "" {
			tally.named = snap.Group
		}
		tally.jobs++
		if core.JobStatus(snap.Status) == core.StatusPaused {
			tally.paused++
		}
		tallies[key] = tally
	}
	return tallies, true
}

// CreateGroup POST /api/v1/groups
func (s *Server) CreateGroup(c *gin.Context) {
	var req CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid request body", Details: err.Error()})
		return
	}
	if err := core.ValidateGroupName(req.Name); err != nil {
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid group name", Details: err.Error()})
		return
	}

	existing, found, err := s.groups.Get(req.Name)
	if err != nil {
		c.JSON(500, ErrorResponse{Code: 500, Message: "failed to read groups", Details: err.Error()})
		return
	}
	if found {
		c.JSON(409, ErrorResponse{
			Code:    409,
			Message: "group already exists",
			Details: fmt.Sprintf("group %q already registered (as %q)", req.Name, existing.Name),
		})
		return
	}

	group := core.Group{Name: req.Name, Description: req.Description, Color: req.Color}
	if err := s.groups.Save(group); err != nil {
		s.respondGroupError(c, err)
		return
	}

	tallies, ok := s.tallyGroups(c)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, s.toGroupResponse(group, tallies[strings.ToLower(group.Name)], true))
}

// UpdateGroup PUT /api/v1/groups/:name
// 改名会连带改写挂着这个组的任务分组标签（走调度器，堆内条目一起改）。
func (s *Server) UpdateGroup(c *gin.Context) {
	name := c.Param("name")

	var req UpdateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid request body", Details: err.Error()})
		return
	}

	current, found, err := s.groups.Get(name)
	if err != nil {
		c.JSON(500, ErrorResponse{Code: 500, Message: "failed to read groups", Details: err.Error()})
		return
	}
	if !found {
		c.JSON(404, ErrorResponse{Code: 404, Message: "group not found"})
		return
	}

	updated := current
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.Color != nil {
		updated.Color = *req.Color
	}
	renamedTo := ""
	if req.Name != nil && *req.Name != current.Name {
		if err := core.ValidateGroupName(*req.Name); err != nil {
			c.JSON(400, ErrorResponse{Code: 400, Message: "invalid group name", Details: err.Error()})
			return
		}
		_, clash, err := s.groups.Get(*req.Name)
		if err != nil {
			c.JSON(500, ErrorResponse{Code: 500, Message: "failed to read groups", Details: err.Error()})
			return
		}
		if clash {
			c.JSON(409, ErrorResponse{
				Code:    409,
				Message: "group already exists",
				Details: fmt.Sprintf("%q is already registered", *req.Name),
			})
			return
		}
		renamedTo = *req.Name
		updated.Name = *req.Name
	}

	if err := s.groups.Save(updated); err != nil {
		s.respondGroupError(c, err)
		return
	}
	if renamedTo != "" {
		// 主键是小写名称：改名要先把旧键删掉，否则新旧两个键同时存在
		if err := s.groups.Delete(current.Name); err != nil && !errors.Is(err, core.ErrGroupNotFound) {
			c.JSON(500, ErrorResponse{Code: 500, Message: "failed to rename group", Details: err.Error()})
			return
		}
		if _, err := s.scheduler.RetagGroup(current.Name, updated.Name); err != nil {
			s.logger.Error("group renamed but jobs were not retagged",
				"from", current.Name, "to", updated.Name, "error", err)
		}
	}

	tallies, ok := s.tallyGroups(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s.toGroupResponse(updated, tallies[strings.ToLower(updated.Name)], true))
}

// DeleteGroup DELETE /api/v1/groups/:name
// 默认 detach：组内任务只解除分组、不会被删除或取消（决策 D5）。
// 显式 ?strategy=block 时组内还有任务就返回 409，留给"先自己清干净"的用法。
func (s *Server) DeleteGroup(c *gin.Context) {
	name := c.Param("name")

	strategy := c.Query("strategy")
	if strategy == "" {
		strategy = groupDeleteStrategyDetach
	}
	if strategy != groupDeleteStrategyDetach && strategy != groupDeleteStrategyBlock {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "unsupported strategy",
			Details: fmt.Sprintf("got %q, expected detach or block", strategy),
		})
		return
	}

	group, found, err := s.groups.Get(name)
	if err != nil {
		c.JSON(500, ErrorResponse{Code: 500, Message: "failed to read groups", Details: err.Error()})
		return
	}
	if !found {
		c.JSON(404, ErrorResponse{Code: 404, Message: "group not found"})
		return
	}

	tallies, ok := s.tallyGroups(c)
	if !ok {
		return
	}
	tally := tallies[strings.ToLower(group.Name)]

	if tally.jobs > 0 {
		if strategy == groupDeleteStrategyBlock {
			c.JSON(409, ErrorResponse{
				Code:    409,
				Message: "group is not empty",
				Details: fmt.Sprintf("%d job(s) still tagged with %q; move them first or drop the strategy param", tally.jobs, group.Name),
			})
			return
		}
		// 先解除挂载再删注册表：反过来的话中途失败会留下"组没了、任务还挂着它"
		if _, err := s.scheduler.RetagGroup(group.Name, ""); err != nil {
			c.JSON(500, ErrorResponse{Code: 500, Message: "failed to detach jobs", Details: err.Error()})
			return
		}
	}

	if err := s.groups.Delete(group.Name); err != nil {
		s.respondGroupError(c, err)
		return
	}

	s.logger.Info("group deleted", "group", group.Name, "detached_jobs", tally.jobs)
	c.Status(http.StatusNoContent)
}

// respondGroupError 把分组存储的错误映射成状态码。
func (s *Server) respondGroupError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, core.ErrGroupNameInvalid):
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid group name", Details: err.Error()})
	case errors.Is(err, core.ErrGroupColorInvalid):
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid group color", Details: err.Error()})
	case errors.Is(err, core.ErrGroupNotFound):
		c.JSON(404, ErrorResponse{Code: 404, Message: "group not found"})
	default:
		c.JSON(500, ErrorResponse{Code: 500, Message: "failed to write groups", Details: err.Error()})
	}
}

// requireGroupStore 在没装配分组注册表的部署里挡住分组端点。
// 任务的 group 标签仍然可用（它在任务快照里），只有注册表需要独立存储。
func (s *Server) requireGroupStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.groups == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
				Code:    http.StatusServiceUnavailable,
				Message: "group registry is not configured",
				Details: "start the server with api.WithGroupStore to enable /api/v1/groups",
			})
			return
		}
		c.Next()
	}
}
