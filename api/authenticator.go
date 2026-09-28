package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"godelayq/core"

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
// 账号来自配置（改账号需重启），令牌状态由 authStore 承载，本类型无状态、可并发使用。
type Authenticator struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	users      map[string]userCredential
	// dummyHash 让"用户不存在"分支也付出一条 bcrypt 的代价，
	// 否则响应时间的差异本身就是一次账号枚举。
	dummyHash string
}

type userCredential struct {
	hash string
	role core.Role
}

// NewAuthenticator 按配置构造认证器。账号为空时返回 nil，表示控制台鉴权未启用。
// 密码哈希格式非法在这里就报错，而不是等到用户第一次登录才发现。
func NewAuthenticator(cfg core.AuthConfig) (*Authenticator, error) {
	if len(cfg.Users) == 0 {
		return nil, nil
	}
	if cfg.JWT.Secret == "" {
		return nil, errors.New("server.auth.jwt.secret is required when accounts are configured")
	}

	a := &Authenticator{
		secret:     []byte(cfg.JWT.Secret),
		accessTTL:  cfg.JWT.AccessTTL,
		refreshTTL: cfg.JWT.RefreshTTL,
		users:      make(map[string]userCredential, len(cfg.Users)),
	}
	if a.accessTTL <= 0 {
		a.accessTTL = core.DefaultAccessTTL
	}
	if a.refreshTTL <= 0 {
		a.refreshTTL = core.DefaultRefreshTTL
	}

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

	dummy, err := bcrypt.GenerateFromPassword([]byte("godelayq-timing-equalizer"), 10)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare credential timing guard: %w", err)
	}
	a.dummyHash = string(dummy)

	return a, nil
}

// Enabled 表示是否配置了控制台账号。
func (a *Authenticator) Enabled() bool { return a != nil && len(a.users) > 0 }

// Authenticate 校验账号密码。无论失败原因是哪种，返回同一个错误。
func (a *Authenticator) Authenticate(username, password string) (Principal, error) {
	cred, ok := a.users[strings.TrimSpace(username)]
	if !ok {
		// 仍然比对一次，把时间差抹平
		_ = bcrypt.CompareHashAndPassword([]byte(a.dummyHash), []byte(password))
		return Principal{}, ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cred.hash), []byte(password)); err != nil {
		return Principal{}, ErrBadCredentials
	}
	return Principal{Name: strings.TrimSpace(username), Role: cred.role}, nil
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

	// 令牌里写着 admin，但账号可能已在配置文件里被降权或删掉：以当前配置为准。
	current, exists := a.users[sub]
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

// LookupRole 供静态 token 之外的身份查询使用（例如审计日志补全角色名）。
func (a *Authenticator) userByName(name string) (Principal, bool) {
	cred, ok := a.users[name]
	if !ok {
		return Principal{}, false
	}
	return Principal{Name: name, Role: cred.role}, true
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
func (a *Authenticator) accountNames() []string {
	names := make([]string, 0, len(a.users))
	for name := range a.users {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
