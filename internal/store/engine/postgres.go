package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

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
func openPostgres(opts Options, logger *slog.Logger) (*DB, error) {
	dsn := strings.TrimSpace(opts.DSN)
	if dsn == "" {
		return nil, errors.New("postgres: dsn must not be empty")
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
