package core

import (
	"time"

	"github.com/robfig/cron/v3"
)

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

func (c *CronParser) Next(cronExpr string, now time.Time) (time.Time, error) {
	schedule, err := c.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(now), nil
}
