package routes

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// registerTraceAdminRoutes 二开模块路由：会话 trace 管理 + 账号用量导出。
// 挂载在 admin 组下，复用现有管理员鉴权/审计中间件。
func registerTraceAdminRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	// 会话 Trace 管理
	traces := admin.Group("/traces")
	{
		traces.GET("/sessions", h.Admin.TraceAdmin.ListSessions)
		traces.GET("/stats", h.Admin.TraceAdmin.Stats)
		traces.POST("/archive", h.Admin.TraceAdmin.Archive)
		traces.GET("/download", h.Admin.TraceAdmin.Download)
		traces.GET("/settings", h.Admin.TraceAdmin.GetSettings)
		traces.PUT("/settings", h.Admin.TraceAdmin.SaveSettings)
	}

	// 上游账号用量导出 + 独立定价
	admin.GET("/account-usage-export", h.Admin.AccountUsageExport.Usage)
	admin.GET("/export-pricing", h.Admin.AccountUsageExport.GetPricing)
	admin.PUT("/export-pricing", h.Admin.AccountUsageExport.SavePricing)

	// 启动进程内定时归档调度器（配置存 data/trace-settings.json）
	h.Admin.TraceAdmin.StartScheduler(context.Background())
}
