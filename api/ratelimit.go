package api

import (
	"sync"
	"time"
)

// loginLimiter 按"来源 IP + 账号"和"来源 IP"两个维度累计登录失败。
//
// 为什么必须限流：bcrypt 是 CPU 密集的（cost 10 约 60-100ms），
// 不限流的登录端点等于把一个免费的重型 DoS 开关挂在公网上，顺带也挡住了按字典枚举密码的路。
//
// 为什么是两条阈值线：只按 IP+账号 限制挡不住"拿一个密码试上百个账号"的撞库；
// 只按 IP 限制则会让同一个 NAT 后的同事互相拖累，甚至被人用来把整个办公室锁在门外。
// 所以配对账号用低阈值精准锁定，对 IP 用高阈值兜住广撒网。
type loginLimiter struct {
	window time.Duration
	// pairLimit 是"同一来源针对同一账号"的允许失败数
	pairLimit int
	// ipLimit 是"同一来源针对任意账号"的允许失败数，明显宽一些
	ipLimit int
	// now 便于测试注入时间
	now func() time.Time

	mu       sync.Mutex
	attempts map[string][]time.Time
}

// newLoginLimiter 用文档约定的阈值（窗口 1 分钟、同账号 5 次、同来源 20 次）构造限流器。
func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		window:    time.Minute,
		pairLimit: 5,
		ipLimit:   20,
		now:       time.Now,
		attempts:  make(map[string][]time.Time),
	}
}

// throttleKeys 给出本次尝试要记账的两个维度：精准锁"这个来源在爆破这个账号"，
// 兜底锁"这个来源在广撒网"。
func throttleKeys(ip, username string) []string {
	return []string{"ip|" + ip + "|user|" + username, "ip|" + ip}
}

// limitsOf 与 throttleKeys 一一对应：各维度允许的失败次数。
func (l *loginLimiter) limitsOf() []int {
	return []int{l.pairLimit, l.ipLimit}
}

// Allow 判断本次尝试是否放行。只有失败才计数（见 Reject），
// 因此"成功登录"不会消耗额度，历史失败也不会随着正常使用累积。
func (l *loginLimiter) Allow(ip, username string) (ok bool, retryAfter time.Duration) {
	keys := throttleKeys(ip, username)
	limits := l.limitsOf()

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	worst := time.Duration(0)
	for i, key := range keys {
		recent := l.recentLocked(key, now)
		if len(recent) < limits[i] {
			continue
		}
		// 最早一条记录滑出窗口的时刻，就是可以重试的时刻
		wait := recent[0].Add(l.window).Sub(now)
		if wait < 0 {
			wait = 0
		}
		if wait > worst {
			worst = wait
		}
	}
	if worst > 0 {
		return false, worst
	}
	return true, 0
}

// Reject 记录一次失败尝试。
func (l *loginLimiter) Reject(ip, username string) {
	keys := throttleKeys(ip, username)

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	for _, key := range keys {
		kept := l.recentLocked(key, now)
		l.attempts[key] = append(kept, now)
	}
}

// Reset 在登录成功后清掉"该来源 + 该账号"的失败计数，避免正常用户被历史失败拖累。
// IP 维度保留：它统计的是这个来源总共失败过多少次，成功一次登录不能抹掉别的爆破。
func (l *loginLimiter) Reset(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, throttleKeys(ip, username)[0])
}

// recentLocked 返回键在窗口内的失败时间戳（顺便丢掉过期的）。调用者需持锁。
func (l *loginLimiter) recentLocked(key string, now time.Time) []time.Time {
	stamps := l.attempts[key]
	if len(stamps) == 0 {
		return nil
	}
	cutoff := now.Add(-l.window)
	idx := 0
	for idx < len(stamps) && stamps[idx].Before(cutoff) {
		idx++
	}
	if idx > 0 {
		stamps = append(stamps[:0], stamps[idx:]...)
		l.attempts[key] = stamps
	}
	if len(stamps) == 0 {
		delete(l.attempts, key)
	}
	return stamps
}
