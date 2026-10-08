package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyConcurrencyIsAtomicAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	clients := []*redis.Client{
		redis.NewClient(&redis.Options{Addr: server.Addr()}),
		redis.NewClient(&redis.Options{Addr: server.Addr()}),
	}
	limiters := make([]service.APIKeyConcurrencyLimiter, len(clients))
	for i, client := range clients {
		t.Cleanup(func() { _ = client.Close() })
		limiters[i] = NewConcurrencyCache(client, 15, 900).(service.APIKeyConcurrencyLimiter)
	}
	ctx := context.Background()
	const cap, attempts = 3, 24
	type outcome struct {
		id       string
		acquired bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, attempts)
	var workers sync.WaitGroup
	for i := range attempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			id := fmt.Sprintf("instance-request-%d", i)
			acquired, err := limiters[i%len(limiters)].AcquireAPIKeySlot(ctx, 16, cap, id)
			results <- outcome{id: id, acquired: acquired, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var owners []string
	for result := range results {
		require.NoError(t, result.err)
		if result.acquired {
			owners = append(owners, result.id)
		}
	}
	require.Len(t, owners, cap, "concurrent instances must never over-admit a shared key")

	otherKey, err := limiters[0].AcquireAPIKeySlot(ctx, 17, 1, "independent-key")
	require.NoError(t, err)
	require.True(t, otherKey, "a full key must not consume another key's capacity")
	require.NoError(t, limiters[1].ReleaseAPIKeySlot(ctx, 16, owners[0]))
	reused, err := limiters[1].AcquireAPIKeySlot(ctx, 16, cap, "replacement")
	require.NoError(t, err)
	require.True(t, reused)
	blocked, err := limiters[0].AcquireAPIKeySlot(ctx, 16, cap, "still-full")
	require.NoError(t, err)
	require.False(t, blocked)
}

func TestAPIKeySlotRenewalPreservesCapacityWithoutResurrection(t *testing.T) {
	server := miniredis.RunT(t)
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	server.SetTime(start)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	limiter := NewConcurrencyCache(client, 1, 60).(service.APIKeyConcurrencyLimiter)
	ctx := context.Background()
	acquired, err := limiter.AcquireAPIKeySlot(ctx, 16, 1, "long-stream")
	require.NoError(t, err)
	require.True(t, acquired)

	server.SetTime(start.Add(59 * time.Second))
	renewed, err := limiter.RefreshAPIKeySlot(ctx, 16, "long-stream")
	require.NoError(t, err)
	require.True(t, renewed)
	server.SetTime(start.Add(61 * time.Second))
	acquired, err = limiter.AcquireAPIKeySlot(ctx, 16, 1, "overlap")
	require.NoError(t, err)
	require.False(t, acquired, "renewed streams must still exclude overlapping requests")

	server.SetTime(start.Add(120 * time.Second))
	renewed, err = limiter.RefreshAPIKeySlot(ctx, 16, "long-stream")
	require.NoError(t, err)
	require.False(t, renewed, "expired ownership must not be silently reclaimed")
	acquired, err = limiter.AcquireAPIKeySlot(ctx, 16, 1, "after-crash")
	require.NoError(t, err)
	require.True(t, acquired)
	renewed, err = limiter.RefreshAPIKeySlot(ctx, 16, "long-stream")
	require.NoError(t, err)
	require.False(t, renewed, "old owners must not steal replacement capacity")
}
