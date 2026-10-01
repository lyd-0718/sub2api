package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

var (
	ErrCustomModelNotFound            = infraerrors.NotFound("CUSTOM_MODEL_NOT_FOUND", "custom model not found")
	ErrCustomModelDisabled            = infraerrors.BadRequest("CUSTOM_MODEL_DISABLED", "custom model is disabled")
	ErrCustomModelAccessDenied        = infraerrors.Forbidden("CUSTOM_MODEL_ACCESS_DENIED", "group does not have access to this custom model")
	ErrCustomModelNesting             = infraerrors.BadRequest("CUSTOM_MODEL_NESTING", "a custom model cannot route to another custom model")
	ErrCustomModelExists              = infraerrors.Conflict("CUSTOM_MODEL_EXISTS", "custom model with this model_id already exists")
	ErrCustomModelUpstreamUnavailable = infraerrors.BadRequest("CUSTOM_MODEL_UPSTREAM_UNAVAILABLE", "custom model upstream group is missing or inactive")
	ErrCustomModelUpstreamDenied      = infraerrors.Forbidden("CUSTOM_MODEL_UPSTREAM_DENIED", "upstream group does not allow this model")
)

type CustomModel struct {
	ID               int64
	ModelID          string
	UpstreamGroupID  int64
	UpstreamModel    string
	SystemPrompt     string
	InjectionMode    string
	Enabled          bool
	Description      string
	DownstreamGroups []int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type CustomModelResolution struct {
	UpstreamGroupID int64
	UpstreamModel   string
	SystemPrompt    string
	InjectionMode   string
}

type CreateCustomModelInput struct {
	ModelID         string
	UpstreamGroupID int64
	UpstreamModel   string
	SystemPrompt    string
	InjectionMode   string
	Description     string
}

type CreateCustomModelInput struct {
	ModelID          string
	UpstreamGroupID  int64
	UpstreamModel    string
	SystemPrompt     string
	Enabled          *bool
	Description      string
	DownstreamGroups []int64
}

type UpdateCustomModelInput struct {
	ModelID          *string
	UpstreamGroupID  *int64
	UpstreamModel    *string
	InjectionMode    *string
	Enabled          *bool
	Description      *string
	DownstreamGroups *[]int64
}

type CustomModelRepository interface {
	Create(context.Context, *CustomModel) error
	Get(context.Context, string) (*CustomModel, error)
	GetByID(context.Context, int64) (*CustomModel, error)
	Update(context.Context, *CustomModel) error
	Delete(context.Context, int64) error
	// List returns enabled models explicitly bound to the downstream group.
	List(context.Context, int64) ([]CustomModel, error)
	ListAdmin(context.Context, pagination.PaginationParams, string, *bool) ([]CustomModel, *pagination.PaginationResult, error)
}

type CustomModelService struct {
	repo   CustomModelRepository
	groups GroupRepository
}

func NewCustomModelService(repo CustomModelRepository, groups GroupRepository) *CustomModelService {
	return &CustomModelService{repo: repo, groups: groups}
}

func (s *CustomModelService) Create(ctx context.Context, input *CreateCustomModelInput) (*CustomModel, error) {
	if input == nil {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "custom model cannot be nil")
	}
	model := &CustomModel{
		ModelID: input.ModelID, UpstreamGroupID: input.UpstreamGroupID,
		UpstreamModel: input.UpstreamModel, SystemPrompt: input.SystemPrompt,
		InjectionMode: input.InjectionMode, Enabled: true, Description: input.Description,
		DownstreamGroups: input.DownstreamGroups,
	}
	if input.Enabled != nil {
		model.Enabled = *input.Enabled
	}
	if err := s.validate(ctx, model); err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, model); err != nil {
		return nil, err
	}
	return model, nil
}

// Get distinguishes any existing custom name from a native model name.
// It does not authorize access; discovery must use List or ResolveCustomModel.
func (s *CustomModelService) Get(ctx context.Context, modelID string) (*CustomModel, error) {
	return s.repo.Get(ctx, modelID)
}

