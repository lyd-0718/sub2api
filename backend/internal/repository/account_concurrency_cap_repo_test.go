//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newConcurrencyCapRepoTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func newConcurrencyCapRepoTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, server
}

func concurrencyCapRowColumns() []string {
	return []string{
		"account_id", "cap", "version", "restricted_at", "next_probe_at",
		"reason", "flap_events", "pinned", "updated_at",
	}
}

func TestAccountConcurrencyCapRepositoryGetCapReturnsNilWhenAbsent(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	mock.ExpectQuery(`FROM account_concurrency_caps`).
		WithArgs(int64(5)).
		WillReturnRows(sqlmock.NewRows(concurrencyCapRowColumns()))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	record, err := repo.GetCap(context.Background(), 5)

	require.NoError(t, err)
	require.Nil(t, record)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryGetCapDecodesRow(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	restrictedAt := time.Date(2026, time.September, 15, 8, 0, 0, 0, time.UTC)
	nextProbeAt := restrictedAt.Add(72 * time.Hour)
	updatedAt := restrictedAt.Add(time.Minute)
	mock.ExpectQuery(`FROM account_concurrency_caps`).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows(concurrencyCapRowColumns()).AddRow(
			int64(9), 1, int64(4), restrictedAt, nextProbeAt,
			"concurrency_403", []byte(`["2026-09-15T08:00:00Z"]`), true, updatedAt,
		))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	record, err := repo.GetCap(context.Background(), 9)

	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, int64(9), record.AccountID)
	require.Equal(t, 1, record.Cap)
	require.Equal(t, int64(4), record.Version)
	require.True(t, restrictedAt.Equal(record.RestrictedAt))
	require.True(t, nextProbeAt.Equal(record.NextProbeAt))
	require.Equal(t, "concurrency_403", record.Reason)
	require.Len(t, record.FlapEvents, 1)
	require.True(t, record.Pinned)
	require.True(t, updatedAt.Equal(record.UpdatedAt))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryGetCapToleratesNullColumns(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	mock.ExpectQuery(`FROM account_concurrency_caps`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows(concurrencyCapRowColumns()).AddRow(
			int64(10), 3, int64(2), nil, nil, nil, []byte(`[]`), false, time.Now(),
		))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	record, err := repo.GetCap(context.Background(), 10)

	require.NoError(t, err)
	require.NotNil(t, record)
	require.True(t, record.RestrictedAt.IsZero(), "回到阶梯上限的记录不再有受限起始")
	require.True(t, record.NextProbeAt.IsZero())
	require.Empty(t, record.Reason)
	require.Empty(t, record.FlapEvents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryUpsertCapPreservesVersioningFlag(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	restrictedAt := time.Date(2026, time.September, 15, 8, 0, 0, 0, time.UTC)
	nextProbeAt := restrictedAt.Add(72 * time.Hour)
	mock.ExpectExec(`INSERT INTO account_concurrency_caps`).
		WithArgs(int64(11), 1, restrictedAt, nextProbeAt, "concurrency_403", true).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	err := repo.UpsertCap(context.Background(), 11, 1, "concurrency_403", &restrictedAt, &nextProbeAt, true)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositorySetNextProbeAt(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	at := time.Date(2026, time.September, 15, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec(`UPDATE account_concurrency_caps`).
		WithArgs(int64(18), &at, "probe_inconclusive").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	require.NoError(t, repo.SetNextProbeAt(context.Background(), 18, &at, "probe_inconclusive"))
	require.NoError(t, mock.ExpectationsWereMet())

	missingDB, missingMock := newConcurrencyCapRepoTestDB(t)
	missingMock.ExpectExec(`UPDATE account_concurrency_caps`).
		WithArgs(int64(19), (*time.Time)(nil), "cap_max").
		WillReturnResult(sqlmock.NewResult(0, 0))
	missingRepo := NewAccountConcurrencyCapRepository(missingDB, nil)
	require.ErrorIs(t, missingRepo.SetNextProbeAt(context.Background(), 19, nil, "cap_max"), service.ErrConcurrencyCapNotFound)
	require.NoError(t, missingMock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositorySetPinnedReportsMissingRecord(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	mock.ExpectExec(`UPDATE account_concurrency_caps`).
		WithArgs(int64(12), true).
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	err := repo.SetPinned(context.Background(), 12, true)

	require.ErrorIs(t, err, service.ErrConcurrencyCapNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryAppendFlapEventReturnsWindowEvents(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	at := time.Date(2026, time.September, 15, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`UPDATE account_concurrency_caps`).
		WithArgs(int64(13), at, 7).
		WillReturnRows(sqlmock.NewRows([]string{"flap_events"}).AddRow(
			[]byte(`["2026-09-10T08:00:00Z","2026-09-15T08:00:00Z"]`),
		))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	events, err := repo.AppendFlapEvent(context.Background(), 13, at, 7)

	require.NoError(t, err)
	require.Len(t, events, 2)
	require.True(t, time.Date(2026, time.September, 10, 8, 0, 0, 0, time.UTC).Equal(events[0]))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryAppendFlapEventReportsMissingRecord(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	at := time.Now()
	mock.ExpectQuery(`UPDATE account_concurrency_caps`).
		WithArgs(int64(14), at, 7).
		WillReturnRows(sqlmock.NewRows([]string{"flap_events"}))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	_, err := repo.AppendFlapEvent(context.Background(), 14, at, 7)

	require.ErrorIs(t, err, service.ErrConcurrencyCapNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryListRestrictedReturnsRecords(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	mock.ExpectQuery(`WHERE cap < \$1 OR next_probe_at IS NOT NULL`).
		WithArgs(3).
		WillReturnRows(sqlmock.NewRows(concurrencyCapRowColumns()).
			AddRow(int64(15), 1, int64(1), time.Now(), time.Now(), "concurrency_403", []byte(`[]`), false, time.Now()).
			AddRow(int64(16), 2, int64(2), time.Now(), time.Now(), "probe_pass", []byte(`[]`), false, time.Now()))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	records, err := repo.ListRestricted(context.Background(), 3)

	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, int64(15), records[0].AccountID)
	require.Equal(t, int64(16), records[1].AccountID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryClearFlapEventsIsIdempotent(t *testing.T) {
	db, mock := newConcurrencyCapRepoTestDB(t)
	mock.ExpectExec(`SET flap_events = '\[\]'::jsonb`).
		WithArgs(int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewAccountConcurrencyCapRepository(db, nil)
	require.NoError(t, repo.ClearFlapEvents(context.Background(), 17))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountConcurrencyCapRepositoryWriteThroughRoundTrip(t *testing.T) {
	client, server := newConcurrencyCapRepoTestRedis(t)
	repo := NewAccountConcurrencyCapRepository(nil, client)
	ctx := context.Background()

	require.NoError(t, repo.SetCachedCap(ctx, 21, 1))
	value, found, err := repo.GetCachedCap(ctx, 21)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 1, value)

	_, found, err = repo.GetCachedCap(ctx, 22)
	require.NoError(t, err)
	require.False(t, found, "无键 = 无记录，硬路径据此不夹帽")

	require.NoError(t, repo.SetCachedCaps(ctx, map[int64]int{23: 2, 24: 3}))
	caps, err := repo.GetCachedCaps(ctx, []int64{21, 23, 24, 25})
	require.NoError(t, err)
	require.Equal(t, map[int64]int{21: 1, 23: 2, 24: 3}, caps)

	// 写穿键不带 TTL：过期会让受限账号静默回到「无记录 = 不夹帽」。
	server.FastForward(30 * 24 * time.Hour)
	value, found, err = repo.GetCachedCap(ctx, 21)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 1, value)
}

func TestAccountConcurrencyCapRepositoryGetCachedCapRejectsGarbage(t *testing.T) {
	client, _ := newConcurrencyCapRepoTestRedis(t)
	repo := NewAccountConcurrencyCapRepository(nil, client)
	ctx := context.Background()
	require.NoError(t, client.Set(ctx, concurrencyCapRedisKey(31), "not-a-number", 0).Err())

	_, _, err := repo.GetCachedCap(ctx, 31)

	require.Error(t, err, "脏值必须报错，让调用方回落进程缓存而不是当成无记录")
}
