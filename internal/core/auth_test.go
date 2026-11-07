package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoleAtLeast(t *testing.T) {
	for _, tc := range []struct {
		role Role
		want map[Role]bool
	}{
		{RoleViewer, map[Role]bool{RoleViewer: true, RoleOperator: false, RoleAdmin: false, RoleOps: false}},
		{RoleOperator, map[Role]bool{RoleViewer: true, RoleOperator: true, RoleAdmin: false, RoleOps: false}},
		{RoleAdmin, map[Role]bool{RoleViewer: true, RoleOperator: true, RoleAdmin: true, RoleOps: false}},
		{RoleOps, map[Role]bool{RoleViewer: true, RoleOperator: true, RoleAdmin: true, RoleOps: true}},
		// machine 等同 operator：能写任务，但不能进 admin 门槛
		{RoleMachine, map[Role]bool{RoleViewer: true, RoleOperator: true, RoleAdmin: false, RoleOps: false}},
		// 未知档位一律拒绝，避免零值被当成最高权限
		{Role(0), map[Role]bool{RoleViewer: false, RoleOperator: false, RoleAdmin: false, RoleOps: false}},
		{Role(99), map[Role]bool{RoleViewer: false, RoleOperator: false, RoleAdmin: false, RoleOps: false}},
	} {
		for min, want := range tc.want {
			assert.Equal(t, want, tc.role.AtLeast(min), "%s.AtLeast(%s)", tc.role, min)
		}
	}
}

func TestRoleStringAndParse(t *testing.T) {
	for _, role := range []Role{RoleViewer, RoleOperator, RoleAdmin, RoleOps} {
		parsed, ok := ParseRole(role.String())
		assert.True(t, ok, role.String())
		assert.Equal(t, role, parsed)
	}

	// 配置里允许大小写混写与首尾空白
	parsed, ok := ParseRole(" Admin ")
	assert.True(t, ok)
	assert.Equal(t, RoleAdmin, parsed)

	// machine 是内部档位，不允许被配置出来，否则一个 token 就能拿到写权限之外的 admin 判定
	_, ok = ParseRole("machine")
	assert.False(t, ok)

	for _, name := range []string{"", "superuser", "root", "viewer2"} {
		_, ok = ParseRole(name)
		assert.False(t, ok, name)
	}

	assert.Equal(t, "unknown", Role(0).String())
	assert.Equal(t, "machine", RoleMachine.String())
}
