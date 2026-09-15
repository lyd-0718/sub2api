package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// concurrencyCapKeyPrefix 是账号级有效并发上限（cap）的 Redis 键前缀。
// 形如 cap:{account_id}，由写路径写穿，硬准入路径原子读取以保证多实例一致。
const concurrencyCapKeyPrefix = "cap:"

const concurrencyCapSelectColumns = `account_id, cap, version, restricted_at, next_probe_at, reason,
       COALESCE(flap_events, '[]'::jsonb), pinned, updated_at`

// accountConcurrencyCapRepository 持久化账号级有效并发上限。
//
// 一致性口径（与 PLAN 的 §三 一致）：
//   - DB 表 account_concurrency_caps 是唯一真源，version 每次写入 +1；
//   - Redis 键 cap:{account_id} 是写穿副本，硬准入路径只读它；
//   - 键不设 TTL：TTL 过期会让受限账号静默回到「无记录 = 不夹帽」。
//     键丢失由 ListRestricted 的写穿修复（探测调度器周期性调用）。
type accountConcurrencyCapRepository struct {
	db  *sql.DB
	rdb *redis.Client
}

// NewAccountConcurrencyCapRepository 创建 cap 仓储（DB 真源 + Redis 写穿）。
func NewAccountConcurrencyCapRepository(db *sql.DB, rdb *redis.Client) service.ConcurrencyCapRepository {
	return &accountConcurrencyCapRepository{db: db, rdb: rdb}
}

func concurrencyCapRedisKey(accountID int64) string {
	return concurrencyCapKeyPrefix + strconv.FormatInt(accountID, 10)
}

// GetCap 读取账号的 cap 记录；无记录返回 (nil, nil)。
func (r *accountConcurrencyCapRepository) GetCap(ctx context.Context, accountID int64) (*service.AccountConcurrencyCap, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil concurrency cap database")
	}
	if accountID <= 0 {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT `+concurrencyCapSelectColumns+`
		FROM account_concurrency_caps
		WHERE account_id = $1
	`, accountID)
	record, err := scanConcurrencyCapRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query concurrency cap: %w", err)
	}
	return record, nil
}

// UpsertCap 写入 cap（version+1）。
//
// preserveRestrictedAt 为真时保留已有 restricted_at（首次受限时间不回退），
// 为假时写入传入值（含 NULL，用于「已回到阶梯上限」的清零）。
func (r *accountConcurrencyCapRepository) UpsertCap(
	ctx context.Context,
	accountID int64,
	cap int,
	reason string,
	restrictedAt *time.Time,
	nextProbeAt *time.Time,
	preserveRestrictedAt bool,
) error {
	if r == nil || r.db == nil {
		return errors.New("nil concurrency cap database")
	}
	if accountID <= 0 {
		return errors.New("invalid concurrency cap account id")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO account_concurrency_caps (account_id, cap, version, restricted_at, next_probe_at, reason, updated_at)
		VALUES ($1, $2, 1, $3, $4, $5, NOW())
		ON CONFLICT (account_id) DO UPDATE
		SET cap = EXCLUDED.cap,
		    version = account_concurrency_caps.version + 1,
		    restricted_at = CASE
		        WHEN $6 THEN account_concurrency_caps.restricted_at
		        ELSE EXCLUDED.restricted_at
		    END,
		    next_probe_at = EXCLUDED.next_probe_at,
		    reason = EXCLUDED.reason,
		    updated_at = NOW()
	`, accountID, cap, restrictedAt, nextProbeAt, reason, preserveRestrictedAt)
	if err != nil {
		return fmt.Errorf("upsert concurrency cap: %w", err)
	}
	return nil
}

