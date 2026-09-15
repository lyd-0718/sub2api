package handler

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 配置缺失（cfg 为 nil 或配置未加载）时的安全默认值，与 viper 默认值一致。
const (
	adminConcurrencyCapDefaultCapMax            = 3
	adminConcurrencyCapDefaultFuseFlapThreshold = 3
	adminConcurrencyCapReasonAdmin              = "admin"
	// adminConcurrencyCapMaxAllowed 是管理接口允许写入的 cap 上限，防止误填把账号锁到荒谬档位。
	adminConcurrencyCapMaxAllowed = 10000
)

// AdminConcurrencyCapHandler 暴露账号级有效并发上限（cap）的管理接口。
// 只读展示 + pinned/解熔断人工干预；自动升降级由探测服务负责。
type AdminConcurrencyCapHandler struct {
	store    service.ConcurrencyCapStore
	accounts service.AdminService
	cfg      *config.Config
}

// NewAdminConcurrencyCapHandler 创建管理接口处理器。
func NewAdminConcurrencyCapHandler(
	store service.ConcurrencyCapStore,
	accounts service.AdminService,
	cfg *config.Config,
) *AdminConcurrencyCapHandler {
	return &AdminConcurrencyCapHandler{store: store, accounts: accounts, cfg: cfg}
}

// adminConcurrencyCapResponse 是 GET/PUT 的统一响应体。
type adminConcurrencyCapResponse struct {
	AccountID    int64  `json:"account_id"`
	Platform     string `json:"platform"`
	CapMax       int    `json:"cap_max"`
	Known        bool   `json:"known"`
	Cap          *int   `json:"cap"`
	Restricted   bool   `json:"restricted"`
	Pinned       bool   `json:"pinned"`
	Reason       string `json:"reason"`
	RestrictedAt string `json:"restricted_at,omitempty"`
	NextProbeAt  string `json:"next_probe_at,omitempty"`
	// 配置并发是 accounts.concurrency，有效并发是 min(配置并发, cap)。
	ConfiguredConcurrency int    `json:"configured_concurrency"`
	EffectiveConcurrency  int    `json:"effective_concurrency"`
	FlapCount7d           int    `json:"flap_count_7d"`
	FuseFlapThreshold     int    `json:"fuse_flap_threshold"`
	Fused                 bool   `json:"fused"`
	Version               int64  `json:"version"`
	UpdatedAt             string `json:"updated_at,omitempty"`
}

// adminConcurrencyCapUpdateRequest 是 PUT 的请求体；字段全部可选，nil 表示不改。
type adminConcurrencyCapUpdateRequest struct {
	Cap       *int  `json:"cap"`
	Pinned    *bool `json:"pinned"`
	ClearFuse bool  `json:"clear_fuse"`
}

// Get 返回账号的并发上限治理状态。
// GET /api/v1/admin/accounts/:id/concurrency-cap
func (h *AdminConcurrencyCapHandler) Get(c *gin.Context) {
	if h == nil {
		response.InternalError(c, "concurrency cap store is unavailable")
		return
	}
	account, record, ok := h.loadAccountAndRecord(c)
	if !ok {
		return
	}
	response.Success(c, h.buildResponse(account, record))
}