func (s *CustomModelService) GetByID(ctx context.Context, id int64) (*CustomModel, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *CustomModelService) Update(ctx context.Context, id int64, input *UpdateCustomModelInput) (*CustomModel, error) {
	if input == nil {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "custom model cannot be nil")
	}
	model, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if input.ModelID != nil {
		model.ModelID = *input.ModelID
	}
	if input.UpstreamGroupID != nil {
		model.UpstreamGroupID = *input.UpstreamGroupID
	}
	if input.UpstreamModel != nil {
		model.UpstreamModel = *input.UpstreamModel
	}
	if input.SystemPrompt != nil {
		model.SystemPrompt = *input.SystemPrompt
	}
	if input.InjectionMode != nil {
		model.InjectionMode = *input.InjectionMode
	}
	if input.Enabled != nil {
		model.Enabled = *input.Enabled
	}
	if input.Description != nil {
		model.Description = *input.Description
	}
	if input.DownstreamGroups != nil {
		model.DownstreamGroups = *input.DownstreamGroups
	}
	if err := s.validate(ctx, model); err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, model); err != nil {
		return nil, err
	}
	return model, nil
}

func (s *CustomModelService) validate(ctx context.Context, model *CustomModel) error {
	// Usage attribution fields accept at most 100 bytes. Reject longer names
	// before routing so successful requests cannot lose their billing records.
	for _, name := range []string{model.ModelID, model.UpstreamModel} {
		if name == "" || !utf8.ValidString(name) || len(name) > 100 || strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return infraerrors.BadRequest("INVALID_MODEL_NAME", "model_id and upstream_model must contain 1–100 UTF-8 bytes without whitespace or control characters")
		}
	}
	if model.UpstreamGroupID <= 0 {
		return infraerrors.BadRequest("INVALID_UPSTREAM_GROUP", "upstream_group_id must be positive")
	}
	if _, err := s.groups.GetByID(ctx, model.UpstreamGroupID); err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			return infraerrors.BadRequest("INVALID_UPSTREAM_GROUP", "upstream group does not exist")
		}
		return err
	}
	groups := make([]int64, 0, len(model.DownstreamGroups))
	for _, id := range model.DownstreamGroups {
		if id <= 0 {
			return infraerrors.BadRequest("INVALID_DOWNSTREAM_GROUP", "downstream group IDs must be positive")
		}
		if slices.Contains(groups, id) {
			continue
		}
		if _, err := s.groups.GetByID(ctx, id); err != nil {
			if errors.Is(err, ErrGroupNotFound) {
				return infraerrors.BadRequest("INVALID_DOWNSTREAM_GROUP", "downstream group does not exist")
			}
			return err
		}
		groups = append(groups, id)
	}
	model.DownstreamGroups = groups
	return nil
}

func (s *CustomModelService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
}

func (s *CustomModelService) List(ctx context.Context, groupID int64) ([]CustomModel, error) {
	if groupID <= 0 {
		return []CustomModel{}, nil
	}
	return s.repo.List(ctx, groupID)
}

func (s *CustomModelService) ListAdmin(ctx context.Context, params pagination.PaginationParams, search string, enabled *bool) ([]CustomModel, *pagination.PaginationResult, error) {
	return s.repo.ListAdmin(ctx, params, strings.TrimSpace(search), enabled)
}

// ResolveCustomModel performs exactly one custom-model hop. Configuration may
// reference any name, but an existing custom name is never an upstream target.
func (s *CustomModelService) ResolveCustomModel(ctx context.Context, modelID string, downstreamGroupID int64) (*CustomModelResolution, error) {
	model, err := s.repo.Get(ctx, modelID)
	if err != nil {
		return nil, err
	}
	if !model.Enabled {
		return nil, ErrCustomModelDisabled
	}
	if downstreamGroupID <= 0 || !slices.Contains(model.DownstreamGroups, downstreamGroupID) {
		return nil, ErrCustomModelAccessDenied
	}
	_, err = s.repo.Get(ctx, model.UpstreamModel)
	if err == nil {
		return nil, ErrCustomModelNesting
	}
	if !errors.Is(err, ErrCustomModelNotFound) {
		return nil, err
	}
	upstream, err := s.groups.GetByID(ctx, model.UpstreamGroupID)
	if err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			return nil, ErrCustomModelUpstreamUnavailable
		}
		return nil, err
	}
	if !upstream.IsActive() {
		return nil, ErrCustomModelUpstreamUnavailable
	}
	if !upstream.ModelAllowlist.Allows(model.UpstreamModel) {
		return nil, ErrCustomModelUpstreamDenied
	}
	return &CustomModelResolution{
		ModelID: model.ModelID, UpstreamGroupID: upstream.ID, UpstreamGroup: upstream,
		UpstreamModel: model.UpstreamModel, SystemPrompt: model.SystemPrompt,
		InjectionMode: model.InjectionMode,
	}, nil
}
