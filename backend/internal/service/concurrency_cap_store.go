package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 账号级有效并发上限（cap）的存储口径，见 PLAN §三：
//   - DB 表 account_concurrency_caps 为唯一真源；
//   - Redis 键 cap:{account_id} 写穿，硬准入路径读它（多实例一致）；
//   - 进程内缓存只服务负载估算等软路径，TTL ≤5s（多实例下广播不可达）。
const (
	// concurrencyCapFlapWindowDays 是抖动（flap）计数的滚动窗口。
	concurrencyCapFlapWindowDays = 7

	defaultConcurrencyCapCacheTTL   = 5 * time.Second
	defaultConcurrencyCapRestricted = 1
	defaultConcurrencyCapCapMax     = 3
	defaultConcurrencyCapHoldHours  = 72
	defaultConcurrencyCapObserveHrs = 12

	// maxConcurrencyCapCacheEntries 限制进程缓存条目数：受限账号数量有限，
	// 批量读取的最坏情况是被查询账号总数，需要兜底淘汰。
	maxConcurrencyCapCacheEntries = 4096

	// concurrencyCapFlapReason 是「抖动先于降级到达」时补建记录的 reason。
	concurrencyCapFlapReason = "flap"
)

// ErrConcurrencyCapNotFound 表示账号没有 cap 记录（非错误态：等价于「不夹帽」）。
var ErrConcurrencyCapNotFound = errors.New("account concurrency cap not found")

// AccountConcurrencyCap 是账号级有效并发上限记录（契约 1）。
type AccountConcurrencyCap struct {
	AccountID    int64
	Cap          int
	Version      int64
	RestrictedAt time.Time
	NextProbeAt  time.Time
	Reason       string
	FlapEvents   []time.Time // 滚动 7 天窗口判定用
	Pinned       bool
	UpdatedAt    time.Time
}

// FlapCount 返回窗口内的事件数（窗口外的事件已由持久化层裁剪）。
func (c *AccountConcurrencyCap) FlapCount(window time.Duration, now time.Time) int {
	if c == nil {
		return 0
	}
	return countConcurrencyCapFlaps(c.FlapEvents, now, window)
}

// ConcurrencyCapStore 是账号级 cap 的读写入口（契约 1，签名与 PLAN-contracts.md 一致）。
type ConcurrencyCapStore interface {
	// EffectiveCap 语义（关键，防误伤全平台）：
	//   known=true            → 有记录，调用方夹帽 min(入参, cap)
	//   known=false, err=nil  → 无记录 → 调用方【不夹帽】
	//   err!=nil              → 存储不可用；store 内部先回落进程缓存，
	//                           缓存有旧值则按旧值返回 known=true, err=nil；
	//                           缓存也没有 → known=false, err=原错误（调用方不夹帽）
	EffectiveCap(ctx context.Context, accountID int64) (cap int, known bool, err error)

	GetCap(ctx context.Context, accountID int64) (*AccountConcurrencyCap, error) // 无记录返回 (nil, nil)
	SetCap(ctx context.Context, accountID int64, cap int, reason string) error   // DB UPSERT version+1 + Redis 写穿 + 进程缓存失效
	RecordFlap(ctx context.Context, accountID int64, at time.Time) (flapCount7d int, err error)
	SetPinned(ctx context.Context, accountID int64, pinned bool) error
	ListRestricted(ctx context.Context) ([]*AccountConcurrencyCap, error) // 探测调度器用：所有 cap<cap_max 或 next_probe_at 未到的记录
}

// ConcurrencyCapBatchReader 是 ConcurrencyCapStore 的可选扩展：批量读取有效 cap。
// 负载/容量等软路径一次遍历几十个账号，逐账号读 Redis 会放大往返，这里走单次 pipeline。
// 单独声明而不并入 ConcurrencyCapStore，是为了不改变其它 slice 按契约实现的桩。
type ConcurrencyCapBatchReader interface {
	// EffectiveCaps 返回 accountID → cap，仅含存在记录的账号；失败返回已命中的部分。
	EffectiveCaps(ctx context.Context, accountIDs []int64) map[int64]int
}

