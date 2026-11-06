package core

import (
	"errors"
	"strings"
	"time"
)

// UserRecord 是一个控制台账号的存储形态。类型放 core 是共享词汇的惯例：
// api 的认证器按它校验登录，store/mgr 按它落库，两边互不 import。
// 密码只存 bcrypt 哈希，明文在任何一层都不落盘——与配置里的 UserConfig 同一口径。
type UserRecord struct {
	// Name 登录名，库内唯一（大小写不敏感：登录与查重都用小写比较）
	Name string
	// PasswordBcrypt bcrypt 哈希（cmd/hashpassword 生成；账号管理 API 由服务端生成）
	PasswordBcrypt string
	// Role viewer|operator|admin|ops（ParseRole 可解析；machine 不可配置）
	Role string
	// CreatedAt/UpdatedAt 由存储实现维护
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrUserNotFound 与 ErrGroupNotFound 同一口径：按名字查不到时的哨兵错误。
var ErrUserNotFound = errors.New("user not found")

// ValidateUserRecord 检查一条账号记录自身的合法性。存储实现在 Save 前调用它，
// 账号管理 API 在落库前也用它拦一道——规则的唯一出处在这里。
// 哈希格式是否真是 bcrypt、cost 是否够高，留给 api 层用 bcrypt.Cost 校验：
// core 不该为了校验一个字符串而依赖 crypto 库（与 AuthConfig.Validate 的口径一致）。
func ValidateUserRecord(r UserRecord) error {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return errors.New("user name must not be empty")
	}
	if r.PasswordBcrypt == "" {
		return errors.New("user password_bcrypt must not be empty")
	}
	if _, ok := ParseRole(r.Role); !ok {
		return errors.New("user role must be one of viewer|operator|admin|ops")
	}
	return nil
}

// UserStore 保存控制台账号。接口形状与 GroupStore/ExecutorProfileStore 同一取向：
// 刻意不含 Flush/Close——账号是低频实体，每次改动同步落库即可，没有后台合并协程需要收尾。
//
// 实现方必须保证的语义：
//   - 名字唯一性按大小写不敏感判定（与配置期 AuthConfig.Validate 的查重口径一致）；
//   - Save 是 upsert：新建补 CreatedAt，覆盖保留已有记录的 CreatedAt；
//   - Delete 对不存在的名字返回 ErrUserNotFound。
type UserStore interface {
	// List 按名字字典序返回全部账号，顺序稳定以便直接铺到 UI 上
	List() ([]UserRecord, error)
	// Get 按名字查找（大小写不敏感）
	Get(name string) (UserRecord, bool, error)
	// Save 新建或整体覆盖一个账号；已存在时保留其 CreatedAt
	Save(record UserRecord) error
	// Delete 忽略大小写删除；不存在返回 ErrUserNotFound
	Delete(name string) error
	// Count 返回账号总数。"库里有没有账号"是鉴权启用判定与一次性导入判定的依据，
	// 单独给一个数而不是让调用方 List 后取 len：账号表只该为登录而整表读。
	Count() (int, error)
}
