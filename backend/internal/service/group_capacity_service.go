package service

import (
	"context"
	"time"
)

// GroupCapacitySummary holds aggregated capacity for a single group.
type GroupCapacitySummary struct {
	GroupID         int64 `json:"group_id"`
	ConcurrencyUsed int   `json:"concurrency_used"`
	ConcurrencyMax  int   `json:"concurrency_max"`
	SessionsUsed    int   `json:"sessions_used"`
	SessionsMax     int   `json:"sessions_max"`
	RPMUsed         int   `json:"rpm_used"`
	RPMMax          int   `json:"rpm_max"`
}

// GroupAccountCapacityRow is the lightweight account projection needed for
// capacity summary aggregation.
type GroupAccountCapacityRow struct {
	GroupID             int64
	AccountID           int64
	Concurrency         int
	Extra               map[string]any
	SessionWindowStart  *time.Time
	SessionWindowEnd    *time.Time
	SessionWindowStatus string
}

type groupCapacityActiveGroupIDLister interface {
	ListActiveIDs(ctx context.Context) ([]int64, error)
}

type groupCapacityAccountLister interface {
	ListSchedulableCapacityByGroupIDs(ctx context.Context, groupIDs []int64) ([]GroupAccountCapacityRow, error)
}

// GroupCapacityService aggregates per-group capacity from runtime data.
type GroupCapacityService struct {
	accountRepo        AccountRepository
	groupRepo          GroupRepository
	concurrencyService *ConcurrencyService
	sessionLimitCache  SessionLimitCache
	rpmCache           RPMCache
}

// NewGroupCapacityService creates a new GroupCapacityService.
func NewGroupCapacityService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	concurrencyService *ConcurrencyService,
	sessionLimitCache SessionLimitCache,
	rpmCache RPMCache,
) *GroupCapacityService {
	return &GroupCapacityService{
		accountRepo:        accountRepo,
		groupRepo:          groupRepo,
		concurrencyService: concurrencyService,
		sessionLimitCache:  sessionLimitCache,
		rpmCache:           rpmCache,
	}
}

// GetAllGroupCapacity returns capacity summary for all active groups.
func (s *GroupCapacityService) GetAllGroupCapacity(ctx context.Context) ([]GroupCapacitySummary, error) {
	groupIDs, err := s.listActiveGroupIDs(ctx)
	if err != nil {
		return nil, err
	}

	if lister, ok := s.accountRepo.(groupCapacityAccountLister); ok {
		return s.getGroupCapacitiesBatch(ctx, groupIDs, lister)
	}

	return s.getGroupCapacitiesSequential(ctx, groupIDs), nil
}

func (s *GroupCapacityService) listActiveGroupIDs(ctx context.Context) ([]int64, error) {
	if lister, ok := s.groupRepo.(groupCapacityActiveGroupIDLister); ok {
		return lister.ListActiveIDs(ctx)
	}

	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]int64, 0, len(groups))
	for i := range groups {
		groupIDs = append(groupIDs, groups[i].ID)
	}
	return groupIDs, nil
}

func (s *GroupCapacityService) getGroupCapacitiesSequential(ctx context.Context, groupIDs []int64) []GroupCapacitySummary {
	results := make([]GroupCapacitySummary, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		cap, err := s.getGroupCapacity(ctx, groupID)
		if err != nil {
			// Skip groups with errors, return partial results
			continue
		}
		cap.GroupID = groupID
		results = append(results, cap)
	}
	return results
}

type groupCapacityAccountRef struct {
	groupID   int64
	accountID int64
}

