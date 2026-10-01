package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// CustomModelHandler handles admin custom model management
type CustomModelHandler struct {
	customModelService *service.CustomModelService
}

// NewCustomModelHandler creates a new custom model handler
func NewCustomModelHandler(customModelService *service.CustomModelService) *CustomModelHandler {
	return &CustomModelHandler{customModelService: customModelService}
}

// CreateCustomModelRequest represents create custom model request
type CreateCustomModelRequest struct {
	ModelID          string  `json:"model_id" binding:"required"`
	UpstreamGroupID  int64   `json:"upstream_group_id" binding:"required"`
	UpstreamModel    string  `json:"upstream_model" binding:"required"`
	SystemPrompt     string  `json:"system_prompt"`
	Enabled          *bool   `json:"enabled"`
	Description      string  `json:"description"`
	DownstreamGroups []int64 `json:"downstream_groups"`
}

// UpdateCustomModelRequest represents update custom model request
type UpdateCustomModelRequest struct {
	ModelID          *string  `json:"model_id"`
	UpstreamGroupID  *int64   `json:"upstream_group_id"`
	UpstreamModel    *string  `json:"upstream_model"`
	SystemPrompt     *string  `json:"system_prompt"`
	Enabled          *bool    `json:"enabled"`
	Description      *string  `json:"description"`
	DownstreamGroups *[]int64 `json:"downstream_groups"`
}

// List handles listing all custom models
// GET /api/v1/admin/custom-models
func (h *CustomModelHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	var enabled *bool
	if value := c.Query("enabled"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			response.BadRequest(c, "Invalid enabled filter")
			return
		}
		enabled = &parsed
	}
	models, result, err := h.customModelService.ListAdmin(c.Request.Context(),
		pagination.PaginationParams{Page: page, PageSize: pageSize}, c.Query("search"), enabled)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	outModels := make([]dto.CustomModel, 0, len(models))
	for i := range models {
		outModels = append(outModels, *dto.CustomModelFromService(&models[i]))
	}

	response.Paginated(c, outModels, result.Total, page, pageSize)
}

// Get handles getting a custom model by ID
// GET /api/v1/admin/custom-models/:id
func (h *CustomModelHandler) Get(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid custom model ID")
		return
	}

	model, err := h.customModelService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.CustomModelFromService(model))
}

// Create handles creating a new custom model
// POST /api/v1/admin/custom-models
func (h *CustomModelHandler) Create(c *gin.Context) {
	var req CreateCustomModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	model, err := h.customModelService.Create(c.Request.Context(), &service.CreateCustomModelInput{
		ModelID:          req.ModelID,
		UpstreamGroupID:  req.UpstreamGroupID,
		UpstreamModel:    req.UpstreamModel,
		SystemPrompt:     req.SystemPrompt,
		Enabled:          req.Enabled,
		Description:      req.Description,
		DownstreamGroups: req.DownstreamGroups,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.CustomModelFromService(model))
}

// Update handles updating a custom model
// PUT /api/v1/admin/custom-models/:id
func (h *CustomModelHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid custom model ID")
		return
	}

	var req UpdateCustomModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	input := &service.UpdateCustomModelInput{
		ModelID:          req.ModelID,
		UpstreamGroupID:  req.UpstreamGroupID,
		UpstreamModel:    req.UpstreamModel,
		SystemPrompt:     req.SystemPrompt,
		Enabled:          req.Enabled,
		Description:      req.Description,
		DownstreamGroups: req.DownstreamGroups,
	}

	model, err := h.customModelService.Update(c.Request.Context(), id, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.CustomModelFromService(model))
}

// Delete handles deleting a custom model
// DELETE /api/v1/admin/custom-models/:id
func (h *CustomModelHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid custom model ID")
		return
	}

	if err := h.customModelService.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, gin.H{"message": "Custom model deleted successfully"})
}
