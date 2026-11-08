package mgr

import (
	"log/slog"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/store/engine"
)

// Handle 是管理库的句柄：公共底座句柄嵌入，本包暂无额外读数。
// 若日后要给运维端点加管理库读数（账号数、最近导入时间），挂在本类型上。
type Handle struct {
	*engine.DB
}

// Open 建好父目录、打开连接、设置 PRAGMA、建表并应用管理库迁移。
// 机制全部委托 engine.Open 与 DB.Migrate，本文件只负责把 core.MgrStoreConfig
// 映射成 Options、指出管理库自己的迁移表与表清单。
//
// 建不起管理库等于账号、分组、档位都无处安放，而 mgrstore.enabled=true 又表明
// 部署方已经决定以库为准——任何一步失败都返回错误，由装配方拒绝启动，
// 与观测库"不带记不住日志的状态上线"同一口径。
func Open(cfg core.MgrStoreConfig, logger *slog.Logger) (*Handle, error) {
	db, err := engine.Open(engine.Options{
		Path:        cfg.Path,
		BusyTimeout: cfg.BusyTimeout,
		Synchronous: cfg.Synchronous,
		// 单连接：管理库写是低频实体（建号、建组、建档位），读是登录与验签的
		// 短查询；与观测库同一取舍，同时消除 SQLITE_BUSY 的主要来源。
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
