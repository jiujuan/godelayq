package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jiujuan/godelayq/internal/core"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Principal 是一次已通过认证的身份。machine 表示"用静态 token 进来的程序"。
type Principal struct {
	Name string
	Role core.Role
}

// machinePrincipalName 是静态 token 对应的身份名，仅用于日志与审计显示。
const machinePrincipalName = "machine"

// minBcryptCost 是配置里允许的最弱哈希。低于它的哈希在现代硬件上几秒就能离线爆破，
// 与其运行时悄悄放过，不如启动时就拒绝这份配置。
const minBcryptCost = 10

// ErrBadCredentials 对"用户不存在"与"密码错误"返回同一个哨兵错误：
// 调用方据此回 401 + 同一文案，避免把账号列表泄露给攻击者。
var ErrBadCredentials = errors.New("invalid credentials")

// Authenticator 校验账号密码并签发/验证访问令牌。
// 账号有两个来源，构造时二选一：
//   - store 非 nil（管理库模式）：账号在 core.UserStore 里，本类型经缓存读库，
//     改账号即时生效（写方负责调 InvalidateUser）；
//   - store 为 nil（遗留配置模式）：账号来自构造时那份配置快照，改账号需重启。
//
// 令牌状态由 authStore 承载，本类型可并发使用。
type Authenticator struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	logger     *slog.Logger

	// users 是遗留配置模式的账号快照；库模式下保持 nil，账号一律走 store。
	users map[string]userCredential
	// store 非 nil 表示库模式。nil 是遗留模式的标记。
	store core.UserStore
	// hasAccounts 是库模式构造时的账号数快照，供 Enabled 使用：
	// "这台进程要不要鉴权"不该随账号增删在运行期翻转——从无到有需要第一个账号，
	// 而第一个账号只能来自导入或带外手段；从有到无（删光）时保持鉴权是安全侧倾斜。
	hasAccounts bool

	// cache 是库模式的账号缓存（含"不存在"的负缓存）：验签路径每个请求都要
	// 对一次"令牌里的身份现在还是什么档位"，直查库会让单连接成为全部请求的串行点。
	// 负缓存挡住伪造 sub 的请求打库；写方（账号管理 API）落库后调 InvalidateUser。
	cacheMu sync.RWMutex
	cache   map[string]cachedCredential

	// dummyHash 让"用户不存在"分支也付出一条 bcrypt 的代价，
	// 否则响应时间的差异本身就是一次账号枚举。
	dummyHash string
}

type userCredential struct {
	hash string
	role core.Role
}

// cachedCredential 是缓存里的一格。exists=false 是负缓存：
// "查过、没有这个人"也要记下来，否则伪造的 sub 每个请求都打一次库。
type cachedCredential struct {
	cred   userCredential
	exists bool
}

// NewAuthenticator 构造认证器。store 为 nil 且配置无账号时返回 nil，
// 表示控制台鉴权未启用。库模式下账号哈希在导入/建号时已校验过，
// 这里只回答"库能不能读"；遗留模式仍在构造期校验哈希与 cost。
// 密码哈希格式非法在这里就报错，而不是等到用户第一次登录才发现。
func NewAuthenticator(cfg core.AuthConfig, store core.UserStore, logger *slog.Logger) (*Authenticator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if store == nil && len(cfg.Users) == 0 {
		return nil, nil
	}
	if cfg.JWT.Secret == "" {
		return nil, errors.New("server.auth.jwt.secret is required when accounts are configured")
	}

	a := &Authenticator{
		secret:     []byte(cfg.JWT.Secret),
		accessTTL:  cfg.JWT.AccessTTL,
		refreshTTL: cfg.JWT.RefreshTTL,
		logger:     logger,
		store:      store,
	}
	if a.accessTTL <= 0 {
		a.accessTTL = core.DefaultAccessTTL
	}
	if a.refreshTTL <= 0 {
		a.refreshTTL = core.DefaultRefreshTTL
	}

	if store != nil {
		a.cache = make(map[string]cachedCredential)
		count, err := store.Count()
		if err != nil {
			return nil, fmt.Errorf("read account store: %w", err)
		}
		a.hasAccounts = count > 0
	} else {
		a.users = make(map[string]userCredential, len(cfg.Users))
		for _, user := range cfg.Users {
			name := strings.TrimSpace(user.Name)
			cost, err := bcrypt.Cost([]byte(user.PasswordBcrypt))
			if err != nil {
				return nil, fmt.Errorf("account %q has an invalid bcrypt hash: %w", name, err)
			}
			// cost 低于 10 的哈希在现代硬件上几秒就能离线爆破；
			// 与其运行时悄悄放过，不如启动时就拒绝这份配置。
			if cost < minBcryptCost || cost > 15 {
				return nil, fmt.Errorf("account %q has bcrypt cost %d, use %d-15", name, cost, minBcryptCost)
			}
			role, ok := user.ResolveRole()
			if !ok {
				return nil, fmt.Errorf("account %q has invalid role %q", name, user.Role)
			}
			a.users[name] = userCredential{hash: user.PasswordBcrypt, role: role}
		}
	}

	dummy, err := bcrypt.GenerateFromPassword([]byte("godelayq-timing-equalizer"), 10)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare credential timing guard: %w", err)
	}
	a.dummyHash = string(dummy)

	return a, nil
}

