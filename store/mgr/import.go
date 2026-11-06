package mgr

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"godelayq/core"

	"golang.org/x/crypto/bcrypt"
)

// minImportBcryptCost 与 api.NewAuthenticator 的启动校验同一口径（cost 10-15）：
// 配置里那份哈希当年要过这道门，导入时不放行更弱的。api 侧的常量不导出，
// 这里的同值常量只服务导入这一个时点。
const (
	minImportBcryptCost = 10
	maxImportBcryptCost = 15
)

// ImportSource 是一次性导入的三个来源。字段为零值表示那一项没有导入来源
// （比如部署从未配过账号），对应的标记不会写下——将来配了还能导。
type ImportSource struct {
	// AuthUsers config 里 server.auth.users 的原样副本。空表示本次没有可导的账号。
	AuthUsers []core.UserConfig
	// GroupsPath 旧的分组 JSON 文件（store.groups_path）。空表示跳过分组导入。
	GroupsPath string
	// ProfilesPath 旧的档位 JSON 文件（executors.profiles_path）。空表示跳过档位导入。
	ProfilesPath string
}

// ImportOnce 把旧存放位置的账号、分组、档位一次性导入管理库。
//
// 三个来源各自独立：标记在 mgr_meta 里分开记，哪项有源导哪项，互不阻塞。
// 幂等规则（每一项相同）：
//   - 标记已存在 → 跳过（导入是一次性事件，之后库是唯一事实源）；
//   - 库里已有数据且标记缺失 → 跳过并补写标记：绝不覆盖已有数据——那可能是
//     上一次运行经账号管理 API 建出来的，回写旧配置等于回滚现网。
//
// 任何一项导入失败都返回错误，由装配方拒绝启动：mgrstore.enabled=true 意味着
// 部署方已经决定以库为准，带着"半导半没导"的状态起来比起不来更难解释。
func ImportOnce(h *Handle, src ImportSource, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if len(src.AuthUsers) > 0 {
		if err := importUsers(h, src.AuthUsers, logger); err != nil {
			return fmt.Errorf("mgr: import accounts from config: %w", err)
		}
	}
	if src.GroupsPath != "" {
		if err := importGroups(h, src.GroupsPath, logger); err != nil {
			return fmt.Errorf("mgr: import groups from file: %w", err)
		}
	}
	if src.ProfilesPath != "" {
		if err := importProfiles(h, src.ProfilesPath, logger); err != nil {
			return fmt.Errorf("mgr: import executor profiles from file: %w", err)
		}
	}
	return nil
}

// importUsers 把 config 里的账号导进 mgr_users。
func importUsers(h *Handle, users []core.UserConfig, logger *slog.Logger) error {
	done, err := metaGet(h, metaUsersImported)
	if err != nil {
		return err
	}
	if done != "" {
		return nil
	}

	users_ := NewUserStore(h)
	count, err := users_.Count()
	if err != nil {
		return err
	}
	if count > 0 {
		logger.Info("mgr: users table is not empty, keep existing accounts and mark config import as done",
			"existing", count, "offered", len(users))
		return metaSetNow(h, metaUsersImported)
	}

	for i, user := range users {
		name := strings.TrimSpace(user.Name)
		cost, err := bcrypt.Cost([]byte(user.PasswordBcrypt))
		if err != nil {
			return fmt.Errorf("account %q (index %d) has an invalid bcrypt hash: %w", name, i, err)
		}
		if cost < minImportBcryptCost || cost > maxImportBcryptCost {
			return fmt.Errorf("account %q (index %d) has bcrypt cost %d, use %d-%d",
				name, i, cost, minImportBcryptCost, maxImportBcryptCost)
		}
		record := core.UserRecord{Name: name, PasswordBcrypt: user.PasswordBcrypt, Role: user.Role}
		if err := users_.Save(record); err != nil {
			return fmt.Errorf("account %q (index %d): %w", name, i, err)
		}
	}
	if err := metaSetNow(h, metaUsersImported); err != nil {
		return err
	}
	logger.Info("mgr: imported console accounts from config", "count", len(users))
	return nil
}

// importGroups 把旧的分组 JSON 文件导进 mgr_groups。
func importGroups(h *Handle, path string, logger *slog.Logger) error {
	done, err := metaGet(h, metaGroupsImported)
	if err != nil {
		return err
	}
	if done != "" {
		return nil
	}

	legacy, err := core.NewJSONFileGroupStore(path)
	if err != nil {
		return fmt.Errorf("open legacy group file %q: %w", path, err)
	}
	items, err := legacy.List()
	if err != nil {
		return fmt.Errorf("read legacy group file %q: %w", path, err)
	}

	store := NewGroupStore(h)
	for _, group := range items {
		// 记录自带 CreatedAt：Save 的"调用方显式带值"口径会原样保留，导入不篡改历史
		if err := store.Save(group); err != nil {
			return fmt.Errorf("group %q: %w", group.Name, err)
		}
	}
	if err := metaSetNow(h, metaGroupsImported); err != nil {
		return err
	}
	logger.Info("mgr: imported groups from legacy file", "count", len(items), "path", path)
	return nil
}

// importProfiles 把旧的档位 JSON 文件导进 mgr_executor_profiles。
func importProfiles(h *Handle, path string, logger *slog.Logger) error {
	done, err := metaGet(h, metaProfilesImported)
	if err != nil {
		return err
	}
	if done != "" {
		return nil
	}

	legacy, err := core.NewJSONFileExecutorProfileStore(path)
	if err != nil {
		return fmt.Errorf("open legacy executor profile file %q: %w", path, err)
	}
	items, err := legacy.List()
	if err != nil {
		return fmt.Errorf("read legacy executor profile file %q: %w", path, err)
	}

	store := NewExecutorProfileStore(h)
	for _, record := range items {
		if err := store.Save(record); err != nil {
			return fmt.Errorf("executor profile %q: %w", record.Name, err)
		}
	}
	if err := metaSetNow(h, metaProfilesImported); err != nil {
		return err
	}
	logger.Info("mgr: imported executor profiles from legacy file", "count", len(items), "path", path)
	return nil
}

// metaGet 读一个标记。缺失返回空串而不是错误——"还没导过"是正常状态。
func metaGet(h *Handle, key string) (string, error) {
	var value string
	err := h.SQL().QueryRow(`SELECT value FROM mgr_meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// metaSetNow 写一个标记，值是当前时刻——只为现场排障看"导没导过、什么时候导的"。
func metaSetNow(h *Handle, key string) error {
	_, err := h.SQL().Exec(`INSERT INTO mgr_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, strconv.FormatInt(time.Now().Unix(), 10))
	return err
}