// Update 人工干预：设置 cap、置位/清位 pinned、解除熔断。
// PUT /api/v1/admin/accounts/:id/concurrency-cap
func (h *AdminConcurrencyCapHandler) Update(c *gin.Context) {
	if h == nil {
		response.InternalError(c, "concurrency cap store is unavailable")
		return
	}
	var req adminConcurrencyCapUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	account, record, ok := h.loadAccountAndRecord(c)
	if !ok {
		return
	}
	// 平台闸：非 CN 账号建 cap 记录没有自动回升出口（探测调度器跳过非 CN），
	// 会被永久夹帽且无告警——直接拒绝（PLAN §3.3「非 CN 不受约束」落到实现）。
	if !account.IsCNProvider() {
		response.BadRequest(c, "concurrency cap only applies to CN provider accounts")
		return
	}
	ctx := c.Request.Context()

	if req.Cap != nil {
		if *req.Cap <= 0 || *req.Cap > adminConcurrencyCapMaxAllowed {
			response.BadRequest(c, "cap must be between 1 and "+strconv.Itoa(adminConcurrencyCapMaxAllowed))
			return
		}
		if err := h.store.SetCap(ctx, account.ID, *req.Cap, adminConcurrencyCapReasonAdmin); err != nil {
			response.InternalError(c, "failed to set concurrency cap: "+err.Error())
			return
		}
	}

	if req.Pinned != nil && *req.Pinned {
		// pinned 必须落在记录上：没有记录时用当前配置并发补建，避免给健康账号
		// 悄悄带上一个比配置更低的 cap。
		if record == nil && req.Cap == nil {
			if account.Concurrency <= 0 {
				response.BadRequest(c, "account has no concurrency configured; set cap together with pinned")
				return
			}
			if err := h.store.SetCap(ctx, account.ID, account.Concurrency, adminConcurrencyCapReasonAdmin); err != nil {
				response.InternalError(c, "failed to prepare concurrency cap record: "+err.Error())
				return
			}
		}
		if err := h.store.SetPinned(ctx, account.ID, true); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	} else if req.Pinned != nil {
		if record != nil {
			if err := h.store.SetPinned(ctx, account.ID, false); err != nil {
				response.ErrorFrom(c, err)
				return
			}
		}
	}

	if req.ClearFuse && record != nil {
		cleaner, ok := h.store.(service.ConcurrencyCapFuseCleaner)
		if !ok {
			response.InternalError(c, "concurrency cap store does not support fuse clearing")
			return
		}
		if err := cleaner.ClearFlapEvents(ctx, account.ID); err != nil {
			response.InternalError(c, "failed to clear concurrency cap flaps: "+err.Error())
			return
		}
	}

	updated, err := h.store.GetCap(ctx, account.ID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, h.buildResponse(account, updated))
}

func (h *AdminConcurrencyCapHandler) loadAccountAndRecord(c *gin.Context) (*service.Account, *service.AccountConcurrencyCap, bool) {
	if h.store == nil || h.accounts == nil {
		response.InternalError(c, "concurrency cap store is unavailable")
		return nil, nil, false
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return nil, nil, false
	}
	ctx := c.Request.Context()
	account, err := h.accounts.GetAccount(ctx, accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return nil, nil, false
	}
	record, err := h.store.GetCap(ctx, accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return nil, nil, false
	}
	return account, record, true
}

func (h *AdminConcurrencyCapHandler) buildResponse(account *service.Account, record *service.AccountConcurrencyCap) adminConcurrencyCapResponse {
	capMax, fuseFlapThreshold := h.limits()
	configured := account.Concurrency
	result := adminConcurrencyCapResponse{
		AccountID:             account.ID,
		Platform:              account.Platform,
		CapMax:                capMax,
		ConfiguredConcurrency: configured,
		EffectiveConcurrency:  configured,
		FuseFlapThreshold:     fuseFlapThreshold,
	}
	if record == nil {
		return result
	}
	result.Known = true
	result.Cap = &record.Cap
	result.Version = record.Version
	result.Reason = record.Reason
	result.Pinned = record.Pinned
	result.Restricted = !record.RestrictedAt.IsZero()
	result.FlapCount7d = record.FlapCount(concurrencyCapFlapWindow, time.Now())
	result.Fused = result.FlapCount7d >= fuseFlapThreshold
	result.EffectiveConcurrency = effectiveConcurrencyFromCap(configured, record.Cap)
	if result.Restricted {
		result.RestrictedAt = record.RestrictedAt.UTC().Format(time.RFC3339)
	}
	if !record.NextProbeAt.IsZero() {
		result.NextProbeAt = record.NextProbeAt.UTC().Format(time.RFC3339)
	}
	if !record.UpdatedAt.IsZero() {
		result.UpdatedAt = record.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return result
}

func (h *AdminConcurrencyCapHandler) limits() (int, int) {
	capMax := adminConcurrencyCapDefaultCapMax
	fuseFlapThreshold := adminConcurrencyCapDefaultFuseFlapThreshold
	if h.cfg == nil {
		return capMax, fuseFlapThreshold
	}
	if value := h.cfg.Gateway.ConcurrencyCap.CapMax; value > 0 {
		capMax = value
	}
	if value := h.cfg.Gateway.ConcurrencyCap.FuseFlapThreshold; value > 0 {
		fuseFlapThreshold = value
	}
	return capMax, fuseFlapThreshold
}

// concurrencyCapFlapWindow 是 flap 计数的展示窗口（滚动 7 天，与熔断口径一致）。
const concurrencyCapFlapWindow = 7 * 24 * time.Hour

// effectiveConcurrencyFromCap 与准入路径同口径：配置并发 <= 0（不限）时只能取 cap。
func effectiveConcurrencyFromCap(configured, capValue int) int {
	if configured > 0 && configured < capValue {
		return configured
	}
	return capValue
}
