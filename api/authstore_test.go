package api

import (
	"strconv"
	"testing"
	"time"

	"godelayq/core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore 返回存储与一个可拨动的时钟指针，便于测过期行为而不必真的 sleep。
func newTestStore(t *testing.T) (*authStore, *time.Time) {
	t.Helper()

	current := time.Now()
	store := newAuthStore()
	store.now = func() time.Time { return current }
	return store, &current
}

func TestAuthStoreRefreshIsSingleUse(t *testing.T) {
	store, _ := newTestStore(t)

	token, err := store.putRefresh(Principal{Name: "rooter", Role: core.RoleAdmin}, time.Hour)
	require.NoError(t, err)

	assert.Equal(t, "rooter", store.takeSubject(token))
	assert.Empty(t, store.takeSubject(token), "同一个 refresh token 不能用第二次")
	assert.Empty(t, store.takeSubject(""), "空串不应命中任何记录")
	assert.Empty(t, store.takeSubject("deadbeefdeadbeefdeadbeefdeadbeef"), "未知串不应命中任何记录")
}

func TestAuthStoreRefreshExpires(t *testing.T) {
	store, current := newTestStore(t)

	token, err := store.putRefresh(Principal{Name: "rooter", Role: core.RoleAdmin}, time.Minute)
	require.NoError(t, err)

	*current = current.Add(2 * time.Minute)
	assert.Empty(t, store.takeSubject(token), "过期 refresh token 必须失效")
}

func TestAuthStoreDropRefresh(t *testing.T) {
	store, _ := newTestStore(t)

	token, err := store.putRefresh(Principal{Name: "rooter", Role: core.RoleAdmin}, time.Hour)
	require.NoError(t, err)

	store.dropRefresh(token)
	assert.Empty(t, store.takeSubject(token))
}

func TestAuthStoreRevokeHonoursExpiry(t *testing.T) {
	store, current := newTestStore(t)

	store.revoke("live-jti", current.Add(time.Minute))
	assert.True(t, store.isRevoked("live-jti"), "已登出的令牌应被拒绝")
	assert.False(t, store.isRevoked("never-issued"), "未登记的 jti 不受影响")

	// 令牌自然过期后，拒绝表可以忘记它：验签阶段已经会拦下
	*current = current.Add(2 * time.Minute)
	assert.False(t, store.isRevoked("live-jti"))
	_, revoked, _ := store.counts()
	assert.Zero(t, revoked, "过期条目应在查询时被顺带清掉")

	// 记录一个已经过期的 jti 没有意义，不该进表
	store.revoke("already-dead", *current)
	_, revoked, _ = store.counts()
	assert.Zero(t, revoked)
}

// 拒绝表必须有上界，并且丢的是先过期的那批——它们拦不住任何东西。
func TestAuthStoreRevokeEvictsOldestWhenFull(t *testing.T) {
	store, current := newTestStore(t)

	// 先塞满，过期时间从早到晚（从 1 秒起：此刻到期的条目不会被记录，那是另一个用例）
	for i := 1; i <= maxRevokedEntries; i++ {
		store.revoke("jti-"+strconv.Itoa(i), current.Add(time.Duration(i)*time.Second))
	}
	_, revoked, _ := store.counts()
	require.Equal(t, maxRevokedEntries, revoked)

	store.revoke("newcomer", current.Add(time.Hour))
	_, revoked, _ = store.counts()
	assert.Less(t, revoked, maxRevokedEntries, "溢出时应腾出空间而不是无界增长")
	assert.True(t, store.isRevoked("newcomer"), "最新登出的令牌必须仍然被拦住")
	assert.False(t, store.isRevoked("jti-1"), "最早过期的条目先被丢弃")
}

func TestAuthStoreTicketIsOneTime(t *testing.T) {
	store, current := newTestStore(t)
	principal := Principal{Name: "watcher", Role: core.RoleViewer}

	ticket, err := store.issueTicket(principal)
	require.NoError(t, err)

	got, err := store.consumeTicket(ticket)
	require.NoError(t, err)
	assert.Equal(t, principal, got)

	_, err = store.consumeTicket(ticket)
	assert.ErrorIs(t, err, errTicketConsumed, "ticket 一次一用")

	// 过期与用完给出同一个错误，避免把"票据是否存在过"泄露出去
	expired, err := store.issueTicket(principal)
	require.NoError(t, err)
	*current = current.Add(ticketTTL + time.Second)
	_, err = store.consumeTicket(expired)
	assert.ErrorIs(t, err, errTicketConsumed)

	_, err = store.consumeTicket("")
	assert.ErrorIs(t, err, errTicketConsumed)
}

func TestAuthStoreSweepDropsExpiredEntries(t *testing.T) {
	store, current := newTestStore(t)

	refresh, err := store.putRefresh(Principal{Name: "rooter", Role: core.RoleAdmin}, time.Second)
	require.NoError(t, err)
	ticket, err := store.issueTicket(Principal{Name: "watcher", Role: core.RoleViewer})
	require.NoError(t, err)
	store.revoke("gone", current.Add(time.Second))

	// 时间前进超过清理节拍，下一次写入会触发 sweep
	*current = current.Add(sweepInterval * 2)
	_, err = store.putRefresh(Principal{Name: "rooter", Role: core.RoleAdmin}, time.Hour)
	require.NoError(t, err)

	refreshCount, revokedCount, ticketCount := store.counts()
	assert.Equal(t, 1, refreshCount, "只剩刚写入的那条")
	assert.Equal(t, 0, revokedCount)
	assert.Equal(t, 0, ticketCount)

	assert.Empty(t, store.takeSubject(refresh), "过期 refresh 已不可用")
	_, err = store.consumeTicket(ticket)
	assert.ErrorIs(t, err, errTicketConsumed)
}

func TestLoginLimiterWindowSlides(t *testing.T) {
	current := time.Now()
	limiter := newLoginLimiter()
	limiter.now = func() time.Time { return current }

	for i := 0; i < limiter.pairLimit; i++ {
		ok, _ := limiter.Allow("10.0.0.1", "rooter")
		require.True(t, ok, "阈值内应放行第 %d 次尝试", i)
		limiter.Reject("10.0.0.1", "rooter")
	}

	ok, retryAfter := limiter.Allow("10.0.0.1", "rooter")
	assert.False(t, ok, "达到阈值后应拒绝")
	assert.Greater(t, retryAfter, time.Duration(0))
	assert.LessOrEqual(t, retryAfter, limiter.window)

	current = current.Add(limiter.window + time.Second)
	ok, _ = limiter.Allow("10.0.0.1", "rooter")
	assert.True(t, ok, "窗口过去后应重新放行")
}

// 同一来源爆破多个账号时由 IP 维度兜住；
// 但同一 NAT 下的正常同事不该因为别人失败过而被牵连。
func TestLoginLimiterSeparatesPairAndIPDimensions(t *testing.T) {
	limiter := newLoginLimiter()

	for i := 0; i < limiter.ipLimit; i++ {
		user := "user-" + strconv.Itoa(i)
		ok, _ := limiter.Allow("10.0.0.9", user)
		require.True(t, ok)
		limiter.Reject("10.0.0.9", user)
	}

	ok, retryAfter := limiter.Allow("10.0.0.9", "another-victim")
	assert.False(t, ok, "同一来源广撒网应被 IP 维度拦住")
	assert.Greater(t, retryAfter, time.Duration(0))

	ok, _ = limiter.Allow("10.0.0.10", "another-victim")
	assert.True(t, ok, "限流惩罚的是爆破行为，不是某个出口 IP 上的所有人")
}

func TestLoginLimiterResetClearsOnlyPair(t *testing.T) {
	limiter := newLoginLimiter()

	limiter.Reject("10.0.0.1", "rooter")
	limiter.Reset("10.0.0.1", "rooter")

	keys := throttleKeys("10.0.0.1", "rooter")
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	assert.Empty(t, limiter.attempts[keys[0]], "成功登录应清掉该来源对该账号的失败计数")
	assert.Len(t, limiter.attempts[keys[1]], 1, "IP 维度保留：一次成功登录抹不掉爆破历史")
}
