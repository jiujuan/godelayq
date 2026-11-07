package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jiujuan/godelayq/internal/core"
	"golang.org/x/crypto/bcrypt"
)

// createUserCost 是账号管理 API 为新密码选的 bcrypt cost。
// 认证器收 10-15；这里取 12：一次登录几十毫秒的可接受范围里，比下限
// 多挡一层离线爆破。登录开销与建号开销是两回事——建号是 admin 的低频操作。
const createUserCost = 12

// maxUserNameLen 是登录名的长度上界。名字会进令牌、台账与日志，
// 无上界等于让一个请求把三者都撑大；下界由非空校验承担。
const maxUserNameLen = 64

// UserView 是账号的对外形状：绝不含哈希。哈希离开存储没有任何合法用途，
// 而列表端点是 admin 档位的常访页面，回哈希等于把爆破材料摆在页面上。
type UserView struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toUserView(record core.UserRecord) UserView {
	return UserView{
		Name:      record.Name,
		Role:      record.Role,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}
}

// requireUserStore 在没有账号存储的部署里挡住管理端点。
// 与 requireAudit 同一取向：503 说的是"这台服务器没在管账号"（遗留配置模式），
// 而不是"没有账号"。
func (s *Server) requireUserStore(c *gin.Context) bool {
	if s.userStore == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
			Code:    http.StatusServiceUnavailable,
			Message: "account store is not configured",
			Details: "start the server with mgrstore.enabled to manage accounts at runtime",
		})
		return false
	}
	return true
}

type createUserRequest struct {
	Name     string `json:"name" binding:"required"`
	Password string `json:"password" binding:"required"`
	Role     string `json:"role" binding:"required"`
}

// ListUsers 返回全部账号（不含哈希）。
func (s *Server) ListUsers(c *gin.Context) {
	if !s.requireUserStore(c) {
		return
	}
	records, err := s.userStore.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to list accounts",
			Details: err.Error(),
		})
		return
	}
	views := make([]UserView, 0, len(records))
	for _, record := range records {
		views = append(views, toUserView(record))
	}
	c.JSON(http.StatusOK, views)
}

// CreateUser 建一个账号。密码由服务端哈希：明文只出现在这一次请求里，
// 不落任何存储（台账只记动作，不记体）。
func (s *Server) CreateUser(c *gin.Context) {
	if !s.requireUserStore(c) {
		return
	}
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid request body",
			Details: err.Error(),
		})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxUserNameLen {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid account name",
			Details: "name must be 1-64 characters",
		})
		return
	}
	if _, ok := core.ParseRole(req.Role); !ok {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid role",
			Details: "role must be one of viewer|operator|admin|ops",
		})
		return
	}

	// 已存在即拒绝而不是覆盖：Save 是 upsert 语义，直接 Save 会把既有账号
	// 的密码与角色无声换掉，"建号"请求绝不该有这种副作用。
	if _, exists, err := s.userStore.Get(name); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to read account",
			Details: err.Error(),
		})
		return
	} else if exists {
		c.JSON(http.StatusConflict, ErrorResponse{
			Code:    http.StatusConflict,
			Message: "account already exists",
			Details: "use PUT /api/v1/admin/users/" + name + " to change it",
		})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), createUserCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to hash password",
			Details: err.Error(),
		})
		return
	}

	record := core.UserRecord{Name: name, PasswordBcrypt: string(hash), Role: req.Role}
	if err := s.userStore.Save(record); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to save account",
			Details: err.Error(),
		})
		return
	}
	s.auth.InvalidateUser(name)
	c.JSON(http.StatusCreated, toUserView(record))
}

type updateUserRequest struct {
	// Password 与 Role 都是指针：nil 表示"这一项不动"。两个字段全 nil 的请求
	// 是写错的请求，回 400 而不是假装成功。
	Password *string `json:"password,omitempty"`
	Role     *string `json:"role,omitempty"`
}

