package rpc

import (
	"context"
	"time"

	academicapp "github.com/LDouble/campus-academic/internal/modules/academic/application"
)

type bulkhead struct {
	permits    chan struct{}
	wait       time.Duration
	retryAfter time.Duration
}

func newBulkhead(maxConcurrent int, wait, retryAfter time.Duration) *bulkhead {
	return &bulkhead{
		permits:    make(chan struct{}, maxConcurrent),
		wait:       wait,
		retryAfter: retryAfter,
	}
}

func (b *bulkhead) acquire(ctx context.Context) (func(), error) {
	if b.wait <= 0 {
		select {
		case b.permits <- struct{}{}:
			return func() { <-b.permits }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, &academicapp.ProviderBusyError{RetryAfter: b.retryAfter}
		}
	}
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	select {
	case b.permits <- struct{}{}:
		return func() { <-b.permits }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, &academicapp.ProviderBusyError{RetryAfter: b.retryAfter}
	}
}
