package engine

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

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	// SQLite 驱动注册名为 "sqlite"。modernc.org/sqlite 是纯 Go 实现（无 CGO）：
	// 设计文档 D2 定了这条，本包是全仓唯一引入它的地方（见 doc.go）。
	_ "modernc.org/sqlite"
)

// 库文件与父目录的权限，与产物文件同一取向（executor/artifact.go 的 artifactDirPerm / artifactFilePerm）：
// 库里会出现账号名、任务标识与拒绝原因，不放给同机其它用户读。
const (
	dirPerm  os.FileMode = 0o750
	filePerm os.FileMode = 0o640
)

// DefaultSynchronous 是调用方没给 synchronous 时的 PRAGMA 取值，与
// core.DefaultObserveSynchronous 同值。默认值的唯一来源仍是 core 的配置层；
// 这里放一份同值常量，是为了直接使用本包、不经配置层的调用方（测试、将来的
// mgr 包）不必反过来 import core——本包对仓内业务包零依赖（见 doc.go）。
const DefaultSynchronous = "normal"

// Dialect 是底座支持的数据库方言。SQLite 是默认值与既事后端；PostgreSQL 自 B05 起
// 可用。方言只影响三件事：打开路径（DSN 与连接级设置）、迁移 DDL 的取语句分支、
// 以及 JournalMode/Synchronous 这类 SQLite 专属诊断的呈现。
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// Options 是打开一个数据库的全部机制性选项。
// 它刻意不绑定任何一节的配置结构（core.ObservabilityConfig、core.MgrStoreConfig
// 各自映射到这里），映射发生在各业务包自己的 Open 里。
type Options struct {
	// Dialect 方言。空值按 sqlite 处理。
	Dialect Dialect
	// Path 库文件路径，仅 sqlite 方言使用。空值与含 '?'/'#' 的取值都会被拒绝。
	Path string
	// DSN 连接串，仅 postgres 方言使用（B05 生效）。
	DSN string
	// BusyTimeout busy 等待上限，仅 sqlite 方言生效；负值按 0 处理。
	BusyTimeout time.Duration
	// Synchronous synchronous PRAGMA：normal|full；空用 DefaultSynchronous，其余报错。
	// 仅 sqlite 方言生效。
	Synchronous string
	// MaxOpenConns 最大连接数；<=0 按 1 处理（PG 方言自 B05 起另行折默认）。
	// 观测库传 1：写入批量单点、读是短查询，单连接同时消除 SQLITE_BUSY 的主要来源
	// （设计文档 §7.2）。
	MaxOpenConns int
}

// DB 是一个数据库的句柄。它是纯机制对象：不认识任何一张业务表，
// 各业务包拿到它之后用 Migrate 建自己的表、用 bun 查询构建器读写自己的行。
//
// DB 内嵌 *bun.DB：业务包直接用 NewInsert/NewSelect/NewDelete/RunInTx 等构建器
// 与事务 API，查询文本由 bun 按方言生成。SQL() 仍交出底层 *sql.DB，作为
// 可移植原生语句（PRAGMA、迁移 DDL、Stats 计数）的出口。
type DB struct {
	*bun.DB
	dialect Dialect
	sqlDB   *sql.DB
	path    string
	logger  *slog.Logger

	// journalMode 与 synchronous 记的是实际生效的 PRAGMA，不是配置里的期望值：
	// 网络文件系统上 WAL 会静默退回 delete，现场判断要看的是前者（本卡 §9 风险 2）。
	// 仅 sqlite 方言有值；PG 方言没有对应物，记 "n/a"。
	journalMode string
	synchronous string
	closeOnce   sync.Once
	closeErr    error
}

// Open 按方言打开一个数据库。建表属于业务（Migrate 由调用方接着调）。
//
// 建不起库等于什么都记不住，所以任何一步失败都返回错误，由装配方决定是否终止启动
// ——cmd/server 的口径是不带"记不住日志"的状态上线（设计文档 §11）。
// opts 正常应由业务包从各自的配置节映射而来；这里仍对空路径与非法 synchronous 兜底报错，
// 因为本包也会被装配方之外的调用方直接使用。
func Open(opts Options, logger *slog.Logger) (*DB, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dialect := Dialect(strings.TrimSpace(string(opts.Dialect)))
	if dialect == "" {
		dialect = DialectSQLite
	}
	switch dialect {
	case DialectSQLite:
		return openSQLite(opts, logger)
	case DialectPostgres:
		return openPostgres(opts, logger)
	default:
		return nil, fmt.Errorf("engine: unknown dialect %q, use sqlite or postgres", opts.Dialect)
	}
}

