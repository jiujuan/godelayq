package core

import (
	"time"

	"github.com/robfig/cron/v3"
)

// maxMisfireWindowScan 是 misfire 窗口计数的迭代封顶（TASK-RE06）：病态表达式
//（秒级高频 + 数年前的起点）不能把恢复路径拖死。达到封顶即截断，
// 调用方在事件 metadata 里留 truncated 标记。
const maxMisfireWindowScan = 1000

// WithMisfirePolicy 注入 cron 错过窗口的全局默认策略与宽限期（TASK-RE06）。
// 任务级 MisfirePolicy 字段优先于全局默认；policy 非 skip 一律按 fire_once
//（现状语义兜底）；grace <= 0 用 DefaultMisfireGracePeriod。
func WithMisfirePolicy(policy string, grace time.Duration) Option {
	return func(o *componentOptions) {
		if policy == "skip" {
			o.misfirePolicy = "skip"
		} else {
			o.misfirePolicy = DefaultMisfirePolicy
		}
		if grace > 0 {
			o.misfireGrace = grace
		} else {
			o.misfireGrace = DefaultMisfireGracePeriod
		}
	}
}

// CronParser Cron解析器包装
type CronParser struct {
	parser cron.Parser
}

func NewCronParser() CronParser {
	// SecondOptional 同时接受两种写法：
	// 5 字段（分 时 日 月 周，历史写法）与 6 字段（秒 分 时 日 月 周，秒级精度）。
	return CronParser{
		parser: cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
}

// Next 返回 cron 表达式在 now 之后的下一次触发时刻。
//
// loc 是展开用的时区（TASK-RE06）：robfig 的 schedule 按入参时刻的时区展开，
// 传任务声明的 IANA 时区即可让"每天 09:00"按那个时区落点。nil 表示不做换算——
// 按入参时刻自带的时区展开（与既有行为逐字节等价：time.Now() 自带服务器本地）。
func (c *CronParser) Next(cronExpr string, now time.Time, loc *time.Location) (time.Time, error) {
	schedule, err := c.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = now.Location()
	}
	return schedule.Next(now.In(loc)), nil
}

// CountMissedWindows 统计 cron 表达式在 (from, to] 之间错过的触发窗口数
//（从 from 的下一次触发起算，到 to 为止）。第二个返回值为 true 表示达到
// 迭代封顶提前截断，计数偏小——调用方在事件里留标记，不假装数全了。
// 表达式解析失败返回 (0, false)：调用方各自有解析报错路径，这里不重复定性。
func (c *CronParser) CountMissedWindows(cronExpr string, from, to time.Time) (int, bool) {
	schedule, err := c.parser.Parse(cronExpr)
	if err != nil {
		return 0, false
	}
	count := 0
	cursor := from
	for count < maxMisfireWindowScan {
		next := schedule.Next(cursor)
		if next.IsZero() || next.After(to) {
			return count, false
		}
		count++
		cursor = next
	}
	return count, true
}
