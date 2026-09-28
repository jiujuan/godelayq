package core

import "strings"

// Role 是控制台的访问档位。
//
// viewer/operator/admin/ops 构成有序阶梯，路由鉴权用 AtLeast 比较即可。
// machine 是静态 token 的身份：它的等级刻意等同 operator，
// 于是 AtLeast(RoleAdmin) 天然为否——脚本能建任务、能取消任务，
// 但拿不到强制暂停、删组、运维这些"替人做破坏性决定"的权限。
type Role int

const (
	RoleViewer Role = iota + 1
	RoleOperator
	RoleAdmin
	RoleOps
	// RoleMachine 表示"用静态 token 进来的程序"，不是配置里可填写的角色。
	RoleMachine
)

// rank 是档位比较用的序号；machine 与 operator 同级。
func (r Role) rank() int {
	if r == RoleMachine {
		return int(RoleOperator)
	}
	return int(r)
}

// AtLeast 判断当前档位是否达到 min 要求。未知档位按最低处理（拒绝）。
func (r Role) AtLeast(min Role) bool {
	if !r.valid() {
		return false
	}
	return r.rank() >= min.rank()
}

func (r Role) valid() bool {
	return r >= RoleViewer && r <= RoleMachine
}

// String 返回配置与 API 里使用的角色名；machine 只出现在凭据通道，不接受配置填写。
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	case RoleOps:
		return "ops"
	case RoleMachine:
		return "machine"
	default:
		return "unknown"
	}
}

// ParseRole 解析配置里的角色名（大小写不敏感）。machine 不可配置，故不在此接受。
func ParseRole(name string) (Role, bool) {
	for _, role := range []Role{RoleViewer, RoleOperator, RoleAdmin, RoleOps} {
		if strings.EqualFold(role.String(), strings.TrimSpace(name)) {
			return role, true
		}
	}
	return 0, false
}
