// Package logger configures structured logging.
package logger

import "go.uber.org/zap"

// New creates a production JSON logger.
func New() (*zap.Logger, error) { return zap.NewProduction() }
