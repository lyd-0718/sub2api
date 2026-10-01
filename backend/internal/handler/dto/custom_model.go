package dto

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// CustomModel represents the admin view of a custom model
type CustomModel struct {
	ID               int64     `json:"id"`
	ModelID          string    `json:"model_id"`
	UpstreamGroupID  int64     `json:"upstream_group_id"`
	UpstreamModel    string    `json:"upstream_model"`
	SystemPrompt     string    `json:"system_prompt"`
	InjectionMode    string    `json:"injection_mode"`
	Enabled          bool      `json:"enabled"`
	Description      string    `json:"description"`
	DownstreamGroups []int64   `json:"downstream_groups"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CustomModelFromService converts a service custom model to DTO
func CustomModelFromService(model *service.CustomModel) *CustomModel {
	if model == nil {
		return nil
	}
	return &CustomModel{
		ID:               model.ID,
		ModelID:          model.ModelID,
		UpstreamGroupID:  model.UpstreamGroupID,
		UpstreamModel:    model.UpstreamModel,
		SystemPrompt:     model.SystemPrompt,
		InjectionMode:    model.InjectionMode,
		Enabled:          model.Enabled,
		Description:      model.Description,
		DownstreamGroups: model.DownstreamGroups,
		CreatedAt:        model.CreatedAt,
		UpdatedAt:        model.UpdatedAt,
	}
}
