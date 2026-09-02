package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// TraceAdminHandler 会话 trace 后台管理（列表/统计/下载/归档/设置）。
type TraceAdminHandler struct {
	svc *service.TraceAdminService
}

func NewTraceAdminHandler(svc *service.TraceAdminService) *TraceAdminHandler {
	return &TraceAdminHandler{svc: svc}
}

// StartScheduler 启动进程内定时归档调度器。
func (h *TraceAdminHandler) StartScheduler(ctx context.Context) {
	h.svc.StartScheduler(ctx)
}

// ListSessions GET /api/v1/admin/traces/sessions
func (h *TraceAdminHandler) ListSessions(c *gin.Context) {
	var archived *bool
	if v := c.Query("archived"); v != "" {
		b := v == "true" || v == "1"
		archived = &b
	}
	var apiKeyID int64
	if v := c.Query("api_key_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			apiKeyID = id
		}
	}
	rows, err := h.svc.ListSessions(c.Request.Context(), c.Query("date"), c.Query("session_id"), apiKeyID, archived)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": rows, "total": len(rows)})
}

// Stats GET /api/v1/admin/traces/stats
func (h *TraceAdminHandler) Stats(c *gin.Context) {
	stats, err := h.svc.Stats(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}

// Archive POST /api/v1/admin/traces/archive  body: {"date":"20260901"}
func (h *TraceAdminHandler) Archive(c *gin.Context) {
	var req struct {
		Date string `json:"date" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "date 必填，格式 20260901")
		return
	}
	res, err := h.svc.ArchiveDay(c.Request.Context(), req.Date)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, res)
}

// Download GET /api/v1/admin/traces/download?date=&session=
func (h *TraceAdminHandler) Download(c *gin.Context) {
	path, name, cleanup, err := h.svc.ResolveDownload(c.Query("date"), c.Query("session"))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	defer cleanup()
	c.Header("Content-Type", "application/zstd")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	c.File(path)
}

// GetSettings GET /api/v1/admin/traces/settings
func (h *TraceAdminHandler) GetSettings(c *gin.Context) {
	response.Success(c, h.svc.GetSettings())
}

// SaveSettings PUT /api/v1/admin/traces/settings
func (h *TraceAdminHandler) SaveSettings(c *gin.Context) {
	var req service.TraceArchiveSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "参数错误")
		return
	}
	if err := h.svc.SaveSettings(req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, h.svc.GetSettings())
}

// ============================================================================

// AccountUsageExportHandler 上游账号用量导出 + 独立定价。
type AccountUsageExportHandler struct {
	svc *service.AccountUsageExportService
}

func NewAccountUsageExportHandler(svc *service.AccountUsageExportService) *AccountUsageExportHandler {
	return &AccountUsageExportHandler{svc: svc}
}

// Usage GET /api/v1/admin/account-usage-export?start=&end=&granularity=&account_ids=&format=
func (h *AccountUsageExportHandler) Usage(c *gin.Context) {
	start, end, err := service.ParseDateRange(c.DefaultQuery("start", ""), c.DefaultQuery("end", ""))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	granularity := c.DefaultQuery("granularity", "month")
	accountIDs := parseIDList(c.Query("account_ids"))

	rows, err := h.svc.Query(c.Request.Context(), start, end, granularity, accountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	service.SortAccountUsageRows(rows)

	if c.Query("format") == "csv" {
		filename := fmt.Sprintf("account-usage_%s_%s.csv", c.Query("start"), c.Query("end"))
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		if err := h.svc.WriteCSV(rows, c.Writer); err != nil {
			c.Status(http.StatusInternalServerError)
		}
		return
	}

	var totCost float64
	allKnown := true
	for _, r := range rows {
		totCost += r.Cost
		if !r.CostKnown {
			allKnown = false
		}
	}
	response.Success(c, gin.H{
		"items": rows, "total": len(rows),
		"total_cost": totCost, "cost_complete": allKnown,
		"currency": h.svc.GetPricing().Currency,
	})
}

// GetPricing GET /api/v1/admin/export-pricing
// 返回定价 + 近 90 天实际出现过的模型（前端定价页自动列行）。
func (h *AccountUsageExportHandler) GetPricing(c *gin.Context) {
	seen, err := h.svc.PricedModelsSeen(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"pricing":     h.svc.GetPricing(),
		"models_seen": seen,
	})
}

// SavePricing PUT /api/v1/admin/export-pricing
func (h *AccountUsageExportHandler) SavePricing(c *gin.Context) {
	var req service.ExportPricing
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "参数错误")
		return
	}
	if err := h.svc.SavePricing(req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, h.svc.GetPricing())
}

func parseIDList(v string) []int64 {
	if v == "" {
		return nil
	}
	out := []int64{}
	for _, s := range strings.Split(v, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}
