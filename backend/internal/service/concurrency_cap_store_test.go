//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeConcurrencyCapRepo 是 ConcurrencyCapRepository 的内存实现，用来验证 store 的
// 回落链、窗口口径与批量读取，不关心 SQL/Redis 细节。
type fakeConcurrencyCapRepo struct {
	mu sync.Mutex

	records map[int64]*AccountConcurrencyCap
	redis   map[int64]int
	// redisUnavailable 模拟 Redis 不可用（硬路径必须回落进程缓存）。
	redisUnavailable bool
	// redisMissing 模拟写穿键丢失（真源仍有记录）。
	redisMissing bool

	appendedFlaps []time.Time
	upsertCalls   int
	cachedWrites  map[int64]int
}

func newFakeConcurrencyCapRepo() *fakeConcurrencyCapRepo {
	return &fakeConcurrencyCapRepo{
		records:      make(map[int64]*AccountConcurrencyCap),
		redis:        make(map[int64]int),
		cachedWrites: make(map[int64]int),
	}
}

func (r *fakeConcurrencyCapRepo) GetCap(_ context.Context, accountID int64) (*AccountConcurrencyCap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[accountID]
	if !ok {
		return nil, nil
	}
	clone := *record
	clone.FlapEvents = append([]time.Time(nil), record.FlapEvents...)
	return &clone, nil
}

func (r *fakeConcurrencyCapRepo) UpsertCap(_ context.Context, accountID int64, capValue int, reason string, restrictedAt *time.Time, nextProbeAt *time.Time, preserveRestrictedAt bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsertCalls++
	record, ok := r.records[accountID]
	if !ok {
		record = &AccountConcurrencyCap{AccountID: accountID}
		r.records[accountID] = record
	} else {
		record.Version++
	}
	record.Cap = capValue
	record.Reason = reason
	if restrictedAt != nil {
		if !preserveRestrictedAt || record.RestrictedAt.IsZero() {
			record.RestrictedAt = *restrictedAt
		}
	} else {
		record.RestrictedAt = time.Time{}
	}
	if nextProbeAt != nil {
		record.NextProbeAt = *nextProbeAt
	} else {
		record.NextProbeAt = time.Time{}
	}
	record.UpdatedAt = time.Now()
	return nil
}

func (r *fakeConcurrencyCapRepo) SetPinned(_ context.Context, accountID int64, pinned bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[accountID]
	if !ok {
		return ErrConcurrencyCapNotFound
	}
	record.Pinned = pinned
	record.Version++
	return nil
}

func (r *fakeConcurrencyCapRepo) SetNextProbeAt(_ context.Context, accountID int64, at *time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[accountID]
	if !ok {
		return ErrConcurrencyCapNotFound
	}
	if at == nil {
		record.NextProbeAt = time.Time{}
	} else {
		record.NextProbeAt = *at
	}
	record.Reason = reason
	record.Version++
	return nil
}

func (r *fakeConcurrencyCapRepo) AppendFlapEvent(_ context.Context, accountID int64, at time.Time, windowDays int) ([]time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[accountID]
	if !ok {
		return nil, ErrConcurrencyCapNotFound
	}
	r.appendedFlaps = append(r.appendedFlaps, at)
	cutoff := at.Add(-time.Duration(windowDays) * 24 * time.Hour)
	events := make([]time.Time, 0, len(record.FlapEvents)+1)
	for _, event := range record.FlapEvents {
		if !event.Before(cutoff) {
			events = append(events, event)
		}
	}
	events = append(events, at)
	record.FlapEvents = events
	record.Version++
	return append([]time.Time(nil), events...), nil
}

func (r *fakeConcurrencyCapRepo) ClearFlapEvents(_ context.Context, accountID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if record, ok := r.records[accountID]; ok {
		record.FlapEvents = nil
	}
	return nil
}

func (r *fakeConcurrencyCapRepo) ListRestricted(_ context.Context, capMax int) ([]*AccountConcurrencyCap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*AccountConcurrencyCap, 0, len(r.records))
	for _, record := range r.records {
		if record.Cap < capMax || !record.NextProbeAt.IsZero() {
			clone := *record
			out = append(out, &clone)
		}
	}
	return out, nil
}

func (r *fakeConcurrencyCapRepo) GetCachedCap(_ context.Context, accountID int64) (int, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisUnavailable {
		return 0, false, errors.New("redis unavailable")
	}
	if r.redisMissing {
		return 0, false, nil
	}
	value, ok := r.redis[accountID]
	return value, ok, nil
}

func (r *fakeConcurrencyCapRepo) GetCachedCaps(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisUnavailable {
		return nil, errors.New("redis unavailable")
	}
	caps := make(map[int64]int, len(accountIDs))
	for _, accountID := range accountIDs {
		if value, ok := r.redis[accountID]; ok {
			caps[accountID] = value
		}
	}
	return caps, nil
}

