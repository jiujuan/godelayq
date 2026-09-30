package sqlite

import (
	"errors"
	"sync"
	"time"

	"godelayq/core"
)

// ErrBatcherClosed 表示写入器已经关闭：之后的 append 返回 false、Flush 返回本错误。
// 关闭之后还在写属于装配顺序错误——撤订阅读者必须早于关连接（设计文档 §7.3）。
var ErrBatcherClosed = errors.New("sqlite: writer is closed")

// errNilFlush 是构造参数错误：没有落盘函数的批量写入器一条也写不出去。
// 这里不返回一个"安静吞掉所有数据"的实例，那正是装配最容易被看漏的失败形态。
var errNilFlush = errors.New("sqlite: batcher requires a flush function")

// batchSlot 是一条待写入记录加上它的重试标记。
// 失败一次就打标记，下一轮再失败即丢弃并计数——这就是"不无限重试"的落点（设计文档 §7.1）。
type batchSlot[T any] struct {
	item    T
	retried bool
}

// batcher 是三张表共用的批量写入骨架：有界队列 + 周期事务落盘。S03/S06 各持一个实例。
//
// 三条硬要求：
//   - append 不阻塞调用方：队满即丢弃并累计计数，绝不把背压传回事件总线或 HTTP 请求路径（D4）。
//   - 一次事务提交整批；失败的那批留在队首重试一次，二次失败则丢弃并记 error。
//   - Close 幂等：先停周期落盘、把剩余条目写完，再封口；Close 之后 append 返回 false、
//     Flush 返回错误而不是 panic。
//
// 用泛型而不是 []any：每张表的行类型在编译期就固定，省掉一层运行期断言，
// 也排除"把审计行喂进事件表"这类错接法。
type batcher[T any] struct {
	capacity int
	interval time.Duration
	flush    func([]T) error
	onError  func(error)

	// mu 只保护队列、计数与状态位；落盘函数在锁外调用，
	// 否则一次慢事务会把 append 的调用方（事件总线订阅协程）拖住。
	mu      sync.Mutex
	queue   []batchSlot[T]
	dropped int64
	closed  bool
	closing bool

	// writeMu 把整轮落盘串行化：ticker 那一轮与关停路径的显式 Flush 可能同时进来，
	// 而两张快照读到的是同一批队首条目——不加这把锁，同一批记录会被写进表两次。
	writeMu sync.Mutex

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	// closeErr 是收尾那一轮落盘的错误（若有），由后台协程在 done 关闭前写好。
	closeErr error
}

// newBatcher 建一个写入器并起后台落盘协程。
//
// capacity 与 interval 的非正取值一律回到 core 的观测层默认值：队列容量为 0 的实际含义是
// "一条都不留"，而调用方传 0 更可能是没配（配置校验已经拦住了显式写 0 的部署）。
// onError 传 nil 表示不额外记录，只累计丢弃数。
func newBatcher[T any](capacity int, interval time.Duration, flush func([]T) error, onError func(error)) (*batcher[T], error) {
	if flush == nil {
		return nil, errNilFlush
	}
	// 三条补齐各自独立判断（写成 switch 会只命中第一条，非正的容量就会把零周期漏过去，
	// 而 time.NewTicker(0) 是直接 panic）
	if capacity <= 0 {
		capacity = core.DefaultObserveQueueCapacity
	}
	if interval <= 0 {
		interval = core.DefaultObserveFlushInterval
	}
	if onError == nil {
		onError = func(error) {}
	}

	b := &batcher[T]{
		capacity: capacity,
		interval: interval,
		flush:    flush,
		onError:  onError,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go b.run()
	return b, nil
}

// append 入队一条。返回 false 表示这条被丢弃（队满或已关闭），调用方不需要处理错误——
// 丢一条观测记录不该变成阻塞调度，代价由 Dropped() 暴露出来。
func (b *batcher[T]) append(item T) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.closing || len(b.queue) >= b.capacity {
		b.dropped++
		return false
	}
	b.queue = append(b.queue, batchSlot[T]{item: item})
	return true
}

// queued 返回还在队列里、尚未落盘的条数。
// 它只在本包内可见，用途是把测试里的"睡一会儿再看结果"换成"等到条数对上"；
// 将来要把队列占用透出到运维端点时，读的是同一个数。
func (b *batcher[T]) queued() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queue)
}