// SetNextProbeAt 精确重排下次探测时间（cap 不变），at 为 nil 表示清除。
// 供探测编排表达「cap 不变但必须重排」（不确定 1h / 占槽推迟 10s / 熔断保持）。
// 无记录时返回 service.ErrConcurrencyCapNotFound。
func (r *accountConcurrencyCapRepository) SetNextProbeAt(ctx context.Context, accountID int64, at *time.Time, reason string) error {
	if r == nil || r.db == nil {
		return errors.New("nil concurrency cap database")
	}
	if accountID <= 0 {
		return errors.New("invalid concurrency cap account id")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE account_concurrency_caps
		SET next_probe_at = $2, reason = $3, version = version + 1, updated_at = NOW()
		WHERE account_id = $1
	`, accountID, at, reason)
	if err != nil {
		return fmt.Errorf("reschedule concurrency cap probe: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reschedule concurrency cap probe rows: %w", err)
	}
	if affected == 0 {
		return service.ErrConcurrencyCapNotFound
	}
	return nil
}

// SetPinned 人工置位/清位；无记录时返回 service.ErrConcurrencyCapNotFound。
func (r *accountConcurrencyCapRepository) SetPinned(ctx context.Context, accountID int64, pinned bool) error {
	if r == nil || r.db == nil {
		return errors.New("nil concurrency cap database")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE account_concurrency_caps
		SET pinned = $2, version = version + 1, updated_at = NOW()
		WHERE account_id = $1
	`, accountID, pinned)
	if err != nil {
		return fmt.Errorf("set concurrency cap pinned: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set concurrency cap pinned rows: %w", err)
	}
	if affected == 0 {
		return service.ErrConcurrencyCapNotFound
	}
	return nil
}

