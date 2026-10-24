// Package infra 负责基础设施连接：MySQL、Redis、RabbitMQ。
package infra

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"imchat/internal/config"
	"imchat/internal/model"
)

// NewMySQL 建立连接池并完成必要调优。
// 关键点：预编译语句缓存、连接生命周期短于 MySQL wait_timeout、慢查询阈值 200ms。
func NewMySQL(cfg config.MySQLConfig, mode string) (*gorm.DB, error) {
	logLevel := gormlogger.Warn
	if mode == "debug" {
		logLevel = gormlogger.Warn // 生产/开发都用 Warn，避免 SQL 日志拖慢压测
	}

	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       cfg.DSN + "&interpolateParams=true",
		DefaultStringSize:         255,
		SkipInitializeWithVersion: false,
	}), &gorm.Config{
		Logger:                 gormlogger.Default.LogMode(logLevel),
		SkipDefaultTransaction: true, // 关闭默认事务包装，单条写操作性能提升明显
		PrepareStmt:            true, // 缓存预编译语句，降低解析开销
		NowFunc:                func() time.Time { return time.Now().Local() },
	})
	if err != nil {
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层连接池失败: %w", err)
	}
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime.Get())
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime.Get())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("MySQL ping 失败: %w", err)
	}
	return db, nil
}

// Migrate 执行表结构迁移。
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(model.AllModels()...); err != nil {
		return fmt.Errorf("表结构迁移失败: %w", err)
	}
	slog.Info("表结构迁移完成", "tables", len(model.AllModels()))
	return nil
}