func (r *fakeConcurrencyCapRepo) SetCachedCap(_ context.Context, accountID int64, capValue int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisUnavailable {
		return errors.New("redis unavailable")
	}
	r.redis[accountID] = capValue
	r.cachedWrites[accountID] = capValue
	return nil
}

func (r *fakeConcurrencyCapRepo) SetCachedCaps(_ context.Context, caps map[int64]int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisUnavailable {
		return errors.New("redis unavailable")
	}
	for accountID, capValue := range caps {
		r.redis[accountID] = capValue
	}
	return nil
}

func newTestConcurrencyCapStore(repo *fakeConcurrencyCapRepo) ConcurrencyCapStore {
	return NewConcurrencyCapStore(repo, ConcurrencyCapStoreConfig{
		Enabled:      true,
		Restricted:   1,
		CapMax:       3,
		HoldHours:    72,
		ObserveHours: 12,
		CacheTTL:     5 * time.Second,
	})
}

func TestConcurrencyCapStoreEffectiveCapUnknownWhenNoRecord(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)

	capValue, known, err := store.EffectiveCap(context.Background(), 7)

	require.NoError(t, err)
	require.False(t, known, "无记录必须返回 known=false，调用方据此不夹帽")
	require.Zero(t, capValue)
}

func TestConcurrencyCapStoreEffectiveCapReadsWriteThroughKey(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	repo.redis[11] = 1
	store := newTestConcurrencyCapStore(repo)

	capValue, known, err := store.EffectiveCap(context.Background(), 11)

	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, 1, capValue)
}

func TestConcurrencyCapStoreEffectiveCapFallsBackToProcessCache(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	repo.redis[12] = 2
	store := newTestConcurrencyCapStore(repo)

	_, known, err := store.EffectiveCap(context.Background(), 12)
	require.NoError(t, err)
	require.True(t, known)

	// 写穿键丢失：进程缓存仍持有最近一次真值，硬路径继续夹帽。
	repo.redisMissing = true
	capValue, known, err := store.EffectiveCap(context.Background(), 12)

	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, 2, capValue)
}

func TestConcurrencyCapStoreEffectiveCapReportsErrorWhenCacheAlsoEmpty(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	repo.redisUnavailable = true
	store := newTestConcurrencyCapStore(repo)

	capValue, known, err := store.EffectiveCap(context.Background(), 13)

	require.Error(t, err)
	require.False(t, known)
	require.Zero(t, capValue)
}

func TestConcurrencyCapStoreEffectiveCapDisabledNeverClamps(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	repo.redis[14] = 1
	store := NewConcurrencyCapStore(repo, ConcurrencyCapStoreConfig{Enabled: false})

	_, known, err := store.EffectiveCap(context.Background(), 14)

	require.NoError(t, err)
	require.False(t, known)
}

func TestConcurrencyCapStoreSetCapArmsHoldWindowForRestrictedCap(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	before := time.Now()

	require.NoError(t, store.SetCap(context.Background(), 21, 1, "concurrency_403"))

	record, err := store.GetCap(context.Background(), 21)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, 1, record.Cap)
	require.Equal(t, 1, repo.redis[21], "写操作必须写穿 Redis")
	require.False(t, record.RestrictedAt.Before(before))
	require.WithinDuration(t, record.RestrictedAt.Add(72*time.Hour), record.NextProbeAt, time.Minute)
}

func TestConcurrencyCapStoreSetCapArmsObserveWindowForMiddleStep(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)

	require.NoError(t, store.SetCap(context.Background(), 22, 2, "probe_pass"))

	record, err := store.GetCap(context.Background(), 22)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.WithinDuration(t, time.Now().Add(12*time.Hour), record.NextProbeAt, time.Minute)
}

func TestConcurrencyCapStoreSetCapAtCapMaxClearsRestriction(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()
	require.NoError(t, store.SetCap(ctx, 23, 1, "concurrency_403"))
	firstRestrictedAt, err := store.GetCap(ctx, 23)
	require.NoError(t, err)

	require.NoError(t, store.SetCap(ctx, 23, 3, "probe_pass"))

	record, err := store.GetCap(ctx, 23)
	require.NoError(t, err)
	require.Equal(t, 3, record.Cap)
	require.True(t, record.RestrictedAt.IsZero(), "回到阶梯上限即不再受限")
	require.True(t, record.NextProbeAt.IsZero(), "回到阶梯上限即不再探测")
	require.False(t, firstRestrictedAt.RestrictedAt.IsZero())
}

func TestConcurrencyCapStoreSetCapKeepsFirstRestrictedAt(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()
	require.NoError(t, store.SetCap(ctx, 24, 1, "concurrency_403"))
	first, err := store.GetCap(ctx, 24)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, store.SetCap(ctx, 24, 1, "concurrency_403"))

	second, err := store.GetCap(ctx, 24)
	require.NoError(t, err)
	require.Equal(t, first.RestrictedAt, second.RestrictedAt, "重复降级不得刷新首次受限时间")
	require.True(t, second.NextProbeAt.After(first.NextProbeAt) || second.NextProbeAt.Equal(first.NextProbeAt),
		"每次 SetCap 按当前档重排下次探测时间")
}