// AppendFlapEvent 追加一条抖动事件并裁剪窗口外的历史，返回裁剪后的全部事件。
// 无记录时返回 service.ErrConcurrencyCapNotFound（由调用方决定是否补记录）。
func (r *accountConcurrencyCapRepository) AppendFlapEvent(ctx context.Context, accountID int64, at time.Time, windowDays int) ([]time.Time, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil concurrency cap database")
	}
	if windowDays <= 0 {
		windowDays = 7
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE account_concurrency_caps
		SET flap_events = COALESCE((
		        SELECT jsonb_agg(elem ORDER BY (elem #>> '{}')::timestamptz)
		        FROM jsonb_array_elements(
		                account_concurrency_caps.flap_events || jsonb_build_array(to_jsonb($2::timestamptz))
		             ) AS elem
		        WHERE (elem #>> '{}')::timestamptz > $2::timestamptz - ($3::int * INTERVAL '1 day')
		    ), '[]'::jsonb),
		    version = version + 1,
		    updated_at = NOW()
		WHERE account_id = $1
		RETURNING flap_events
	`, accountID, at, windowDays)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrConcurrencyCapNotFound
		}
		return nil, fmt.Errorf("append concurrency cap flap event: %w", err)
	}
	return decodeConcurrencyCapFlapEvents(raw)
}

// ClearFlapEvents 清空抖动事件（解除熔断）。无记录视为已是目标状态，返回 nil。
func (r *accountConcurrencyCapRepository) ClearFlapEvents(ctx context.Context, accountID int64) error {
	if r == nil || r.db == nil {
		return errors.New("nil concurrency cap database")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE account_concurrency_caps
		SET flap_events = '[]'::jsonb, version = version + 1, updated_at = NOW()
		WHERE account_id = $1
	`, accountID)
	if err != nil {
		return fmt.Errorf("clear concurrency cap flap events: %w", err)
	}
	return nil
}

// ListRestricted 返回全部「受限中或仍需探测」的记录，供探测调度器按时间过滤。
func (r *accountConcurrencyCapRepository) ListRestricted(ctx context.Context, capMax int) ([]*service.AccountConcurrencyCap, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil concurrency cap database")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+concurrencyCapSelectColumns+`
		FROM account_concurrency_caps
		WHERE cap < $1 OR next_probe_at IS NOT NULL
		ORDER BY next_probe_at NULLS LAST, account_id
	`, capMax)
	if err != nil {
		return nil, fmt.Errorf("list restricted concurrency caps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]*service.AccountConcurrencyCap, 0, 8)
	for rows.Next() {
		record, scanErr := scanConcurrencyCapRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan restricted concurrency cap: %w", scanErr)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate restricted concurrency caps: %w", err)
	}
	return records, nil
}

// GetCachedCap 读取 Redis 写穿副本；键不存在返回 (0, false, nil)。
func (r *accountConcurrencyCapRepository) GetCachedCap(ctx context.Context, accountID int64) (int, bool, error) {
	if r == nil || r.rdb == nil {
		return 0, false, errors.New("nil concurrency cap redis client")
	}
	key := concurrencyCapRedisKey(accountID)
	raw, err := r.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(raw))
	if parseErr != nil {
		// 脏值按「键不可信」处理：返回错误让调用方回落进程缓存，而不是当成无记录。
		return 0, false, fmt.Errorf("parse concurrency cap key %s: %w", key, parseErr)
	}
	return value, true, nil
}

// GetCachedCaps 批量读取写穿副本（单次 pipeline，兼容 Redis Cluster）。
// 返回的 map 只含命中的账号；单个键错误不阻塞其余键（写穿副本是硬路径缓存，失败即不夹帽）。
func (r *accountConcurrencyCapRepository) GetCachedCaps(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	if r == nil || r.rdb == nil {
		return nil, errors.New("nil concurrency cap redis client")
	}
	if len(accountIDs) == 0 {
		return map[int64]int{}, nil
	}
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		cmds = append(cmds, pipe.Get(ctx, concurrencyCapRedisKey(accountID)))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("pipeline exec concurrency caps: %w", err)
	}
	caps := make(map[int64]int, len(accountIDs))
	for i, cmd := range cmds {
		raw, err := cmd.Result()
		if err != nil {
			continue
		}
		value, parseErr := strconv.Atoi(strings.TrimSpace(raw))
		if parseErr != nil {
			continue
		}
		caps[accountIDs[i]] = value
	}
	return caps, nil
}

// SetCachedCap 写穿单个账号的 cap。
func (r *accountConcurrencyCapRepository) SetCachedCap(ctx context.Context, accountID int64, cap int) error {
	if r == nil || r.rdb == nil {
		return errors.New("nil concurrency cap redis client")
	}
	if accountID <= 0 {
		return errors.New("invalid concurrency cap account id")
	}
	return r.rdb.Set(ctx, concurrencyCapRedisKey(accountID), strconv.Itoa(cap), 0).Err()
}

// DeleteCachedCap 删除单个账号的 cap 缓存键。用于「毕业（cap≥cap_max）写入 Redis 失败」
// 的兜底：毕业记录退出 ListRestricted 周期修复集，残留旧值会永久夹帽；删键后读侧
// miss → 不夹帽，与 DB 真源一致（宁可不夹帽，不留旧值）。
func (r *accountConcurrencyCapRepository) DeleteCachedCap(ctx context.Context, accountID int64) error {
	if r == nil || r.rdb == nil {
		return errors.New("nil concurrency cap redis client")
	}
	if accountID <= 0 {
		return errors.New("invalid concurrency cap account id")
	}
	return r.rdb.Del(ctx, concurrencyCapRedisKey(accountID)).Err()
}

// SetCachedCaps 批量写穿（ListRestricted 修复路径用；单次 pipeline）。
func (r *accountConcurrencyCapRepository) SetCachedCaps(ctx context.Context, caps map[int64]int) error {
	if r == nil || r.rdb == nil {
		return errors.New("nil concurrency cap redis client")
	}
	if len(caps) == 0 {
		return nil
	}
	pipe := r.rdb.Pipeline()
	for accountID, cap := range caps {
		if accountID <= 0 {
			continue
		}
		pipe.Set(ctx, concurrencyCapRedisKey(accountID), strconv.Itoa(cap), 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("pipeline write concurrency caps: %w", err)
	}
	return nil
}

type concurrencyCapScanner interface {
	Scan(dest ...any) error
}

func scanConcurrencyCapRow(row concurrencyCapScanner) (*service.AccountConcurrencyCap, error) {
	var (
		record       service.AccountConcurrencyCap
		restrictedAt sql.NullTime
		nextProbeAt  sql.NullTime
		reason       sql.NullString
		flapEvents   []byte
	)
	if err := row.Scan(
		&record.AccountID,
		&record.Cap,
		&record.Version,
		&restrictedAt,
		&nextProbeAt,
		&reason,
		&flapEvents,
		&record.Pinned,
		&record.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if restrictedAt.Valid {
		record.RestrictedAt = restrictedAt.Time
	}
	if nextProbeAt.Valid {
		record.NextProbeAt = nextProbeAt.Time
	}
	if reason.Valid {
		record.Reason = reason.String
	}
	events, err := decodeConcurrencyCapFlapEvents(flapEvents)
	if err != nil {
		return nil, err
	}
	record.FlapEvents = events
	return &record, nil
}

func decodeConcurrencyCapFlapEvents(raw []byte) ([]time.Time, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var events []time.Time
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("decode concurrency cap flap events: %w", err)
	}
	return events, nil
}