func (s *GroupCapacityService) getGroupCapacitiesBatch(ctx context.Context, groupIDs []int64, lister groupCapacityAccountLister) ([]GroupCapacitySummary, error) {
	results := make([]GroupCapacitySummary, len(groupIDs))
	groupIndex := make(map[int64]int, len(groupIDs))
	for i, groupID := range groupIDs {
		results[i].GroupID = groupID
		groupIndex[groupID] = i
	}
	if len(groupIDs) == 0 {
		return results, nil
	}

	rows, err := lister.ListSchedulableCapacityByGroupIDs(ctx, groupIDs)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return results, nil
	}

	refs := make([]groupCapacityAccountRef, 0, len(rows))
	seenGroupAccount := make(map[groupCapacityAccountRef]struct{}, len(rows))
	accountIDSet := make(map[int64]struct{}, len(rows))
	accountIDs := make([]int64, 0, len(rows))
	configuredConcurrency := make(map[int64]int, len(rows))
	sessionTimeouts := make(map[int64]time.Duration)

	for _, row := range rows {
		idx, ok := groupIndex[row.GroupID]
		if !ok || row.AccountID <= 0 {
			continue
		}

		ref := groupCapacityAccountRef{groupID: row.GroupID, accountID: row.AccountID}
		if _, ok := seenGroupAccount[ref]; ok {
			continue
		}
		seenGroupAccount[ref] = struct{}{}
		refs = append(refs, ref)

		if _, ok := accountIDSet[row.AccountID]; !ok {
			accountIDSet[row.AccountID] = struct{}{}
			accountIDs = append(accountIDs, row.AccountID)
			configuredConcurrency[row.AccountID] = row.Concurrency
		}

		acc := Account{
			ID:                  row.AccountID,
			Concurrency:         row.Concurrency,
			Extra:               row.Extra,
			SessionWindowStart:  row.SessionWindowStart,
			SessionWindowEnd:    row.SessionWindowEnd,
			SessionWindowStatus: row.SessionWindowStatus,
		}

		if maxSessions := acc.GetMaxSessions(); maxSessions > 0 {
			results[idx].SessionsMax += maxSessions
			timeout := time.Duration(acc.GetSessionIdleTimeoutMinutes()) * time.Minute
			if timeout <= 0 {
				timeout = 5 * time.Minute
			}
			sessionTimeouts[acc.ID] = timeout
		}

		if rpm := acc.GetBaseRPM(); rpm > 0 {
			results[idx].RPMMax += rpm
		}
	}

	if len(accountIDs) == 0 {
		return results, nil
	}

	concurrencyMap := map[int64]int{}
	if s.concurrencyService != nil {
		concurrencyMap, _ = s.concurrencyService.GetAccountConcurrencyBatch(ctx, accountIDs)
	}

	sessionAccountIDs := accountIDsForGroupsWithLimit(refs, groupIndex, results, func(summary GroupCapacitySummary) bool {
		return summary.SessionsMax > 0
	})
	var sessionsMap map[int64]int
	if len(sessionAccountIDs) > 0 && s.sessionLimitCache != nil {
		sessionsMap, _ = s.sessionLimitCache.GetActiveSessionCountBatch(ctx, sessionAccountIDs, sessionTimeouts)
	}

	rpmAccountIDs := accountIDsForGroupsWithLimit(refs, groupIndex, results, func(summary GroupCapacitySummary) bool {
		return summary.RPMMax > 0
	})
	var rpmMap map[int64]int
	if len(rpmAccountIDs) > 0 && s.rpmCache != nil {
		rpmMap, _ = s.rpmCache.GetRPMBatch(ctx, rpmAccountIDs)
	}

	// 容量上限按「有效并发」汇总：账号级 cap 夹帽，取不到 cap 时回落配置并发。
	// 否则「配置 vs 有效」在容量视图里仍然不可见（受限账号仍按 10 计容量）。
	effectiveConcurrency := s.effectiveAccountConcurrency(ctx, accountIDs, configuredConcurrency)

	for _, ref := range refs {
		idx := groupIndex[ref.groupID]
		results[idx].ConcurrencyMax += effectiveConcurrency[ref.accountID]
		results[idx].ConcurrencyUsed += concurrencyMap[ref.accountID]
		if sessionsMap != nil && results[idx].SessionsMax > 0 {
			results[idx].SessionsUsed += sessionsMap[ref.accountID]
		}
		if rpmMap != nil && results[idx].RPMMax > 0 {
			results[idx].RPMUsed += rpmMap[ref.accountID]
		}
	}
	return results, nil
}