// Enabled 表示控制台账号可用。库模式看构造时的账号快照（见字段注释），
// 遗留模式看配置里有没有账号。
func (a *Authenticator) Enabled() bool {
	if a == nil {
		return false
	}
	if a.store != nil {
		return a.hasAccounts
	}
	return len(a.users) > 0
}

// dbBacked 表示账号是否来自管理库。鉴权启用判定（Server.authRequired）用它把
// "库里有账号"计入，配置里的 users 节在库模式下只是导入源。
func (a *Authenticator) dbBacked() bool { return a != nil && a.store != nil }

// current 取账号的当前凭据：遗留模式读构造快照，库模式经缓存查库。
// 缓存键统一小写：库的唯一性本来就是 NOCASE，登录名大小写变体命中同一账号
// （遗留模式保持原样——快照键就是配置书写的大小写，不在这里悄悄改语义）。
// 查库失败按"不存在"处理（fail closed）且不写缓存：瞬时故障不该永久拉黑一个账号，
// 也不该被误读成缓存里有把握的答案。
func (a *Authenticator) current(name string) (userCredential, bool) {
	if a.store == nil {
		cred, ok := a.users[strings.TrimSpace(name)]
		return cred, ok
	}
	key := strings.ToLower(strings.TrimSpace(name))

	a.cacheMu.RLock()
	hit, ok := a.cache[key]
	a.cacheMu.RUnlock()
	if ok {
		return hit.cred, hit.exists
	}

	rec, found, err := a.store.Get(name)
	if err != nil {
		a.logger.Error("failed to read account from store",
			"account", name, "error", err)
		return userCredential{}, false
	}
	cred := cachedCredential{exists: found}
	if found {
		role, ok := core.ParseRole(rec.Role)
		if !ok {
			// 库里存着不认识的角色：按不存在处理，但不写负缓存——
			// 下一轮再查一次，让"库里的坏数据"在日志里反复可见。
			a.logger.Error("account carries an unknown role",
				"account", rec.Name, "role", rec.Role)
			return userCredential{}, false
		}
		cred.cred = userCredential{hash: rec.PasswordBcrypt, role: role}
	}

	a.cacheMu.Lock()
	a.cache[key] = cred
	a.cacheMu.Unlock()
	return cred.cred, cred.exists
}

// InvalidateUser 让一个账号的缓存失效。账号管理 API 落库后调用：
// 改角色、改密码、删号即时生效的语义就落在这条线上。
func (a *Authenticator) InvalidateUser(name string) {
	if a == nil || a.store == nil {
		return
	}
	a.cacheMu.Lock()
	delete(a.cache, strings.ToLower(strings.TrimSpace(name)))
	a.cacheMu.Unlock()
}

// InvalidateAll 清空账号缓存。导入完成、批量变更等"说不清改了谁"的场合用。
func (a *Authenticator) InvalidateAll() {
	if a == nil || a.store == nil {
		return
	}
	a.cacheMu.Lock()
	a.cache = make(map[string]cachedCredential)
	a.cacheMu.Unlock()
}

// Authenticate 校验账号密码。无论失败原因是哪种，返回同一个错误。
func (a *Authenticator) Authenticate(username, password string) (Principal, error) {
	cred, ok := a.current(username)
	if !ok {
		// 仍然比对一次，把时间差抹平
		_ = bcrypt.CompareHashAndPassword([]byte(a.dummyHash), []byte(password))
		return Principal{}, ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cred.hash), []byte(password)); err != nil {
		return Principal{}, ErrBadCredentials
	}
	name := strings.TrimSpace(username)
	return Principal{Name: name, Role: cred.role}, nil
}

// Session 是一次成功登录/刷新的产物。
type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Principal    Principal
	TokenID      string
}

