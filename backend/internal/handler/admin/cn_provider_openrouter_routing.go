package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 二开：OpenRouter 账号的供应商路由（首选 / 备选 provider）读写接口。

// GetOpenRouterRouting 返回账号当前的供应商路由配置，以及各上游模型在 OpenRouter 上的可选 provider。
// GET /api/v1/admin/cn-providers/accounts/:id/openrouter-routing
func (h *CNProviderHandler) GetOpenRouterRouting(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.balanceService == nil {
		response.BadRequest(c, "cn provider balance service is not enabled")
		return
	}
	result, err := h.balanceService.GetOpenRouterProviderRouting(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// UpdateOpenRouterRouting 保存账号的供应商路由配置。
// PUT /api/v1/admin/cn-providers/accounts/:id/openrouter-routing
func (h *CNProviderHandler) UpdateOpenRouterRouting(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.balanceService == nil {
		response.BadRequest(c, "cn provider balance service is not enabled")
		return
	}
	var req service.OpenRouterProviderRouting
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	result, err := h.balanceService.UpdateOpenRouterProviderRouting(c.Request.Context(), accountID, req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
