package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	// PostgreSQL 驱动注册名为 "pgx"。pgx/v5 是 PG 生态的事实标准（纯 Go）：
	// 与 modernc.org/sqlite 一样，全仓只允许本包引入（见 doc.go）。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// defaultPostgresMaxConns 是调用方没有显式给 MaxOpenConns 时 PG 方言的连接池上限。
// SQLite 的单连接是消除 SQLITE_BUSY 的手段，PG 行级锁没有这个问题，读多写少的
// 观测库给一个小池子即可；要放大会由部署方显式配置（本卡不提供配置面）。
const defaultPostgresMaxConns = 4

// openPostgres 按 DSN 打开一个 PostgreSQL 连接池（pgx 驱动 + pgdialect）。
//
// 连接级设置全部交给 DSN 本身与服务端（sslmode、pool 大小、search_path 等）：
// SQLite 的 busy_timeout/WAL/synchronous 在 PG 没有对应物，Engine 把
// JournalMode/Synchronous 记成 "n/a"，装配方启动日志里的这两个读数按方言呈现。
// openPostgres 按 DSN 打开一个 PostgreSQL 连接池（pgx 驱动 + pgdialect）。
//
// 连接级设置全部交给 DSN 本身与服务端（sslmode、pool 大小、search_path 等）：
// SQLite 的 busy_timeout/WAL/synchronous 在 PG 没有对应物，Engine 把
// JournalMode/Synchronous 记成 "n/a"，装配方启动日志里的这两个读数按方言呈现。
func openPostgres(opts Options, logger *slog.Logger) (*DB, error) {
	dsn := strings.TrimSpace(opts.DSN)
	if dsn == "" {
		return nil, errors.New("postgres: dsn must not be empty")
	}
	// 语句超时（G6/Q07）注入 DSN：pgx 把 URL 查询串里它不认识的参数当作
	// 服务端运行时参数逐会话应用，statement_timeout 因此对池里每条连接生效。
	// 迁移路径的豁免不在这里——见 Migrate 里的 SET LOCAL。
	if opts.StatementTimeout > 0 {
		merged, err := withStatementTimeout(dsn, opts.StatementTimeout)
		if err != nil {
			return nil, err
		}
		dsn = merged
	}
	// 未配置只 Warn 不拒绝：超时上限是运维的显式动作，缺省行为必须与
	// 本卡之前一字不差；但"没有上限"这件事要在启动日志里说出来。
	if opts.StatementTimeout <= 0 {
		logger.Warn("postgres database has no statement_timeout; a runaway query can hold a connection indefinitely",
			"hint", "set store.statement_timeout to give every business statement a ceiling")
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	maxConns := opts.MaxOpenConns
	if maxConns <= 0 {
		maxConns = defaultPostgresMaxConns
	}
	sqlDB.SetMaxOpenConns(maxConns)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	db := &DB{
		DB:          bun.NewDB(sqlDB, pgdialect.New()),
		dialect:     DialectPostgres,
		sqlDB:       sqlDB,
		path:        redactDSN(dsn),
		logger:      logger,
		journalMode: "n/a",
		synchronous: "n/a",
	}
	db.logger.Debug("postgres database opened",
		"dsn", db.path,
		"max_open_conns", maxConns)
	return db, nil
}

// dsnPasswordPattern 匹配 keyword/value 形态 DSN 里的口令（host=localhost password=secret）。
var dsnPasswordPattern = regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|"[^"]*"|\S+)`)

// statementTimeoutParam 匹配 DSN 里已存在的 statement_timeout 参数（两种形态）。
// URL 形态经 url.Parse 精确查；keyword/value 形态用词边界正则，避免误伤
// 字段值里碰巧含这个词的取值。
var statementTimeoutParam = regexp.MustCompile(`(?i)(^|[?\s&])statement_timeout\s*=`)

// withStatementTimeout 把 statement_timeout（毫秒）注入 DSN。合并语义（Q07 卡内定）：
// **冲突报错**——同一个上限出现在配置键与 DSN 两处，取哪边都是猜；fail fast 让
// 写重的人当场看到，而不是带着一份自己没意识到的取值上线。
// 两种 DSN 形态都支持（pgx 通吃）：URL 查询串与 keyword/value 追加。
func withStatementTimeout(dsn string, timeout time.Duration) (string, error) {
	ms := timeout.Milliseconds()
	if ms <= 0 {
		// 亚毫秒上限等价于"每条语句都失败"，调用方不该走到这里；直接拒绝。
		return "", fmt.Errorf("postgres: statement_timeout %v is too small, use at least 1ms", timeout)
	}
	param := fmt.Sprintf("statement_timeout=%d", ms)

	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		if u.Query().Get("statement_timeout") != "" {
			return "", fmt.Errorf("postgres: dsn already sets statement_timeout; remove it from the dsn or unset store.statement_timeout")
		}
		q := u.Query()
		q.Set("statement_timeout", fmt.Sprintf("%d", ms))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	if statementTimeoutParam.MatchString(dsn) {
		return "", fmt.Errorf("postgres: dsn already sets statement_timeout; remove it from the dsn or unset store.statement_timeout")
	}
	return dsn + " " + param, nil
}

// redactDSN 把 DSN 里的口令抹掉，供日志与 Path() 呈现。
// pgx 同时接受 URL 形态与 keyword/value 形态，两种都处理。
func redactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.UserPassword(u.User.Username(), "*****")
			}
		}
		return u.String()
	}
	return dsnPasswordPattern.ReplaceAllString(dsn, `${1}*****`)
}