// UpdateUser 改密码与/或角色。保护规则与 Delete 相同：库里必须始终留有
// 至少一个 admin——最后一个 admin 降权会让所有管理端点永久失去操作者。
func (s *Server) UpdateUser(c *gin.Context) {
	if !s.requireUserStore(c) {
		return
	}
	var req updateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid request body",
			Details: err.Error(),
		})
		return
	}
	if req.Password == nil && req.Role == nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "nothing to update",
			Details: "provide password and/or role",
		})
		return
	}

	name := strings.TrimSpace(c.Param("name"))
	existing, exists, err := s.userStore.Get(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to read account",
			Details: err.Error(),
		})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code:    http.StatusNotFound,
			Message: "account not found",
		})
		return
	}

	role := existing.Role
	if req.Role != nil {
		if _, ok := core.ParseRole(*req.Role); !ok {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Code:    http.StatusBadRequest,
				Message: "invalid role",
				Details: "role must be one of viewer|operator|admin|ops",
			})
			return
		}
		role = *req.Role
	}
	// 最后一个 admin 的降权在这里拦：改完之后库里必须还有 admin。
	if err := s.checkLastAdmin(existing, role); err != nil {
		c.JSON(http.StatusConflict, ErrorResponse{
			Code:    http.StatusConflict,
			Message: "cannot demote the last admin",
			Details: err.Error(),
		})
		return
	}

	hash := existing.PasswordBcrypt
	if req.Password != nil {
		newHash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), createUserCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Code:    http.StatusInternalServerError,
				Message: "failed to hash password",
				Details: err.Error(),
			})
			return
		}
		hash = string(newHash)
	}

	record := core.UserRecord{Name: existing.Name, PasswordBcrypt: hash, Role: role}
	if err := s.userStore.Save(record); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to save account",
			Details: err.Error(),
		})
		return
	}
	s.auth.InvalidateUser(existing.Name)
	c.JSON(http.StatusOK, toUserView(record))
}

// DeleteUser 删一个账号。两条保护规则：
//   - 不能删自己——正在用的会话删掉自己的账号，下一个请求就是 401；
//   - 删完之后库里必须还有 admin——最后一个 admin 被删，管理端点永久失去操作者。
func (s *Server) DeleteUser(c *gin.Context) {
	if !s.requireUserStore(c) {
		return
	}
	name := strings.TrimSpace(c.Param("name"))

	principal, ok := PrincipalFrom(c)
	if ok && strings.EqualFold(principal.Name, name) {
		c.JSON(http.StatusConflict, ErrorResponse{
			Code:    http.StatusConflict,
			Message: "cannot delete your own account",
			Details: "sign in as another admin to delete this account",
		})
		return
	}

	existing, exists, err := s.userStore.Get(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to read account",
			Details: err.Error(),
		})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code:    http.StatusNotFound,
			Message: "account not found",
		})
		return
	}
	if err := s.checkLastAdmin(existing, ""); err != nil {
		c.JSON(http.StatusConflict, ErrorResponse{
			Code:    http.StatusConflict,
			Message: "cannot delete the last admin",
			Details: err.Error(),
		})
		return
	}

	if err := s.userStore.Delete(existing.Name); err != nil {
		if errors.Is(err, core.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, ErrorResponse{
				Code:    http.StatusNotFound,
				Message: "account not found",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to delete account",
			Details: err.Error(),
		})
		return
	}
	s.auth.InvalidateUser(existing.Name)
	c.Status(http.StatusNoContent)
}

// checkLastAdmin 守住"库里始终至少有一个 admin"：
// demoteTo 为空串表示删除（现役 admin 即将彻底离开），
// 非空表示改角色（现役 admin 即将变成别的档位）。
// 目标仍是 admin（或非 admin 账号的操作）不触碰这条线。
func (s *Server) checkLastAdmin(existing core.UserRecord, demoteTo string) error {
	if existing.Role != core.RoleAdmin.String() {
		return nil
	}
	if demoteTo == core.RoleAdmin.String() {
		return nil
	}
	records, err := s.userStore.List()
	if err != nil {
		return err
	}
	admins := 0
	for _, record := range records {
		if record.Role == core.RoleAdmin.String() {
			admins++
		}
	}
	if admins > 1 {
		return nil
	}
	return errors.New("account " + existing.Name + " is the only admin; create another admin first")
}
