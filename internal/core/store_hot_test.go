package core

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件的用例全部走公开写路径（Save/Update），不直接调 trimAfterWriteLocked：
// 那个方法的前置条件是调用方持 s.mu，绕过 Update 去调它等于测了一个生产中不存在的形状。

// TestSetHistoryRetention_LimitTakesEffectOnNextWrite 条数上限换掉之后，
// 生效时机是"下一次写入触发的 trim"：调完 setter 不能立刻重排已有留痕。
func TestSetHistoryRetention_LimitTakesEffectOnNextWrite(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: 100})
	base := time.Now()

	// 先写进 5 条终态快照，100 条上限下全部留存
	for i := 0; i < 5; i++ {
		require.NoError(t, store.Update(terminalSnapshot(
			string(rune('a'+i)), StatusSuccess, base.Add(time.Duration(i)*time.Minute))))
	}
	assert.Len(t, mustLoadAll(t, store), 5)

	store.SetHistoryRetention(2, 0)
	assert.Len(t, mustLoadAll(t, store), 5, "retention must not trim without a write")

	require.NoError(t, store.Update(terminalSnapshot("f", StatusSuccess, base.Add(5*time.Minute))))
	assert.Len(t, mustLoadAll(t, store), 2, "the new limit must apply to the next trim")
}

// TestSetHistoryRetention_TTLExpiresOnNextWrite 时长上限同样成对给出：
// 新 TTL 让此前的留痕在下一次写入时被淘汰，刚写的那条留下。
func TestSetHistoryRetention_TTLExpiresOnNextWrite(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: 100})
	old := time.Now().Add(-48 * time.Hour)

	for i := 0; i < 3; i++ {
		require.NoError(t, store.Update(terminalSnapshot(
			string(rune('a'+i)), StatusSuccess, old.Add(time.Duration(i)*time.Minute))))
	}
	store.SetHistoryRetention(100, 24*time.Hour)
	require.NoError(t, store.Update(terminalSnapshot("fresh", StatusSuccess, time.Now())))

	ids := snapshotIDs(mustLoadAll(t, store))
	// 只剩一条的原因是时长而不是条数：上限给的是 100，而三条旧快照比 24h 的新 TTL 老（写在 48h 前）。
	// 判据取"活下来的正是刚写的那条"，只断言数量的话，"把三条旧的留下、把新的淘汰掉"
	// 这种反过来的错也照样过。
	require.Len(t, ids, 1)
	assert.True(t, ids["fresh"])
}

// TestSetHistoryRetention_NegativeLimitKeepsNothing -1 是"不留痕"而不是"不限量"，
// 这一条与构造时 HistoryLimit=-1 的口径同一条（store_history_test.go 里那条对照）。
func TestSetHistoryRetention_NegativeLimitKeepsNothing(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: 100})
	store.SetHistoryRetention(-1, 0)
	require.NoError(t, store.Update(terminalSnapshot("a", StatusSuccess, time.Now())))

	assert.Empty(t, mustLoadAll(t, store), "limit -1 means write-then-delete")
}

// TestSetHistoryRetention_ZeroLimitFallsBackToDefault 0 在 setter 里也走同一条补齐：
// 回到 DefaultHistoryLimit 而不是"不限量"，否则 setter 就成了第二套规则。
// 判据取存进去的那个数：写满 5 条在"默认 100"与"不限量"两种实现下都成立，
// 只有读数能区分这两者（读数断言在同包的 store_history_test.go:89 已有先例）。
func TestSetHistoryRetention_ZeroLimitFallsBackToDefault(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: 2})
	store.SetHistoryRetention(0, 0)
	assert.Equal(t, int64(DefaultHistoryLimit), store.historyLimit.Load(),
		"0 必须回落到 DefaultHistoryLimit，而不是当成不限量存进去")

	base := time.Now()
	for i := 0; i < 5; i++ {
		require.NoError(t, store.Update(terminalSnapshot(
			string(rune('a'+i)), StatusSuccess, base.Add(time.Duration(i)*time.Minute))))
	}
	assert.Len(t, mustLoadAll(t, store), 5, "0 falls back to the default limit, not to no history")
}

// TestSetHistoryRetention_ConcurrentWithTrim 写策略与读策略交错：
// 留痕字段换成原子值就是为了这一种交错——重载协程写、写入协程在同一次 trim 里读。
// 判据取"并发结束之后自己再走一遍成对设置 + 一次写入"：交错期间哪一代策略在生效
// 取决于两个协程谁被调度到，直接断"记录数不超过某个值"会把判据建在调度顺序上
// （setter 若被推迟到写入循环收尾，生效的仍是构造时的 50 条）。
// 所以这里收到 3 条再写一条，让最后一次 trim 必然用 3，剩下的条数就是可断的语义。
func TestSetHistoryRetention_ConcurrentWithTrim(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: 50})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			store.SetHistoryRetention(10+i, time.Duration(i)*time.Hour)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := store.Update(terminalSnapshot(strconv.Itoa(i), StatusSuccess, time.Now())); err != nil {
				t.Errorf("concurrent Update(%d): %v", i, err)
				return
			}
		}
	}()
	wg.Wait()

	store.SetHistoryRetention(3, 0)
	require.NoError(t, store.Update(terminalSnapshot("after", StatusSuccess, time.Now())))
	assert.LessOrEqual(t, len(mustLoadAll(t, store)), 3,
		"末次 trim 必须用得上并发写完之后设定的条数上限")
}
