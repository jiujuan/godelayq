package sqlite

import (
	"errors"
	"sync"
	"testing"
	"time"

	"godelayq/core"
)

// flushRecorder 是一个可控的落盘函数：记下每一批的内容，按脚本决定失败与否。
// 用例一律靠通道与显式 Flush 推进，不用 sleep 猜时间（本卡 §9 风险 3）。
type flushRecorder struct {
	mu       sync.Mutex
	batches  [][]int64
	callErrs []error
	calls    int
	notify   chan []int64 // 非空时，每次落盘成功后把批次内容发出去（供通道等待）
}

func newRecorder(failTimes int) *flushRecorder {
	r := &flushRecorder{}
	for i := 0; i < failTimes; i++ {
		r.callErrs = append(r.callErrs, errors.New("simulated write failure"))
	}
	return r
}

func (r *flushRecorder) flush(batch []int64) error {
	r.mu.Lock()
	r.calls++
	err := r.popError()
	if err == nil {
		r.batches = append(r.batches, batch)
	}
	notify := r.notify
	r.mu.Unlock()

	if err == nil && notify != nil {
		select {
		case notify <- batch:
		default:
			// 没人等结果：通知只是可选旁路，不该因为它塞住落盘
		}
	}
	return err
}

// popError 取出这次调用的预设错误（用完后返回 nil，表示开始成功）。必须在锁内调用。
func (r *flushRecorder) popError() error {
	if len(r.callErrs) > 0 {
		err := r.callErrs[0]
		r.callErrs = r.callErrs[1:]
		return err
	}
	return nil
}

// recorded 返回已落盘的批次快照。
func (r *flushRecorder) recorded() [][]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]int64, len(r.batches))
	copy(out, r.batches)
	return out
}

