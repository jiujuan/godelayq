package api

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// authStore 承载令牌的"服务端状态"：refresh token、已登出的 access token、
// 以及给 WebSocket / SSE 用的一次性 ticket。
//
// 全部在内存里：进程重启后所有人都要重新登录。这是"不引入会话持久化与密钥落盘"
// 换来的代价，已在 docs/design/web-console-design.md §5.7.4 记为已知取舍。
type authStore struct {
	mu      sync.Mutex
	refresh map[string]refreshRecord
	revoked map[string]time.Time // jti → 自然过期时间，过期后即可丢弃
	tickets map[string]ticketRecord

	// now 便于测试注入时间
	now func() time.Time
	// sweepEvery 限制清理频率，避免每次请求都遍历全表
	lastSweep time.Time
}

type refreshRecord struct {
	subject string
	exp     time.Time
}

type ticketRecord struct {
	principal Principal
	exp       time.Time
}

// ticketTTL 是 ticket 的可用窗口。够前端建一次 WS 连接，又短到泄漏无意义。
const ticketTTL = 5 * time.Second

// sweepInterval 是过期条目的清理节拍。
const sweepInterval = time.Minute

// maxRevokedEntries 给拒绝表设上限：被强制登出的令牌寿命有限（accessTTL），
// 正常运营下不会填满；填满说明有人在刷 logout，此时丢弃最旧的（最早过期的）。
const maxRevokedEntries = 4096

var errTicketConsumed = errors.New("ticket already used or unknown")

// newAuthStore 创建空的凭据状态表。
func newAuthStore() *authStore {
	return &authStore{
		refresh: make(map[string]refreshRecord),
		revoked: make(map[string]time.Time),
		tickets: make(map[string]ticketRecord),
		now:     time.Now,
	}
}

// putRefresh 登记一个 refresh token 并返回其字符串形式。
func (s *authStore) putRefresh(p Principal, ttl time.Duration) (string, error) {
	token, err := randomTokenID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.refresh[token] = refreshRecord{subject: p.Name, exp: s.now().Add(ttl)}
	return token, nil
}

// takeSubject 消费一个 refresh token：无论成功与否它都不再可用（轮转语义）。
// 返回空串表示未知或已过期。
func (s *authStore) takeSubject(token string) string {
	if token == "" {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	rec, ok := s.refresh[token]
	if !ok {
		return ""
	}
	delete(s.refresh, token)
	if rec.exp.Before(s.now()) {
		return ""
	}
	return rec.subject
}

// dropRefresh 让 refresh token 立即失效（登出用）。
func (s *authStore) dropRefresh(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refresh, token)
}

// revoke 把 access token 的 jti 记入拒绝表，直到它自然过期。
func (s *authStore) revoke(jti string, expiresAt time.Time) {
	if jti == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !expiresAt.After(s.now()) {
		// 已经过期（或恰好此刻到期）的令牌拦不住任何请求，不必占表位
		return
	}
	if len(s.revoked) >= maxRevokedEntries {
		s.dropOldestRevokedLocked()
	}
	s.revoked[jti] = expiresAt
}

// isRevoked 判断 jti 是否已被登出。
func (s *authStore) isRevoked(jti string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	exp, ok := s.revoked[jti]
	if !ok {
		return false
	}
	if exp.Before(s.now()) {
		// 令牌本身已过期，拒绝表不再需要它
		delete(s.revoked, jti)
		return false
	}
	return true
}

// issueTicket 为身份发一张一次性 ticket，供 WS/SSE 握手使用。
// 凭据出现在 URL 里会被访问日志记下（api/logging.go），所以这里用短命 ticket 代替 JWT。
func (s *authStore) issueTicket(p Principal) (string, error) {
	token, err := randomTokenID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.tickets[token] = ticketRecord{principal: p, exp: s.now().Add(ticketTTL)}
	return token, nil
}

// consumeTicket 取出并删除 ticket：第二次使用同一个 ticket 必然失败。
func (s *authStore) consumeTicket(token string) (Principal, error) {
	if token == "" {
		return Principal{}, errTicketConsumed
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.tickets[token]
	if !ok {
		return Principal{}, errTicketConsumed
	}
	delete(s.tickets, token)
	if rec.exp.Before(s.now()) {
		return Principal{}, errTicketConsumed
	}
	return rec.principal, nil
}

// counts 暴露三张表的长度，供 /api/v1/admin/runtime 之类的诊断使用。
func (s *authStore) counts() (refresh, revoked, tickets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refresh), len(s.revoked), len(s.tickets)
}

// sweepLocked 清理过期条目。调用者必须持有 s.mu。
func (s *authStore) sweepLocked() {
	now := s.now()
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < sweepInterval {
		return
	}
	s.lastSweep = now

	for token, rec := range s.refresh {
		if rec.exp.Before(now) {
			delete(s.refresh, token)
		}
	}
	for jti, exp := range s.revoked {
		if exp.Before(now) {
			delete(s.revoked, jti)
		}
	}
	for token, rec := range s.tickets {
		if rec.exp.Before(now) {
			delete(s.tickets, token)
		}
	}
}

// dropOldestRevokedLocked 按过期时间从早到晚丢弃，保留还能拦得住的条目。
func (s *authStore) dropOldestRevokedLocked() {
	type entry struct {
		jti string
		exp time.Time
	}
	all := make([]entry, 0, len(s.revoked))
	for jti, exp := range s.revoked {
		all = append(all, entry{jti: jti, exp: exp})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].exp.Before(all[j].exp) })

	// 腾出 1/8 的空间，避免每次插入都排序一遍
	drop := len(all)/8 + 1
	for i := 0; i < drop && i < len(all); i++ {
		delete(s.revoked, all[i].jti)
	}
}