// ConcurrencyCapFuseCleaner 是 ConcurrencyCapStore 的可选扩展：管理接口解除熔断。
type ConcurrencyCapFuseCleaner interface {
	ClearFlapEvents(ctx context.Context, accountID int64) error
}

// ConcurrencyCapRepository 由 repository 层实现：DB 为唯一真源 + Redis 键写穿。
type ConcurrencyCapRepository interface {
	GetCap(ctx context.Context, accountID int64) (*AccountConcurrencyCap, error)
	UpsertCap(ctx context.Context, accountID int64, cap int, reason string, restrictedAt *time.Time, nextProbeAt *time.Time, preserveRestrictedAt bool) error
	SetNextProbeAt(ctx context.Context, accountID int64, at *time.Time, reason string) error
	SetPinned(ctx context.Context, accountID int64, pinned bool) error
	AppendFlapEvent(ctx context.Context, accountID int64, at time.Time, windowDays int) ([]time.Time, error)
	ClearFlapEvents(ctx context.Context, accountID int64) error
	ListRestricted(ctx context.Context, capMax int) ([]*AccountConcurrencyCap, error)
	GetCachedCap(ctx context.Context, accountID int64) (int, bool, error)
	GetCachedCaps(ctx context.Context, accountIDs []int64) (map[int64]int, error)
	SetCachedCap(ctx context.Context, accountID int64, cap int) error
	SetCachedCaps(ctx context.Context, caps map[int64]int) error
}

// ConcurrencyCapStoreConfig 是 store 的运行时参数（映射 config.Gateway.ConcurrencyCap）。
type ConcurrencyCapStoreConfig struct {
	Enabled      bool
	Restricted   int
	CapMax       int
	HoldHours    int
	ObserveHours int
	CacheTTL     time.Duration
}

func (c ConcurrencyCapStoreConfig) withDefaults() ConcurrencyCapStoreConfig {
	if c.Restricted <= 0 {
		c.Restricted = defaultConcurrencyCapRestricted
	}
	if c.CapMax < c.Restricted {
		c.CapMax = c.Restricted
	}
	if c.CapMax <= 0 {
		c.CapMax = defaultConcurrencyCapCapMax
	}
	if c.HoldHours <= 0 {
		c.HoldHours = defaultConcurrencyCapHoldHours
	}
	if c.ObserveHours <= 0 {
		c.ObserveHours = defaultConcurrencyCapObserveHrs
	}
	if c.CacheTTL <= 0 || c.CacheTTL > defaultConcurrencyCapCacheTTL {
		c.CacheTTL = defaultConcurrencyCapCacheTTL
	}
	return c
}

type concurrencyCapCacheEntry struct {
	cap       int
	expiresAt time.Time
}

type concurrencyCapStore struct {
	repo ConcurrencyCapRepository
	cfg  ConcurrencyCapStoreConfig

	mu    sync.RWMutex
	cache map[int64]concurrencyCapCacheEntry
}

// NewConcurrencyCapStore 创建账号级 cap 存储。
// repo 为 nil 时 store 退化为「永远无记录」：不影响调用方，只是不夹帽。
func NewConcurrencyCapStore(repo ConcurrencyCapRepository, cfg ConcurrencyCapStoreConfig) ConcurrencyCapStore {
	return &concurrencyCapStore{
		repo:  repo,
		cfg:   cfg.withDefaults(),
		cache: make(map[int64]concurrencyCapCacheEntry),
	}
}