// Dropped 返回累计丢弃条数：队满、关闭后写入、以及二次失败被放弃的批次都计在这里。
// 这个数必须可查，否则一张看起来完整的表实际缺页，运维无从判断（设计文档 §7.1）。
func (b *batcher[T]) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// run 是后台落盘循环：每个周期把当时攒下的整批写进去。
// 失败的上报由 Flush 自己完成（它知道这一批是被放弃还是留给重试），这里只管收尾。
func (b *batcher[T]) run() {
	defer close(b.done)

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = b.Flush()
		case <-b.stop:
			// 收尾一轮：写不进去的那批由封口路径计入丢弃，错误从 Close 的返回值透出，
			// 装配方据此记日志——关停时静默丢数据比启动时报错更难被发现。
			if err := b.Flush(); err != nil && !errors.Is(err, ErrBatcherClosed) {
				b.mu.Lock()
				b.closeErr = err
				b.mu.Unlock()
			}
			return
		}
	}
}

// Flush 立即落盘当前批次，空批次不产生事务。供关停路径与测试使用。
//
// 返回值是本轮落盘的失败（重试也失败时），调用方据此记日志；被放弃的条目已计入 Dropped，
// 并且只有"真的丢了数据"这一轮才会走 onError。
// 已经关闭时返回 ErrBatcherClosed 而不是 panic：关闭顺序写错要能被看见。
func (b *batcher[T]) Flush() error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBatcherClosed
	}
	if len(b.queue) == 0 {
		b.mu.Unlock()
		return nil
	}
	batch := make([]T, len(b.queue))
	for i := range b.queue {
		batch[i] = b.queue[i].item
	}
	b.mu.Unlock()

	err := b.flush(batch)
	if err == nil {
		b.mu.Lock()
		b.takeCommitted(len(batch))
		b.mu.Unlock()
		return nil
	}

	b.mu.Lock()
	givenUp := b.recordFailure(len(batch))
	b.mu.Unlock()
	if givenUp > 0 {
		// 只有被放弃的那一轮才记 error：第一次失败只是留给重试，数据还没丢
		b.onError(err)
	}
	return err
}

// takeCommitted 从队首摘掉已经落库的那一批。在跑期间新 append 的都排在它后面，顺序不会乱。
//
// 队列可能比这一批短：Close 封口时会把还没写出去的条目一次清掉，那时这里不该再切。
func (b *batcher[T]) takeCommitted(n int) {
	if n <= len(b.queue) {
		b.queue = b.queue[n:]
	} else {
		b.queue = nil
	}
}

// recordFailure 处理一轮落盘失败，返回这一轮被放弃的条数。
//
// 没重试过的打上标记留在队首，等下一轮重试；已经重试过一次的放弃并计数。
// 留在队首而不是退回队列尾部，是为了保住事件的时间顺序——重试链里同一任务的几条事件
// 一旦打乱，读出来就是一份错乱的时间线。
func (b *batcher[T]) recordFailure(n int) int {
	kept := make([]batchSlot[T], 0, len(b.queue))
	givenUp := 0
	for i := 0; i < len(b.queue); i++ {
		slot := b.queue[i]
		if i < n {
			// 这一批刚失败过：重试过一次的放弃，其余打上标记留待下一轮
			if slot.retried {
				b.dropped++
				givenUp++
				continue
			}
			slot.retried = true
		}
		kept = append(kept, slot)
	}
	b.queue = kept
	return givenUp
}

// Close 停止周期落盘、把剩余条目写完，然后封口。幂等：重复调用不重复落盘，返回同一个结果。
//
// Close 正常返回时，队列里能写的都已落库；写不进去的（含二次失败的与封口时仍留在队列里的）
// 都已计入 Dropped 并通过 onError 记过一次。
func (b *batcher[T]) Close() error {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		close(b.stop)
		<-b.done

		b.mu.Lock()
		b.closed = true
		// 封口后仍留在队列里的条目不会再有事务来写它们：按丢弃计，别装作已经落库。
		b.dropped += int64(len(b.queue))
		b.queue = nil
		b.mu.Unlock()
	})
	return b.closeErr
}