func (r *flushRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newTestBatcher 建一个不会自己到期的写入器（周期取一小时），由用例显式 Flush 驱动。
func newTestBatcher(t *testing.T, capacity int, r *flushRecorder, onError func(error)) *batcher[int64] {
	t.Helper()

	b, err := newBatcher[int64](capacity, time.Hour, r.flush, onError)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestBatcher_AppendsWithinCapacity(t *testing.T) {
	r := newRecorder(0)
	b := newTestBatcher(t, 4, r, nil)

	for i := int64(0); i < 4; i++ {
		if !b.append(i) {
			t.Fatalf("append %d should succeed within capacity", i)
		}
	}
	if got := b.Dropped(); got != 0 {
		t.Fatalf("expected no drops, got %d", got)
	}
}

func TestBatcher_DropsWhenFull(t *testing.T) {
	r := newRecorder(0)
	b := newTestBatcher(t, 2, r, nil)

	// 队满时 append 必须立刻返回 false，而不是等出队腾位置。
	// 断言方式是"在超时之前必须收到完成信号"：调用方一旦阻塞，用例直接失败。
	// 这条是设计文档 D4 的唯一硬证据——观测记录丢一条不能变成阻塞调度。
	const total = 10
	type result struct {
		accepted int
	}
	done := make(chan result, 1)
	go func() {
		accepted := 0
		for i := int64(0); i < total; i++ {
			if b.append(i) {
				accepted++
			}
		}
		done <- result{accepted: accepted}
	}()

	select {
	case got := <-done:
		if got.accepted != 2 {
			t.Fatalf("expected exactly the capacity to be accepted, got %d", got.accepted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("append blocked the caller when the queue was full")
	}

	if got := b.Dropped(); got != int64(total-2) {
		t.Fatalf("expected %d dropped, got %d", total-2, got)
	}
}

func TestBatcher_FlushesInOneTransaction(t *testing.T) {
	r := newRecorder(0)
	b := newTestBatcher(t, 10, r, nil)

	for _, item := range []int64{1, 2, 3} {
		b.append(item)
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	got := r.recorded()
	if len(got) != 1 {
		t.Fatalf("expected one batch, got %d: %v", len(got), got)
	}
	// 整批一次调用，且顺序与入队一致（时间线的因果顺序靠它）
	if len(got[0]) != 3 || got[0][0] != 1 || got[0][1] != 2 || got[0][2] != 3 {
		t.Fatalf("expected the whole batch in order, got %v", got[0])
	}

	// 空批次不产生事务：再 Flush 一次不该多出调用
	if err := b.Flush(); err != nil {
		t.Fatalf("second Flush failed: %v", err)
	}
	if r.callCount() != 1 {
		t.Fatalf("expected an empty queue to skip the transaction, calls=%d", r.callCount())
	}
}

func TestBatcher_RetryOnceThenGiveUp(t *testing.T) {
	t.Run("first attempt fails second succeeds", func(t *testing.T) {
		r := newRecorder(1) // 只有第一次调用失败
		var failures int
		b := newTestBatcher(t, 10, r, func(error) { failures++ })

		for _, item := range []int64{7, 8, 9} {
			b.append(item)
		}
		if err := b.Flush(); err == nil {
			t.Fatal("expected the first Flush to report the failure")
		}
		if got := b.Dropped(); got != 0 {
			t.Fatalf("a first failure must not drop anything, got %d", got)
		}
		if err := b.Flush(); err != nil {
			t.Fatalf("expected the retry to succeed, got %v", err)
		}

		got := r.recorded()
		if len(got) != 1 || len(got[0]) != 3 {
			t.Fatalf("expected the whole batch to land on the retry, got %v", got)
		}
		if b.Dropped() != 0 {
			t.Fatalf("expected no drops after a successful retry, got %d", b.Dropped())
		}
		if failures != 0 {
			t.Fatalf("a successful retry must not be reported as an error, got %d reports", failures)
		}
	})

	t.Run("both attempts fail", func(t *testing.T) {
		r := newRecorder(10) // 每次都失败
		var failures int
		b := newTestBatcher(t, 10, r, func(error) { failures++ })

		for _, item := range []int64{1, 2} {
			b.append(item)
		}
		if err := b.Flush(); err == nil {
			t.Fatal("expected the first Flush to fail")
		}
		if got := b.Dropped(); got != 0 {
			t.Fatalf("the first failure keeps the batch, drops=%d", got)
		}
		if err := b.Flush(); err == nil {
			t.Fatal("expected the retry to fail as well")
		}
		// 二次失败：整批按丢弃收口，并记一次 error，不再无限重试
		if got := b.Dropped(); got != 2 {
			t.Fatalf("expected both records counted as dropped, got %d", got)
		}
		if failures != 1 {
			t.Fatalf("expected one error report (only the round that lost data), got %d", failures)
		}
		if len(r.recorded()) != 0 {
			t.Fatalf("nothing should have landed, got %v", r.recorded())
		}

		// 放弃之后队列已空：第三次 Flush 不该再产生事务
		if err := b.Flush(); err != nil {
			t.Fatalf("Flush on an empty queue should be a no-op, got %v", err)
		}
		if r.callCount() != 2 {
			t.Fatalf("expected exactly two flush attempts, got %d", r.callCount())
		}
	})
}

func TestBatcher_CloseFlushesRemaining(t *testing.T) {
	r := newRecorder(0)
	b, err := newBatcher[int64](10, time.Hour, r.flush, nil)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}

	for _, item := range []int64{4, 5} {
		if !b.append(item) {
			t.Fatal("append should succeed before Close")
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got := r.recorded()
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("expected Close to flush the remaining records, got %v", got)
	}

	// 幂等：重复 Close 不再落盘、不报错
	for i := 0; i < 2; i++ {
		if err := b.Close(); err != nil {
			t.Fatalf("Close #%d failed: %v", i+2, err)
		}
	}
	if r.callCount() != 1 {
		t.Fatalf("expected the flush to run once, got %d", r.callCount())
	}

	// Close 之后再写：append 返回 false，Flush 返回错误而不是 panic
	if b.append(6) {
		t.Error("append after Close must report the drop")
	}
	if err := b.Flush(); !errors.Is(err, ErrBatcherClosed) {
		t.Errorf("expected ErrBatcherClosed, got %v", err)
	}
	if got := b.Dropped(); got != 1 {
		t.Errorf("the write after Close must be counted, got %d", got)
	}
}

// TestBatcher_CloseWithFailingFlush 固定关停路径上的最坏情况：
// 收尾那轮落盘失败时，剩余条目按丢弃计数、错误从 Close 返回，
// 而不是安静地留在一个再也没人读的队列里。
func TestBatcher_CloseWithFailingFlush(t *testing.T) {
	r := newRecorder(10)
	b, err := newBatcher[int64](10, time.Hour, r.flush, nil)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}
	b.append(1)
	b.append(2)

	if err := b.Close(); err == nil {
		t.Fatal("expected Close to report the failed flush")
	}
	if got := b.Dropped(); got != 2 {
		t.Fatalf("expected the two unwritten records counted, got %d", got)
	}
	// Close 之后队列封口：不再重复计一次丢弃
	if err := b.Close(); err == nil {
		t.Fatal("expected the repeated Close to return the same failure")
	}
	if got := b.Dropped(); got != 2 {
		t.Fatalf("expected the drop count to stay, got %d", got)
	}
}

// TestBatcher_TickerFlushes 验证后台周期真的会落盘：用例等的是落盘回调发出的信号，
// 不是"睡一会儿再看结果"。
func TestBatcher_TickerFlushes(t *testing.T) {
	r := newRecorder(0)
	notify := make(chan []int64, 1)
	r.notify = notify

	b, err := newBatcher[int64](10, 10*time.Millisecond, r.flush, nil)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}
	defer func() { _ = b.Close() }()

	if !b.append(42) {
		t.Fatal("append should succeed")
	}
	select {
	case batch := <-notify:
		if len(batch) != 1 || batch[0] != 42 {
			t.Fatalf("expected the ticker to flush the queued record, got %v", batch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker never flushed the queued record")
	}
}

func TestBatcher_ConstructorGuards(t *testing.T) {
	r := newRecorder(0)
	if _, err := newBatcher[int64](4, time.Minute, nil, nil); !errors.Is(err, errNilFlush) {
		t.Fatalf("expected a missing flush function to be rejected, got %v", err)
	}

	// 非正的容量与周期回到 core 的默认值：0 容量的实际含义是"一条都不留"，
	// 而调用方传 0 更可能是没配（配置校验只在 YAML 那条路上拦显式写 0）。
	b, err := newBatcher[int64](0, 0, r.flush, nil)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.capacity != core.DefaultObserveQueueCapacity {
		t.Errorf("expected capacity %d, got %d", core.DefaultObserveQueueCapacity, b.capacity)
	}
	if b.interval != core.DefaultObserveFlushInterval {
		t.Errorf("expected interval %v, got %v", core.DefaultObserveFlushInterval, b.interval)
	}
}

// TestBatcher_AppendIsConcurrencySafe 在 -race 下跑：多个订阅协程同时写一个写入器，
// 再叠加周期落盘与关停，这是观测层在生产里的真实形态。
// gateRecorder 是一个可以卡住的落盘函数：每轮进入时发一个信号，然后等用例放行。
// 用它把"多个 Flush 同时读到同一段队首快照"这个窗口撑开，否则落盘太快、竞争观察不到。
type gateRecorder struct {
	mu      sync.Mutex
	batches [][]int64
	entered chan struct{} // 容量足够大：不会被收到，也不能塞住落盘
	release chan struct{} // 由用例 close，一次放行所有在跑的轮次
}

func newGate() *gateRecorder {
	return &gateRecorder{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (g *gateRecorder) flush(batch []int64) error {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release

	g.mu.Lock()
	g.batches = append(g.batches, batch)
	g.mu.Unlock()
	return nil
}

func (g *gateRecorder) recorded() [][]int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([][]int64, len(g.batches))
	copy(out, g.batches)
	return out
}

// TestBatcher_ConcurrentFlushDoesNotDuplicate 钉住落盘轮次必须串行：
// 周期落盘与关停路径的显式 Flush 可能同时进来，而两张快照读到的是同一段队首。
// 少一把串行锁的后果不是报错，是同一条记录在表里出现两次——事件表里就是一条重复的时间线。
func TestBatcher_ConcurrentFlushDoesNotDuplicate(t *testing.T) {
	const total = 10
	gate := newGate()
	b, err := newBatcher[int64](total*4, time.Hour, gate.flush, nil)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}
	for i := int64(0); i < total; i++ {
		if !b.append(i) {
			t.Fatalf("append %d should fit within the capacity", i)
		}
	}

	const rounds = 4
	var wg sync.WaitGroup
	for w := 0; w < rounds; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Flush()
		}()
	}

	// 至少等到第一轮已经进到落盘函数里（此时队首还没被摘掉），再放其余轮次进去。
	// 收不满 rounds 个是预期的：串行锁下只有一个轮次在跑，其余都排在锁上。
	<-gate.entered
	entered := 1
	deadline := time.After(200 * time.Millisecond)
collect:
	for entered < rounds {
		select {
		case <-gate.entered:
			entered++
		case <-deadline:
			break collect
		}
	}
	close(gate.release)
	wg.Wait()
	if err := b.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	seen := map[int64]int{}
	landed := 0
	for _, batch := range gate.recorded() {
		landed += len(batch)
		for _, item := range batch {
			seen[item]++
		}
	}
	for i := int64(0); i < total; i++ {
		if seen[i] != 1 {
			t.Fatalf("record %d landed %d times across %d flush rounds (total landed %d), expected exactly once",
				i, seen[i], entered, landed)
		}
	}
	if got := b.Dropped(); got != 0 {
		t.Fatalf("a serialized flush round must not drop anything, got %d", got)
	}
}

func TestBatcher_AppendIsConcurrencySafe(t *testing.T) {
	r := newRecorder(0)
	b, err := newBatcher[int64](1024, 200*time.Microsecond, r.flush, nil)
	if err != nil {
		t.Fatalf("newBatcher failed: %v", err)
	}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int64(0); i < 200; i++ {
				b.append(i)
			}
		}()
	}
	wg.Wait()

	if err := b.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	// 落库的加上丢弃的，必须正好等于投进去的条数：一条都不能凭空消失
	var landed int64
	for _, batch := range r.recorded() {
		landed += int64(len(batch))
	}
	if got := landed + b.Dropped(); got != 800 {
		t.Fatalf("expected 800 records accounted for (landed %d + dropped %d), got %d",
			landed, b.Dropped(), got)
	}
}