func (s *concurrencyCapStore) EffectiveCap(ctx context.Context, accountID int64) (int, bool, error) {
	if s == nil || s.repo == nil || accountID <= 0 || !s.cfg.Enabled {
		return 0, false, nil
	}
	now := time.Now()
	cap, found, err := s.repo.GetCachedCap(ctx, accountID)
	if err == nil && found {
		s.storeCachedCap(accountID, cap, now)
		return cap, true, nil
	}
	// Redis 键不存在或不可用：进程缓存是唯一可信的回落点。
	if cached, ok := s.cachedCap(accountID, now); ok {
		return cached, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

func (s *concurrencyCapStore) EffectiveCaps(ctx context.Context, accountIDs []int64) map[int64]int {
	if s == nil || s.repo == nil || len(accountIDs) == 0 || !s.cfg.Enabled {
		return nil
	}
	now := time.Now()
	caps := make(map[int64]int, len(accountIDs))
	missing := make([]int64, 0, len(accountIDs))
	seen := make(map[int64]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID <= 0 {
			continue
		}
		if _, duplicate := seen[accountID]; duplicate {
			continue
		}
		seen[accountID] = struct{}{}
		if cached, ok := s.cachedCap(accountID, now); ok {
			caps[accountID] = cached
			continue
		}
		missing = append(missing, accountID)
	}
	if len(missing) == 0 {
		return caps
	}
	fetched, err := s.repo.GetCachedCaps(ctx, missing)
	if err != nil {
		// 软路径失败即视为无记录：宁可不夹帽，也不能阻塞负载计算。
		logger.LegacyPrintf("service.concurrency_cap", "Warning: batch read concurrency caps failed: %v", err)
		return caps
	}
	for accountID, cap := range fetched {
		caps[accountID] = cap
		s.storeCachedCap(accountID, cap, now)
	}
	return caps
}

func (s *concurrencyCapStore) GetCap(ctx context.Context, accountID int64) (*AccountConcurrencyCap, error) {
	if s == nil || s.repo == nil || accountID <= 0 {
		return nil, nil
	}
	record, err := s.repo.GetCap(ctx, accountID)
	if err != nil || record == nil {
		return record, err
	}
	// 读真源顺带回填进程缓存：Redis 抖动时硬路径仍能按最近一次真值夹帽。
	s.storeCachedCap(accountID, record.Cap, time.Now())
	return record, nil
}

// SetCap 写入有效并发上限。
//
// 窗口口径（PLAN §四 的阶梯）：
//   - cap >= cap_max：已回到阶梯上限 → 清除 restricted_at/next_probe_at（不再受限、不再探测）；
//   - cap <= restricted：受限档 → next_probe_at = now + hold_hours；
//   - 其余（中间档）：观察档 → next_probe_at = now + observe_hours。
//
// restricted_at 只前进（保留首次受限时间），next_probe_at 每次调用按当前档重排：
// 探测失败后的「保持 cap、重排 72h」依赖后者。
func (s *concurrencyCapStore) SetCap(ctx context.Context, accountID int64, cap int, reason string) error {
	if s == nil || s.repo == nil || accountID <= 0 {
		return nil
	}
	if cap <= 0 {
		return errors.New("concurrency cap must be positive")
	}
	now := time.Now()
	restrictedAt, nextProbeAt := s.capProbeWindow(now, cap)
	if err := s.repo.UpsertCap(ctx, accountID, cap, reason, restrictedAt, nextProbeAt, restrictedAt != nil); err != nil {
		return err
	}
	s.storeCachedCap(accountID, cap, now)
	if err := s.repo.SetCachedCap(ctx, accountID, cap); err != nil {
		logger.LegacyPrintf("service.concurrency_cap", "Warning: concurrency cap write-through failed for account %d: %v", accountID, err)
	}
	return nil
}

func (s *concurrencyCapStore) RecordFlap(ctx context.Context, accountID int64, at time.Time) (int, error) {
	if s == nil || s.repo == nil || accountID <= 0 {
		return 0, nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	events, err := s.repo.AppendFlapEvent(ctx, accountID, at, concurrencyCapFlapWindowDays)
	if errors.Is(err, ErrConcurrencyCapNotFound) {
		// 抖动只可能来自并发受限或刚撞并发限流的账号；若记录缺失就补一条受限记录，
		// 否则熔断计数会静默丢失（漏计比多计保守档更危险）。
		if setErr := s.SetCap(ctx, accountID, s.cfg.Restricted, concurrencyCapFlapReason); setErr != nil {
			return 0, setErr
		}
		events, err = s.repo.AppendFlapEvent(ctx, accountID, at, concurrencyCapFlapWindowDays)
	}
	if err != nil {
		return 0, err
	}
	return countConcurrencyCapFlaps(events, at, concurrencyCapFlapWindowDays*24*time.Hour), nil
}

// SetNextProbeAt 精确重排下次探测时间，cap 与受限状态不变。
// 探测编排用它表达「cap 不变但必须重排」：不确定 → +1h、占槽推迟 → +10s、熔断保持。
// 与 SetCap 的区别：SetCap 按 cap 档位套用 hold/observe 窗口，本方法写调用方给的时间。
// at 为零值表示清除（回到「不需要探测」）。
func (s *concurrencyCapStore) SetNextProbeAt(ctx context.Context, accountID int64, at time.Time, reason string) error {
	if s == nil || s.repo == nil || accountID <= 0 {
		return nil
	}
	var nextProbeAt *time.Time
	if !at.IsZero() {
		nextProbeAt = &at
	}
	return s.repo.SetNextProbeAt(ctx, accountID, nextProbeAt, reason)
}

func (s *concurrencyCapStore) SetPinned(ctx context.Context, accountID int64, pinned bool) error {
	if s == nil || s.repo == nil || accountID <= 0 {
		return nil
	}
	return s.repo.SetPinned(ctx, accountID, pinned)
}

func (s *concurrencyCapStore) ListRestricted(ctx context.Context) ([]*AccountConcurrencyCap, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	records, err := s.repo.ListRestricted(ctx, s.cfg.CapMax)
	if err != nil {
		return nil, err
	}
	// 顺带修复写穿副本：探测调度器周期性调用本方法，键丢失/实例重启后能自愈。
	caps := make(map[int64]int, len(records))
	now := time.Now()
	for _, record := range records {
		if record == nil || record.AccountID <= 0 {
			continue
		}
		caps[record.AccountID] = record.Cap
		s.storeCachedCap(record.AccountID, record.Cap, now)
	}
	if len(caps) > 0 {
		if err := s.repo.SetCachedCaps(ctx, caps); err != nil {
			logger.LegacyPrintf("service.concurrency_cap", "Warning: refresh concurrency cap write-through keys failed: %v", err)
		}
	}
	return records, nil
}

func (s *concurrencyCapStore) ClearFlapEvents(ctx context.Context, accountID int64) error {
	if s == nil || s.repo == nil || accountID <= 0 {
		return nil
	}
	return s.repo.ClearFlapEvents(ctx, accountID)
}

// capProbeWindow 返回该 cap 档位对应的受限起始与下次探测时间。
func (s *concurrencyCapStore) capProbeWindow(now time.Time, cap int) (*time.Time, *time.Time) {
	if cap >= s.cfg.CapMax {
		return nil, nil
	}
	restrictedAt := now
	horizon := time.Duration(s.cfg.HoldHours) * time.Hour
	if cap > s.cfg.Restricted {
		horizon = time.Duration(s.cfg.ObserveHours) * time.Hour
	}
	nextProbeAt := now.Add(horizon)
	return &restrictedAt, &nextProbeAt
}

func (s *concurrencyCapStore) cachedCap(accountID int64, now time.Time) (int, bool) {
	s.mu.RLock()
	entry, ok := s.cache[accountID]
	s.mu.RUnlock()
	if !ok || !now.Before(entry.expiresAt) {
		return 0, false
	}
	return entry.cap, true
}

func (s *concurrencyCapStore) storeCachedCap(accountID int64, cap int, now time.Time) {
	s.mu.Lock()
	if s.cache == nil {
		s.cache = make(map[int64]concurrencyCapCacheEntry)
	}
	if len(s.cache) >= maxConcurrencyCapCacheEntries {
		for key, entry := range s.cache {
			if !now.Before(entry.expiresAt) {
				delete(s.cache, key)
			}
		}
		for len(s.cache) >= maxConcurrencyCapCacheEntries {
			for key := range s.cache {
				delete(s.cache, key)
				break
			}
		}
	}
	s.cache[accountID] = concurrencyCapCacheEntry{cap: cap, expiresAt: now.Add(s.cfg.CacheTTL)}
	s.mu.Unlock()
}

func countConcurrencyCapFlaps(events []time.Time, now time.Time, window time.Duration) int {
	if len(events) == 0 {
		return 0
	}
	cutoff := now.Add(-window)
	count := 0
	for _, event := range events {
		if !event.Before(cutoff) {
			count++
		}
	}
	return count
}