// IssueSession 为身份签发一对令牌。refreshToken 是随机串，服务端留档后才可用。
func (a *Authenticator) IssueSession(p Principal, store *authStore) (Session, error) {
	jti, err := randomTokenID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now()
	expiresAt := now.Add(a.accessTTL)

	claims := jwt.MapClaims{
		"sub":  p.Name,
		"role": p.Role.String(),
		"iat":  now.Unix(),
		"exp":  expiresAt.Unix(),
		"jti":  jti,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
	if err != nil {
		return Session{}, fmt.Errorf("failed to sign access token: %w", err)
	}

	refresh, err := store.putRefresh(p, a.refreshTTL)
	if err != nil {
		return Session{}, err
	}

	return Session{
		AccessToken:  signed,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		Principal:    p,
		TokenID:      jti,
	}, nil
}

// TokenInfo 是一次已通过验签的访问令牌的元数据。
type TokenInfo struct {
	// ID 是令牌的 jti，登出时按它写入拒绝表
	ID string
	// ExpiresAt 是令牌自然过期时间，供 /auth/me 显示剩余有效期
	ExpiresAt time.Time
}

// VerifyAccessToken 验签并返回身份与元数据。已登出的令牌（jti 在拒绝表中）在这里被拒。
func (a *Authenticator) VerifyAccessToken(token string, store *authStore) (Principal, TokenInfo, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		// 只认 HS256：否则 alg 混淆（none / RS256 当成公钥验）是经典攻击面
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return a.secret, nil
	})
	if err != nil {
		return Principal{}, TokenInfo{}, err
	}
	if !parsed.Valid {
		return Principal{}, TokenInfo{}, errors.New("invalid token")
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return Principal{}, TokenInfo{}, errors.New("malformed token claims")
	}
	sub, _ := claims["sub"].(string)
	roleName, _ := claims["role"].(string)
	jti, _ := claims["jti"].(string)
	if sub == "" || jti == "" {
		return Principal{}, TokenInfo{}, errors.New("token is missing sub or jti")
	}

	role, ok := core.ParseRole(roleName)
	if !ok {
		return Principal{}, TokenInfo{}, fmt.Errorf("token carries unknown role %q", roleName)
	}
	p := Principal{Name: sub, Role: role}

	// 令牌里写着 admin，但账号可能已被降权或删掉：以账号的当前状态为准
	// （遗留模式=配置快照，库模式=缓存穿透后的那一查）。
	current, exists := a.current(sub)
	if !exists {
		return Principal{}, TokenInfo{}, fmt.Errorf("token subject %q no longer exists", sub)
	}
	if current.role != role {
		return Principal{}, TokenInfo{}, fmt.Errorf("token role %q no longer matches account %q", roleName, sub)
	}

	if store.isRevoked(jti) {
		return Principal{}, TokenInfo{}, errors.New("token has been logged out")
	}

	info := TokenInfo{ID: jti}
	if exp, ok := claims["exp"].(float64); ok {
		info.ExpiresAt = time.Unix(int64(exp), 0)
	}
	return p, info, nil
}

// Refresh 用 refresh token 换一对新令牌，并轮转 refresh token（旧的立即作废）。
func (a *Authenticator) Refresh(refreshToken string, store *authStore) (Session, error) {
	p, ok := a.userByName(store.takeSubject(refreshToken))
	if !ok {
		return Session{}, ErrBadCredentials
	}
	// takeSubject 已把旧 rt 消费掉，等同轮转
	session, err := a.IssueSession(p, store)
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// Logout 吊销 refresh token，并把 access token 的 jti 记入拒绝表直到其自然过期。
func (a *Authenticator) Logout(accessToken, refreshToken string, store *authStore) {
	if refreshToken != "" {
		store.dropRefresh(refreshToken)
	}
	if accessToken == "" {
		return
	}
	// 已登出/过期的令牌不必再解析：拒绝表只关心还活着的 jti。
	// 这里跳过 claims 校验（尤其是 exp），因为"过期但仍在拒绝表窗口内"的令牌
	// 记录一下也无害，而验证签名对吊销没有任何帮助——吊销表才是权威。
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := jwt.MapClaims{}
	if _, _, err := parser.ParseUnverified(accessToken, claims); err != nil {
		return
	}
	jti, _ := claims["jti"].(string)
	exp, _ := claims["exp"].(float64)
	if jti == "" || exp <= 0 {
		return
	}
	store.revoke(jti, time.Unix(int64(exp), 0))
}

// userByName 供静态 token 之外的身份查询使用（例如审计日志补全角色名）。
func (a *Authenticator) userByName(name string) (Principal, bool) {
	cred, ok := a.current(name)
	if !ok {
		return Principal{}, false
	}
	return Principal{Name: strings.TrimSpace(name), Role: cred.role}, true
}

// randomTokenID 生成 16 字节随机标识的十六进制形式，用于 jti 与 ticket。
func randomTokenID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to read randomness: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// accountNames 返回排序后的账号名，用于启动日志（不打印哈希）。
// 库模式直接问存储：构造期的快照可能已经被导入改写。
func (a *Authenticator) accountNames() []string {
	if a.store != nil {
		records, err := a.store.List()
		if err != nil {
			// 启动日志读不出账号：库里刚才还 Count 得出来，这里失败意味着存储
			// 正在出问题。报 empty 并留一条 error，别让启动日志看起来一切正常。
			a.logger.Error("failed to list accounts for the startup log", "error", err)
			return nil
		}
		names := make([]string, 0, len(records))
		for _, record := range records {
			names = append(names, record.Name)
		}
		sort.Strings(names)
		return names
	}
	names := make([]string, 0, len(a.users))
	for name := range a.users {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