// concurrencyCapProbeRescheduler 与探测编排（slice D 的 capProbeRescheduleStore）同形：
// cap 不变时的精确重排能力，签名必须保持一致。
type concurrencyCapProbeRescheduler interface {
	SetNextProbeAt(ctx context.Context, accountID int64, at time.Time, reason string) error
}

func TestConcurrencyCapStoreSetNextProbeAtReschedulesWithoutTouchingCap(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()
	require.NoError(t, store.SetCap(ctx, 26, 1, "concurrency_403"))

	rescheduler, ok := store.(concurrencyCapProbeRescheduler)
	require.True(t, ok, "探测编排依赖精确重排能力，store 必须实现")
	at := time.Now().Add(time.Hour)
	require.NoError(t, rescheduler.SetNextProbeAt(ctx, 26, at, "probe_inconclusive"))

	record, err := store.GetCap(ctx, 26)
	require.NoError(t, err)
	require.Equal(t, 1, record.Cap, "精确重排不得改变 cap")
	require.WithinDuration(t, at, record.NextProbeAt, time.Second)
	require.Equal(t, "probe_inconclusive", record.Reason)
}

func TestConcurrencyCapStoreSetCapRejectsNonPositiveCap(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)

	require.Error(t, store.SetCap(context.Background(), 25, 0, "admin"))

	require.Zero(t, repo.upsertCalls)
}

func TestConcurrencyCapStoreRecordFlapCreatesRestrictedRecordWhenMissing(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	at := time.Now()

	count, err := store.RecordFlap(context.Background(), 31, at)

	require.NoError(t, err)
	require.Equal(t, 1, count)
	record, err := store.GetCap(context.Background(), 31)
	require.NoError(t, err)
	require.NotNil(t, record, "抖动必须先有记录，否则熔断计数会静默丢失")
	require.Equal(t, 1, record.Cap)
}

func TestConcurrencyCapStoreRecordFlapCountsWithinRollingWindow(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()
	require.NoError(t, store.SetCap(ctx, 32, 1, "concurrency_403"))
	now := time.Now()

	for i := 0; i < 3; i++ {
		count, err := store.RecordFlap(ctx, 32, now.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
		require.Equal(t, i+1, count)
	}

	// 窗口外的历史事件自然滑出：8 天后的抖动只计 1 次。
	stale := now.Add(8 * 24 * time.Hour)
	count, err := store.RecordFlap(ctx, 32, stale)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestConcurrencyCapStoreSetPinnedRequiresRecord(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()

	require.ErrorIs(t, store.SetPinned(ctx, 41, true), ErrConcurrencyCapNotFound)

	require.NoError(t, store.SetCap(ctx, 41, 1, "concurrency_403"))
	require.NoError(t, store.SetPinned(ctx, 41, true))
	record, err := store.GetCap(ctx, 41)
	require.NoError(t, err)
	require.True(t, record.Pinned)
}

func TestConcurrencyCapStoreListRestrictedRepairsWriteThroughKeys(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()
	require.NoError(t, store.SetCap(ctx, 51, 1, "concurrency_403"))
	require.NoError(t, store.SetCap(ctx, 52, 3, "probe_pass"))
	delete(repo.redis, 51)

	records, err := store.ListRestricted(ctx)

	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, int64(51), records[0].AccountID)
	require.Equal(t, 1, repo.redis[51], "ListRestricted 必须顺手修复丢失的写穿键")
}

func TestConcurrencyCapStoreEffectiveCapsReadsBatchAndSkipsUnknown(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	// 61 有写穿键；62 有记录但键丢失（批量软路径不得凭空造值）；63 无记录。
	repo.redis[61] = 1
	repo.records[62] = &AccountConcurrencyCap{AccountID: 62, Cap: 2}
	store := newTestConcurrencyCapStore(repo)

	batchReader, ok := store.(ConcurrencyCapBatchReader)
	require.True(t, ok, "store 必须支持批量软路径读取")

	caps := batchReader.EffectiveCaps(context.Background(), []int64{61, 62, 63})

	require.Equal(t, map[int64]int{61: 1}, caps)
}

func TestConcurrencyCapStoreClearFlapEventsReleasesFuse(t *testing.T) {
	repo := newFakeConcurrencyCapRepo()
	store := newTestConcurrencyCapStore(repo)
	ctx := context.Background()
	require.NoError(t, store.SetCap(ctx, 71, 1, "concurrency_403"))
	now := time.Now()
	for i := 0; i < 3; i++ {
		_, err := store.RecordFlap(ctx, 71, now)
		require.NoError(t, err)
	}

	cleaner, ok := store.(ConcurrencyCapFuseCleaner)
	require.True(t, ok)
	require.NoError(t, cleaner.ClearFlapEvents(ctx, 71))

	record, err := store.GetCap(ctx, 71)
	require.NoError(t, err)
	require.Zero(t, record.FlapCount(concurrencyCapFlapWindowDays*24*time.Hour, now))
}
