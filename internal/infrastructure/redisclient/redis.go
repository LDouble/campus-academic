// Package redisclient opens Redis connections owned by campus-academic.
package redisclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/LDouble/campus-academic/internal/core/bootstrap"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const maxTLSFileSize = 1 << 20

// Options creates a bounded Redis client configuration for one service DB.
func Options(config bootstrap.RedisConfig) (*redis.Options, error) {
	tlsConfig, err := TLSConfig(config)
	if err != nil {
		return nil, err
	}
	return &redis.Options{
		Addr:      config.Address,
		Username:  config.Username,
		Password:  config.Password,
		DB:        config.DB,
		TLSConfig: tlsConfig,
	}, nil
}

// Open creates and verifies a Redis client before returning it to the caller.
func Open(ctx context.Context, config bootstrap.RedisConfig) (*redis.Client, error) {
	options, err := Options(config)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping academic Redis: %w", err)
	}
	return client, nil
}

// AsynqOptions returns the same authenticated TLS transport used by the
// Analytics Redis client so the task producer and worker cannot drift.
func AsynqOptions(config bootstrap.RedisConfig) (asynq.RedisClientOpt, error) {
	tlsConfig, err := TLSConfig(config)
	if err != nil {
		return asynq.RedisClientOpt{}, err
	}
	return asynq.RedisClientOpt{
		Addr:      config.Address,
		Username:  config.Username,
		Password:  config.Password,
		DB:        config.DB,
		TLSConfig: tlsConfig,
	}, nil
}

// TLSConfig loads Redis trust material from one confined operator-controlled
// directory. Client certificates are optional, but must be configured as a pair.
func TLSConfig(config bootstrap.RedisConfig) (*tls.Config, error) {
	if !config.TLS {
		return nil, nil
	}
	if !filepath.IsAbs(config.TLSFilesRoot) {
		return nil, fmt.Errorf("Redis TLS files root must be absolute")
	}
	root, err := os.OpenRoot(config.TLSFilesRoot)
	if err != nil {
		return nil, fmt.Errorf("open Redis TLS root: %w", err)
	}
	defer func() { _ = root.Close() }()

	caData, err := readTLSFile(root, config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read Redis CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("parse Redis CA")
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: config.ServerName,
	}
	if config.ClientCertFile == "" && config.ClientKeyFile == "" {
		return tlsConfig, nil
	}
	certData, err := readTLSFile(root, config.ClientCertFile)
	if err != nil {
		return nil, fmt.Errorf("read Redis client certificate: %w", err)
	}
	keyData, err := readTLSFile(root, config.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read Redis client key: %w", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return nil, fmt.Errorf("parse Redis client key pair: %w", err)
	}
	tlsConfig.Certificates = []tls.Certificate{certificate}
	return tlsConfig, nil
}

func readTLSFile(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTLSFileSize {
		return nil, fmt.Errorf("TLS material must be a regular file no larger than %d bytes", maxTLSFileSize)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTLSFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTLSFileSize {
		return nil, fmt.Errorf("TLS material must be no larger than %d bytes", maxTLSFileSize)
	}
	return data, nil
}
