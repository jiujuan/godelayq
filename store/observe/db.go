package observe

import (
	"fmt"
	"log/slog"

	"godelayq/core"
	"godelayq/store/sqlite"
)

// Handle 是观测库的句柄：公共底座句柄加上观测库自己的读数（Stats）。
// 嵌入让 Path/JournalMode/Synchronous/Close/SQL 按 *sqlite.DB 的原样透出，
// 装配方（cmd/server）的 observabilityDB 接口因此由本类型满足，方法集不变。
type Handle struct {
	*sqlite.DB
}

// Open 建好父目录、打开连接、设置 PRAGMA、建表并应用观测层迁移。
// 机制全部委托 sqlite.OpenFile 与 DB.Migrate，本文件只负责把 core.ObservabilityConfig
// 映射成 Options、指出观测库自己的迁移表与表清单。
//
// 建不起库等于什么都记不住，所以任何一步失败都返回错误，由装配方决定是否终止启动
// ——cmd/server 的口径是不带"记不住日志"的状态上线（设计文档 §11）。
// cfg 正常应由 core.Config.Normalized() 补齐过；空路径与非法 synchronous 仍会在
// sqlite.OpenFile 里兜底报错，因本包也会被装配方之外的调用方直接使用。
func Open(cfg core.ObservabilityConfig, logger *slog.Logger) (*Handle, error) {
	db, err := sqlite.OpenFile(sqlite.Options{
		Path:        cfg.Path,
		BusyTimeout: cfg.BusyTimeout,
		Synchronous: cfg.Synchronous,
		// 写死一个连接：写入批量单点、读是短查询，单连接同时消除 SQLITE_BUSY 的
		// 主要来源（sqlite-observability-design.md §7.2）。要放开时另议（§8 不在范围内）。
		MaxOpenConns: 1,
	}, logger)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(migrationTableName, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Handle{DB: db}, nil
}

// Stats 是观测库的自我描述：三张表的行数与已应用的迁移版本。
// 各写入器的"丢了多少"长在写入器自己身上（batcher.Dropped），Handle 拿不到，
// 由 S03/S06 的 EventLog、AuditLog 一起上报给运维端点。
type Stats struct {
	Events        int64
	Artifacts     int64
	AuditRows     int64
	SchemaVersion int
}

// Stats 查三张表的行数与迁移版本。任一查询失败都返回错误而不是 0：
// 把"读不出来"报成"表是空的"，等于让运维以为观测层正在正常工作。
func (h *Handle) Stats() (Stats, error) {
	var stats Stats
	if err := h.SQL().QueryRow(currentSchemaVersion).Scan(&stats.SchemaVersion); err != nil {
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
		if err := h.SQL().QueryRow(item.query).Scan(item.dst); err != nil {
			return stats, fmt.Errorf("sqlite: count rows: %w", err)
		}
	}
	return stats, nil
}