// openSQLite 建好父目录、打开连接、设置 PRAGMA（sqlite 方言分支）。
func openSQLite(opts Options, logger *slog.Logger) (*DB, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("sqlite: path must not be empty")
	}
	// 路径要拼进 DSN，而 '?' 与 '#' 在 DSN 里是查询串与片段的分隔符：
	// 含这两个字符的路径会被解析成"文件名 + 一串 PRAGMA"，与其得出一条看不懂的启动失败，
	// 不如在这里直接说清路径不合法。Windows 本来也不允许文件名带 '?'。
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("sqlite: path %q must not contain '?' or '#'", path)
	}
	synchronous := strings.TrimSpace(opts.Synchronous)
	switch synchronous {
	case "", "normal", "full":
	default:
		return nil, fmt.Errorf("sqlite: synchronous %q is invalid, use normal or full", opts.Synchronous)
	}
	if synchronous == "" {
		synchronous = DefaultSynchronous
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

	sqlDB, err := sql.Open("sqlite", dsn(path, opts.BusyTimeout, synchronous))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	maxConns := opts.MaxOpenConns
	if maxConns <= 0 {
		maxConns = 1
	}
	sqlDB.SetMaxOpenConns(maxConns)

	db := &DB{
		DB:      bun.NewDB(sqlDB, sqlitedialect.New()),
		dialect: DialectSQLite,
		sqlDB:   sqlDB,
		path:    path,
		logger:  logger,
	}
	if err := db.checkConnectionSettings(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// dsn 把 PRAGMA 写进连接串，而不是打开后执行一次：
// busy_timeout、synchronous、foreign_keys 都是每条连接的设置，
// 连接因故重建后只有 DSN 里这份还会生效。
func dsn(path string, busyTimeout time.Duration, synchronous string) string {
	timeout := busyTimeout
	if timeout < 0 {
		timeout = 0
	}
	return fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(%s)&_pragma=foreign_keys(1)",
		path, timeout.Milliseconds(), strings.ToUpper(synchronous))
}

// checkConnectionSettings 读回实际生效的 PRAGMA。
//
// 期望值没生效时只记日志、不报错：journal_mode 在网络文件系统上会静默退回 delete，
// 那是要部署方判断的环境问题，让库带着降级继续跑比拒绝启动更有用。
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

	d.logger.Debug("sqlite database opened",
		"path", d.path,
		"journal_mode", d.journalMode,
		"synchronous", d.synchronous)
	if !strings.EqualFold(d.journalMode, "wal") {
		d.logger.Warn("sqlite database is not in WAL mode",
			"path", d.path,
			"journal_mode", d.journalMode,
			"hint", "this filesystem does not support WAL (network mount?), read and write no longer overlap")
	}
	return nil
}

// Migration 是一段按版本号顺序执行的内置语句。
type Migration struct {
	Version int
	// Up 里的每一条都是独立可执行的语句；整批在同一个事务里跑，
	// 中途失败时版本号不会写进去，下次启动重来。
	// 这是通用语句集：SQLite 与 PostgreSQL 语义一致时只填这一份。
	Up []string
	// UpPostgres 是 PostgreSQL 专属的替代语句；nil（或空）表示与 Up 相同。
	// 只在方言确实分歧时才给（自增主键、大小写不敏感唯一索引），避免两份 DDL
	// 无意义地漂移。
	UpPostgres []string
}

// statements 按方言取出本版本的语句列表。
func (m *Migration) statements(d Dialect) []string {
	if d == DialectPostgres && len(m.UpPostgres) > 0 {
		return m.UpPostgres
	}
	return m.Up
}

