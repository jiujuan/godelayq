package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"godelayq/core"

	// SQLite 驱动注册名为 "sqlite"。modernc.org/sqlite 是纯 Go 实现（无 CGO）：
	// 设计文档 D2 定了这条，本包是全仓唯一引入它的地方（见 doc.go）。
	_ "modernc.org/sqlite"
)

// 库文件与父目录的权限，与产物文件同一取向（executor/artifact.go 的 artifactDirPerm / artifactFilePerm）：
// 观测表里会出现账号名与拒绝原因，不放给同机其它用户读。
const (
	dirPerm  os.FileMode = 0o750
	filePerm os.FileMode = 0o640
)

// Stats 是观测库的自我描述：三张表的行数与已应用的迁移版本。
// 各写入器的"丢了多少"长在写入器自己身上（batcher.Dropped），DB 拿不到，
// 由 S03/S06 的 EventLog、AuditLog 一起上报给运维端点。
type Stats struct {
	Events        int64
	Artifacts     int64
	AuditRows     int64
	SchemaVersion int
}

// DB 是观测库的句柄，内部只有一个连接：写入是批量单点、读是短查询，
// 一个连接足够，同时消除 SQLITE_BUSY 的主要来源（设计文档 §7.2）。
type DB struct {
	sqlDB  *sql.DB
	path   string
	logger *slog.Logger

	// journalMode 与 synchronous 记的是实际生效的 PRAGMA，不是配置里的期望值：
	// 网络文件系统上 WAL 会静默退回 delete，现场判断要看的是前者（本卡 §9 风险 2）。
	journalMode string
	synchronous string
	closeOnce   sync.Once
	closeErr    error
}

// Open 建好父目录、打开连接、设置 PRAGMA、建表并应用迁移。
//
// 建不起库等于什么都记不住，所以任何一步失败都返回错误，由装配方决定是否终止启动
// ——cmd/server 的口径是不带"记不住日志"的状态上线（设计文档 §11）。
// cfg 正常应由 core.Config.Normalized() 补齐过；这里仍对空路径与非法 synchronous 兜底报错，
// 因为本包也会被装配方之外的调用方直接使用。
func Open(cfg core.ObservabilityConfig, logger *slog.Logger) (*DB, error) {
	if logger == nil {
		logger = slog.Default()
	}

	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, errors.New("sqlite: observability.path must not be empty")
	}
	// 路径要拼进 DSN，而 '?' 与 '#' 在 DSN 里是查询串与片段的分隔符：
	// 含这两个字符的路径会被解析成"文件名 + 一串 PRAGMA"，与其得出一条看不懂的启动失败，
	// 不如在这里直接说清路径不合法。Windows 本来也不允许文件名带 '?'。
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("sqlite: observability.path %q must not contain '?' or '#'", path)
	}
	synchronous := strings.TrimSpace(cfg.Synchronous)
	switch synchronous {
	case "", "normal", "full":
	default:
		return nil, fmt.Errorf("sqlite: observability.synchronous %q is invalid, use normal or full", cfg.Synchronous)
	}
	if synchronous == "" {
		synchronous = core.DefaultObserveSynchronous
	}

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("sqlite: create dir for %s: %w", path, err)
	}
	// 先按目标权限把文件建出来，再让驱动打开：驱动自己创建时权限由系统 umask 决定，
	// 同机其它用户可能正好能读这份带账号名的台账。已存在的文件不动它的权限。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return nil, fmt.Errorf("sqlite: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: create database file: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", dsn(path, cfg, synchronous))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// 写死一个连接：见类型注释。要放开时另议（本卡 §8 明确不在范围内）。
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sqlDB: sqlDB, path: path, logger: logger}
	if err := db.checkConnectionSettings(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := db.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// dsn 把 PRAGMA 写进连接串，而不是打开后执行一次：
// busy_timeout、synchronous、foreign_keys 都是每条连接的设置，
// 连接因故重建后只有 DSN 里这份还会生效。
func dsn(path string, cfg core.ObservabilityConfig, synchronous string) string {
	timeout := cfg.BusyTimeout
	if timeout < 0 {
		timeout = 0
	}
	return fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(%s)&_pragma=foreign_keys(1)",
		path, timeout.Milliseconds(), strings.ToUpper(synchronous))
}

