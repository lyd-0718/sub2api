package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// APIKeyConcurrencyLimiter extends stats-only tracking with atomic admission.
// Both ordinary requests and Live calls consume the same key-level capacity.
type APIKeyConcurrencyLimiter interface {
	AcquireAPIKeySlot(ctx context.Context, apiKeyID int64, maxConcurrency int, requestID string) (bool, error)
	RefreshAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) (bool, error)
	ReleaseAPIKeySlot(ctx context.Context, apiKeyID int64, requestID string) error
	APIKeySlotTTL() time.Duration
}

var ErrAPIKeyConcurrencyLeaseLost = errors.New("api key concurrency lease lost")

// AcquireAPIKeySlot is fail-closed for configured caps. Unlimited keys retain
// their existing best-effort stats tracking without a renewal goroutine.
func (s *ConcurrencyService) AcquireAPIKeySlot(ctx context.Context, apiKeyID int64, maxConcurrency int) (*AcquireResult, error) {
	if maxConcurrency <= 0 {
		return &AcquireResult{Acquired: true, ReleaseFunc: s.TrackAPIKeySlot(ctx, apiKeyID)}, nil
	}
	if s == nil || s.cache == nil || apiKeyID <= 0 {
		return nil, fmt.Errorf("api key concurrency service is unavailable")
	}
	cache, ok := s.cache.(APIKeyConcurrencyLimiter)
	if !ok || cache.APIKeySlotTTL() <= 0 {
		return nil, fmt.Errorf("api key concurrency cache is unavailable")
	}
	requestID := generateRequestID()
	acquireCtx, cancel := context.WithTimeout(ctx, apiKeySlotTrackTimeout)
	acquired, err := cache.AcquireAPIKeySlot(acquireCtx, apiKeyID, maxConcurrency, requestID)
	cancel()
	if err != nil {
		return nil, err
	}
	if !acquired {
		return &AcquireResult{Acquired: false}, nil
	}

	slotCtx, cancelSlot := context.WithCancelCause(ctx)
	slot := &apiKeyConcurrencySlot{
		ctx: slotCtx, cancel: cancelSlot, cache: cache,
		apiKeyID: apiKeyID, requestID: requestID,
		ttl: cache.APIKeySlotTTL(), done: make(chan struct{}),
	}
	go slot.run()
	return &AcquireResult{
		Acquired: true, Context: slotCtx,
		ReleaseFunc: func() {
			cancelSlot(nil)
			<-slot.done
		},
	}, nil
}

type apiKeyConcurrencySlot struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	cache     APIKeyConcurrencyLimiter
	apiKeyID  int64
	requestID string
	ttl       time.Duration
	done      chan struct{}
}

func (s *apiKeyConcurrencySlot) run() {
	defer close(s.done)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.cache.ReleaseAPIKeySlot(ctx, s.apiKeyID, s.requestID); err != nil {
			logger.LegacyPrintf("service.concurrency", "Warning: failed to release api key slot for %d (req=%s): %v", s.apiKeyID, s.requestID, err)
		}
	}()

	interval := min(s.ttl/3, 20*time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastConfirmed := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			var lost bool
			lastConfirmed, lost = s.refresh(lastConfirmed)
			if lost {
				s.cancel(ErrAPIKeyConcurrencyLeaseLost)
				return
			}
		}
	}
}

func (s *apiKeyConcurrencySlot) refresh(lastConfirmed time.Time) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(s.ctx, min(apiKeySlotTrackTimeout, s.ttl/3))
	owned, err := s.cache.RefreshAPIKeySlot(ctx, s.apiKeyID, s.requestID)
	cancel()
	if err == nil && owned {
		return time.Now(), false
	}
	if s.ctx.Err() != nil {
		return lastConfirmed, false
	}
	if err == nil {
		return lastConfirmed, true
	}
	logger.LegacyPrintf("service.concurrency", "Warning: failed to refresh api key slot for %d (req=%s): %v", s.apiKeyID, s.requestID, err)
	// Stop before Redis may expire the last confirmed slot, not one tick later.
	return lastConfirmed, time.Since(lastConfirmed) >= s.ttl-min(s.ttl/3, 20*time.Second)
}
