// Package redisclient opens Redis connections owned by campus-academic.
package redisclient

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/weouc-plus/campus-academic/internal/core/bootstrap"
)

// Options creates a bounded Redis client configuration for one service DB.
func Options(config bootstrap.RedisConfig) *redis.Options {
	return &redis.Options{
		Addr:     config.Address,
		Password: config.Password,
		DB:       config.DB,
	}
}

// Open creates and verifies a Redis client before returning it to the caller.
func Open(ctx context.Context, config bootstrap.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(Options(config))
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping academic Redis: %w", err)
	}
	return client, nil
}
