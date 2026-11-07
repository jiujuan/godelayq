package mgr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jiujuan/godelayq/internal/core"
	"golang.org/x/crypto/bcrypt"
)

// hashOf 生成指定口令的 bcrypt 哈希，供导入用例构造 config 账号。
func hashOf(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	require.NoError(t, err)
	return string(h)
}

func TestImportOnce_UsersFromConfig(t *testing.T) {
	h := openMgr(t)

	src := ImportSource{AuthUsers: []core.UserConfig{
		{Name: "admin", PasswordBcrypt: hashOf(t, "pw-admin-1"), Role: "admin"},
		{Name: "viewer", PasswordBcrypt: hashOf(t, "pw-viewer-1"), Role: "viewer"},
	}}
	require.NoError(t, ImportOnce(h, src, nil))

	store := NewUserStore(h)
	rec, ok, err := store.Get("ADMIN")
	require.NoError(t, err)
	require.True(t, ok, "账号应已入库，大小写不敏感可查")
	assert.Equal(t, "admin", rec.Role)

	// 幂等：重跑不报错、不重复
	require.NoError(t, ImportOnce(h, src, nil))
	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestImportOnce_NeverOverwritesExistingAccounts(t *testing.T) {
	h := openMgr(t)
	store := NewUserStore(h)

	// 库里已有同名账号（角色不同、哈希不同）——模拟"上一次运行经 API 建号"
	existing := core.UserRecord{Name: "admin", PasswordBcrypt: hashOf(t, "current-password"), Role: "ops"}
	require.NoError(t, store.Save(existing))

	src := ImportSource{AuthUsers: []core.UserConfig{
		{Name: "admin", PasswordBcrypt: hashOf(t, "old-config-password"), Role: "viewer"},
		{Name: "extra", PasswordBcrypt: hashOf(t, "pw-extra-1"), Role: "viewer"},
	}}
	require.NoError(t, ImportOnce(h, src, nil))

	// 现网账号原样保留，config 里的账号一条都不写
	rec, ok, err := store.Get("admin")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ops", rec.Role)
	_, ok, err = store.Get("extra")
	require.NoError(t, err)
	assert.False(t, ok, "库非空时配置导入必须整体跳过")

	// 标记已补写：之后再跑也不导入
	require.NoError(t, ImportOnce(h, src, nil))
	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestImportOnce_RejectsWeakOrMalformedHashes(t *testing.T) {
	weak, err := bcrypt.GenerateFromPassword([]byte("weak"), 4)
	require.NoError(t, err)

	cases := map[string][]core.UserConfig{
		"cost 过低": {{Name: "a", PasswordBcrypt: string(weak), Role: "admin"}},
		"非哈希文本":   {{Name: "a", PasswordBcrypt: "plain-text", Role: "admin"}},
		"非法角色":    {{Name: "a", PasswordBcrypt: testHash, Role: "machine"}},
	}
	for name, users := range cases {
		t.Run(name, func(t *testing.T) {
			h := openMgr(t)
			err := ImportOnce(h, ImportSource{AuthUsers: users}, nil)
			require.Error(t, err)
		})
	}
}

func TestImportOnce_GroupsAndProfilesFromFiles(t *testing.T) {
	dir := t.TempDir()

	// 先用 JSON 存储造两份"历史文件"——与真实升级路径同一形态
	legacyGroups := filepath.Join(dir, "groups.json")
	gs, err := core.NewJSONFileGroupStore(legacyGroups)
	require.NoError(t, err)
	require.NoError(t, gs.Save(core.Group{Name: "prod", Description: "生产环境", Color: "#ff0000"}))
	require.NoError(t, gs.Save(core.Group{Name: "test", Color: "#00ff00"}))

	legacyProfiles := filepath.Join(dir, "profiles.json")
	ps, err := core.NewJSONFileExecutorProfileStore(legacyProfiles)
	require.NoError(t, err)
	require.NoError(t, ps.Save(core.ExecutorProfileRecord{
		Name: "demo", Kind: "script", Runtime: "bash", Script: "echo hi",
	}))

	h := openMgr(t)
	require.NoError(t, ImportOnce(h, ImportSource{GroupsPath: legacyGroups, ProfilesPath: legacyProfiles}, nil))

	// 分组入库、名字与时间戳保留
	groupStore := NewGroupStore(h)
	groups, err := groupStore.List()
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "prod", groups[0].Name)
	assert.Equal(t, "生产环境", groups[0].Description)
	assert.False(t, groups[0].CreatedAt.IsZero())

	// 档位入库、记录往返无损
	profileStore := NewExecutorProfileStore(h)
	record, ok, err := profileStore.Get("DEMO")
	require.NoError(t, err)
	require.True(t, ok, "档位名大小写不敏感可查")
	assert.Equal(t, "bash", record.Runtime)
	assert.Equal(t, "echo hi", record.Script)

	// 幂等：重跑不重复
	require.NoError(t, ImportOnce(h, ImportSource{GroupsPath: legacyGroups, ProfilesPath: legacyProfiles}, nil))
	groups, err = groupStore.List()
	require.NoError(t, err)
	assert.Len(t, groups, 2)
	profiles, err := profileStore.List()
	require.NoError(t, err)
	assert.Len(t, profiles, 1)
}

func TestImportOnce_EmptySourcesAreNoop(t *testing.T) {
	h := openMgr(t)
	// 三个来源全空：不报错、不写标记、不建任何行
	require.NoError(t, ImportOnce(h, ImportSource{}, nil))
	count, err := NewUserStore(h).Count()
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestImportOnce_MissingFilesCountAsImported(t *testing.T) {
	h := openMgr(t)
	// 文件不存在 = 没有历史数据，导入照常完成并写标记（部署从未用过分组是正常状态）
	require.NoError(t, ImportOnce(h, ImportSource{
		GroupsPath:   filepath.Join(t.TempDir(), "no-such-groups.json"),
		ProfilesPath: filepath.Join(t.TempDir(), "no-such-profiles.json"),
	}, nil))
	// 再跑一次仍是无害的
	require.NoError(t, ImportOnce(h, ImportSource{
		GroupsPath:   filepath.Join(t.TempDir(), "no-such-groups.json"),
		ProfilesPath: filepath.Join(t.TempDir(), "no-such-profiles.json"),
	}, nil))
}

func TestImportOnce_CorruptedLegacyFileFailsStartup(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "profiles.json")
	require.NoError(t, os.WriteFile(bad, []byte("{not json"), 0644))

	h := openMgr(t)
	err := ImportOnce(h, ImportSource{ProfilesPath: bad}, nil)
	require.Error(t, err, "损坏的旧文件不能当成空集合：那会让页面上的档位无声消失")
}