// Migrate 按版本号顺序执行 ms：先建迁移版本表（表名由调用方给，各业务库用不同的表名，
// 同一台机器上多库共存时迁移记录不能撞车），再只跑比库里已应用版本更高的那些，
// 每个版本一个事务，跑成功后落一行版本记录。中途失败时版本号不会落下，下次启动重跑同一个版本。
func (d *DB) Migrate(table string, ms []Migration) error {
	// table 来自代码里的常量而非用户输入，直接拼接是安全的。
	createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  version    INTEGER NOT NULL,
  applied_at INTEGER NOT NULL              -- 写入时的 Unix 秒
);`, table)
	if _, err := d.sqlDB.Exec(createTable); err != nil {
		return fmt.Errorf("sqlite: create migration table: %w", err)
	}

	readVersion := fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) FROM %s`, table)
	var applied int
	if err := d.sqlDB.QueryRow(readVersion).Scan(&applied); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}

	// 记账语句经 *sql.DB 原生执行，占位符风格由方言决定：pgx 只认 $N，
	// 不会把 ? 转译过去；sqlite 两边通吃。
	insert := fmt.Sprintf(`INSERT INTO %s (version, applied_at) VALUES (?, ?)`, table)
	if d.dialect == DialectPostgres {
		insert = fmt.Sprintf(`INSERT INTO %s (version, applied_at) VALUES ($1, $2)`, table)
	}
	for _, m := range ms {
		if m.Version <= applied {
			continue
		}
		tx, err := d.sqlDB.Begin()
		if err != nil {
			return fmt.Errorf("sqlite: begin migration %d: %w", m.Version, err)
		}
		for _, stmt := range m.statements(d.dialect) {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("sqlite: apply migration %d: %w", m.Version, err)
			}
		}
		if _, err := tx.Exec(insert, m.Version, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit migration %d: %w", m.Version, err)
		}
	}
	return nil
}

// SQL 交出底层连接，供各业务包执行可移植原生语句（PRAGMA、Stats 计数等）。
// 常规读写请用内嵌 *bun.DB 的查询构建器。句柄的关闭仍归 Close 管：
// 写入器先撤订阅并落完最后一批，最后才轮到本方法拿到的这条连接退场（设计文档 §7.3）。
func (d *DB) SQL() *sql.DB {
	return d.sqlDB
}

// Stats 返回连接池的瞬时占用（G4/Q05）：运维端点读数用。
// 机制读数归机制包：业务包（jobs/observe/mgr）的读数方法折算自这里，
// 不各自摸 *sql.DB。
func (d *DB) Stats() sql.DBStats {
	return d.sqlDB.Stats()
}

// MigrationVersion 读取已应用迁移的最高版本号；迁移表无行（全新库）返回 0。
// 与 Migrate 共用同一张版本表（表名由调用方给）与同一条读法——版本号的
// 权威语义只有这一份，运维读数与迁移判定不许分叉。
// 语句无占位符，两方言通吃，与 Migrate 的 readVersion 同一取向。
func (d *DB) MigrationVersion(table string) (int, error) {
	readVersion := fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) FROM %s`, table)
	var applied int
	if err := d.sqlDB.QueryRow(readVersion).Scan(&applied); err != nil {
		return 0, fmt.Errorf("engine: read schema version of %s: %w", table, err)
	}
	return applied, nil
}

// Dialect 返回本句柄使用的方言。
func (d *DB) Dialect() Dialect {
	return d.dialect
}

// Path 返回本句柄打开的库文件路径，照配置里的写法返回，不做绝对化。
// PG 方言返回脱敏后的 DSN（去密码），供装配方写进启动日志。
func (d *DB) Path() string {
	return d.path
}

// JournalMode 返回实际生效的日志模式，供装配方把降级情况写进启动日志。
// 仅 sqlite 方言有值；PG 方言没有对应物，返回 "n/a"。
func (d *DB) JournalMode() string {
	return d.journalMode
}

// Synchronous 返回实际生效的 synchronous 取值（normal|full|off），
// 与配置里的期望值对照才能看出"这一份部署其实没有那条保证"。
// 仅 sqlite 方言有值；PG 方言返回 "n/a"。
func (d *DB) Synchronous() string {
	return d.synchronous
}

// Close 关闭连接。幂等：重复调用返回第一次的结果，不会二次关闭。
//
// 顺序由装配方的 defer 保证（设计文档 §7.3）：先撤订阅读者、再关各写入器、最后关本句柄；
// 颠倒过来就会在已关闭的连接上写入。
func (d *DB) Close() error {
	d.closeOnce.Do(func() { d.closeErr = d.sqlDB.Close() })
	return d.closeErr
}
