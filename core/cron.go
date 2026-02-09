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
	// 使用标准解析器，支持秒级精度（可选）
	return CronParser{
		parser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
}

func (c *CronParser) Next(cronExpr string, now time.Time) (time.Time, error) {
	schedule, err := c.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(now), nil
}