// effectiveAccountConcurrency 汇总每个账号的有效并发上限：配置并发夹账号级 cap。
// 取不到 cap 记录的账号回落配置并发（非 CN 平台因此不受影响）。
func (s *GroupCapacityService) effectiveAccountConcurrency(ctx context.Context, accountIDs []int64, configured map[int64]int) map[int64]int {
	effective := make(map[int64]int, len(accountIDs))
	if s.concurrencyService == nil {
		for _, accountID := range accountIDs {
			effective[accountID] = configured[accountID]
		}
		return effective
	}
	loadReq := make([]AccountWithConcurrency, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		loadReq = append(loadReq, AccountWithConcurrency{ID: accountID, MaxConcurrency: configured[accountID]})
	}
	capped := s.concurrencyService.EffectiveAccountConcurrencyBatch(ctx, loadReq)
	for _, accountID := range accountIDs {
		if value, ok := capped[accountID]; ok && value > 0 {
			effective[accountID] = value
			continue
		}
		effective[accountID] = configured[accountID]
	}
	return effective
}

func accountIDsForGroupsWithLimit(refs []groupCapacityAccountRef, groupIndex map[int64]int, summaries []GroupCapacitySummary, include func(GroupCapacitySummary) bool) []int64 {
	seen := make(map[int64]struct{})
	accountIDs := make([]int64, 0)
	for _, ref := range refs {
		idx, ok := groupIndex[ref.groupID]
		if !ok || !include(summaries[idx]) {
			continue
		}
		if _, ok := seen[ref.accountID]; ok {
			continue
		}
		seen[ref.accountID] = struct{}{}
		accountIDs = append(accountIDs, ref.accountID)
	}
	return accountIDs
}

func (s *GroupCapacityService) getGroupCapacity(ctx context.Context, groupID int64) (GroupCapacitySummary, error) {
	accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, groupID)
	if err != nil {
		return GroupCapacitySummary{}, err
	}
	if len(accounts) == 0 {
		return GroupCapacitySummary{}, nil
	}

	// Collect account IDs and config values
	accountIDs := make([]int64, 0, len(accounts))
	configuredConcurrency := make(map[int64]int, len(accounts))
	sessionTimeouts := make(map[int64]time.Duration)
	var sessionsMax, rpmMax int

	for i := range accounts {
		acc := &accounts[i]
		accountIDs = append(accountIDs, acc.ID)
		configuredConcurrency[acc.ID] = acc.Concurrency

		if ms := acc.GetMaxSessions(); ms > 0 {
			sessionsMax += ms
			timeout := time.Duration(acc.GetSessionIdleTimeoutMinutes()) * time.Minute
			if timeout <= 0 {
				timeout = 5 * time.Minute
			}
			sessionTimeouts[acc.ID] = timeout
		}

		if rpm := acc.GetBaseRPM(); rpm > 0 {
			rpmMax += rpm
		}
	}

	// Batch query runtime data from Redis
	concurrencyMap, _ := s.concurrencyService.GetAccountConcurrencyBatch(ctx, accountIDs)

	var sessionsMap map[int64]int
	if sessionsMax > 0 && s.sessionLimitCache != nil {
		sessionsMap, _ = s.sessionLimitCache.GetActiveSessionCountBatch(ctx, accountIDs, sessionTimeouts)
	}

	var rpmMap map[int64]int
	if rpmMax > 0 && s.rpmCache != nil {
		rpmMap, _ = s.rpmCache.GetRPMBatch(ctx, accountIDs)
	}

	// Aggregate
	var concurrencyUsed, sessionsUsed, rpmUsed int
	// 容量上限按「有效并发」汇总：账号级 cap 夹帽，取不到 cap 时回落配置并发。
	effectiveConcurrency := s.effectiveAccountConcurrency(ctx, accountIDs, configuredConcurrency)
	concurrencyMax := 0
	for _, id := range accountIDs {
		concurrencyMax += effectiveConcurrency[id]
		concurrencyUsed += concurrencyMap[id]
		if sessionsMap != nil {
			sessionsUsed += sessionsMap[id]
		}
		if rpmMap != nil {
			rpmUsed += rpmMap[id]
		}
	}

	return GroupCapacitySummary{
		ConcurrencyUsed: concurrencyUsed,
		ConcurrencyMax:  concurrencyMax,
		SessionsUsed:    sessionsUsed,
		SessionsMax:     sessionsMax,
		RPMUsed:         rpmUsed,
		RPMMax:          rpmMax,
	}, nil
}