// checkConnectionSettings 读回实际生效的 PRAGMA。
//
// 期望值没生效时只记日志、不报错：journal_mode 在网络文件系统上会静默退回 delete，
// 那是要部署方判断的环境问题，让观测层带着降级继续跑比拒绝启动更有用。
// 生效值留在句柄上，装配方启动日志与现场排障都读它。
func (d *DB) checkConnectionSettings() error {
	if err := d.sqlDB.Ping(); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	if err := d.sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&d.journalMode); err != nil {
		return fmt.Errorf("sqlite: read journal_mode: %w", err)
	}
	// synchronous 的返回值是数字：0=OFF、1=NORMAL、2=FULL
	var level int
	if err := d.sqlDB.QueryRow(`PRAGMA synchronous`).Scan(&level); err != nil {
		return fmt.Errorf("sqlite: read synchronous: %w", err)
	}
	switch level {
	case 0:
		d.synchronous = "off"
	case 1:
		d.synchronous = "normal"
	case 2, 3:
		// 3 = EXTRA（SQLite 3.8 之后 FULL 的实现值，WAL 下与 FULL 同义）
		d.synchronous = "full"
	default:
		d.synchronous = fmt.Sprintf("unknown(%d)", level)
	}

	d.logger.Debug("observability database opened",
		"path", d.path,
		"journal_mode", d.journalMode,
		"synchronous", d.synchronous)
	if !strings.EqualFold(d.journalMode, "wal") {
		d.logger.Warn("observability database is not in WAL mode",
			"path", d.path,
			"journal_mode", d.journalMode,
			"hint", "this filesystem does not support WAL (network mount?), read and write no longer overlap")
	}
	return nil
}

// migrate 按版本号顺序执行内置语句：先建迁移版本表，再只跑比库里已应用版本更高的那些，
// 每个版本一个事务，跑成功后落一行版本记录。中途失败时版本号不会落下，下次启动重跑同一个版本。
func (d *DB) migrate() error {
	if _, err := d.sqlDB.Exec(migrationTable); err != nil {
		return fmt.Errorf("sqlite: create migration table: %w", err)
	}

	var applied int
	if err := d.sqlDB.QueryRow(currentSchemaVersion).Scan(&applied); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= applied {
			continue
		}
		tx, err := d.sqlDB.Begin()
		if err != nil {
			return fmt.Errorf("sqlite: begin migration %d: %w", m.version, err)
		}
		for _, stmt := range m.up {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("sqlite: apply migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO observe_schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// Path 返回本句柄打开的库文件路径，照配置里的写法返回，不做绝对化。
func (d *DB) Path() string {
	return d.path
}

// JournalMode 返回实际生效的日志模式，供装配方把降级情况写进启动日志。
func (d *DB) JournalMode() string {
	return d.journalMode
}

// Synchronous 返回实际生效的 synchronous 取值（normal|full|off），
// 与配置里的期望值对照才能看出"这一份部署其实没有那条保证"。
func (d *DB) Synchronous() string {
	return d.synchronous
}

// Stats 查三张表的行数与迁移版本。任一查询失败都返回错误而不是 0：
// 把"读不出来"报成"表是空的"，等于让运维以为观测层正在正常工作。
func (d *DB) Stats() (Stats, error) {
	var stats Stats
	if err := d.sqlDB.QueryRow(currentSchemaVersion).Scan(&stats.SchemaVersion); err != nil {
		return stats, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	counts := []struct {
		query string
		dst   *int64
	}{
		{`SELECT COUNT(*) FROM job_events`, &stats.Events},
		{`SELECT COUNT(*) FROM artifact_index`, &stats.Artifacts},
		{`SELECT COUNT(*) FROM write_audit`, &stats.AuditRows},
	}
	for _, item := range counts {
		if err := d.sqlDB.QueryRow(item.query).Scan(item.dst); err != nil {
			return stats, fmt.Errorf("sqlite: count rows: %w", err)
		}
	}
	return stats, nil
}

// Close 关闭连接。幂等：重复调用返回第一次的结果，不会二次关闭。
//
// 顺序由装配方的 defer 保证（设计文档 §7.3）：先撤订阅读者、再关各写入器、最后关本句柄；
// 颠倒过来就会在已关闭的连接上写入。
func (d *DB) Close() error {
	d.closeOnce.Do(func() { d.closeErr = d.sqlDB.Close() })
	return d.closeErr
}
