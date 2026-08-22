// Package mysql opens the database owned by Analytics.
package mysql

import (
	"context"
	"fmt"

	"github.com/weouc-plus/campus-academic/internal/core/bootstrap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Open creates a GORM connection for the Analytics publication database.
func Open(ctx context.Context, config bootstrap.MySQLConfig) (*gorm.DB, error) {
	if config.DSN == "" {
		return nil, fmt.Errorf("academic analytics MySQL DSN is required")
	}
	db, err := gorm.Open(gormmysql.Open(config.DSN), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open academic analytics MySQL: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get academic analytics SQL database: %w", err)
	}
	if config.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(config.MaxOpenConns)
	}
	if config.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(config.MaxIdleConns)
	}
	if config.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(config.ConnMaxLifetime)
	}
	if config.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(config.ConnMaxIdleTime)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping academic analytics MySQL: %w", err)
	}
	return db, nil
}
