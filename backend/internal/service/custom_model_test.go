package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type customModelTestRepo struct {
	CustomModelRepository
	models       map[string]*CustomModel
	lookupErrors map[string]error
}

func (r *customModelTestRepo) Get(_ context.Context, name string) (*CustomModel, error) {
	if err := r.lookupErrors[name]; err != nil {
		return nil, err
	}
	if model := r.models[name]; model != nil {
		copy := *model
		return &copy, nil
	}
	return nil, ErrCustomModelNotFound
}

func (r *customModelTestRepo) GetByID(ctx context.Context, id int64) (*CustomModel, error) {
	for name, model := range r.models {
		if model.ID == id {
			return r.Get(ctx, name)
		}
	}
	return nil, ErrCustomModelNotFound
}

func (r *customModelTestRepo) Create(_ context.Context, model *CustomModel) error {
	if r.models[model.ModelID] != nil {
		return ErrCustomModelExists
	}
	model.ID = int64(len(r.models) + 1)
	copy := *model
	r.models[model.ModelID] = &copy
	return nil
}

func (r *customModelTestRepo) Update(_ context.Context, model *CustomModel) error {
	for name, existing := range r.models {
		if existing.ID == model.ID {
			delete(r.models, name)
			copy := *model
			r.models[model.ModelID] = &copy
			return nil
		}
	}
	return ErrCustomModelNotFound
}

type customModelTestGroups struct {
	GroupRepository
	groups map[int64]*Group
	err    error
}

func (r *customModelTestGroups) GetByID(_ context.Context, id int64) (*Group, error) {
	if r.err != nil {
		return nil, r.err
	}
	if group := r.groups[id]; group != nil {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

func customModelFixture() (*CustomModelService, *customModelTestRepo, *customModelTestGroups) {
	repo := &customModelTestRepo{models: map[string]*CustomModel{}, lookupErrors: map[string]error{}}
	groups := &customModelTestGroups{groups: map[int64]*Group{
		1:   {ID: 1, Status: StatusActive},
		100: {ID: 100, Status: StatusActive, Platform: PlatformOpenAI},
	}}
	return NewCustomModelService(repo, groups), repo, groups
}

func TestCustomModelService_ResolveCustomModel(t *testing.T) {
	databaseError := errors.New("database unavailable")
	for _, tc := range []struct {
		name    string
		change  func(*customModelTestRepo, *customModelTestGroups)
		groupID int64
		want    error
	}{
		{name: "success", groupID: 1},
		{name: "native model", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) { delete(r.models, "custom") }, want: ErrCustomModelNotFound},
		{name: "disabled", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) { r.models["custom"].Enabled = false }, want: ErrCustomModelDisabled},
		{name: "unbound group", groupID: 2, want: ErrCustomModelAccessDenied},
		{name: "group zero is not admin", groupID: 0, want: ErrCustomModelAccessDenied},
		{name: "empty bindings deny", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) { r.models["custom"].DownstreamGroups = nil }, want: ErrCustomModelAccessDenied},
		{name: "self reference", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) { r.models["custom"].UpstreamModel = "custom" }, want: ErrCustomModelNesting},
		{name: "disabled inaccessible custom target", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) {
			r.models["native"] = &CustomModel{ModelID: "native", UpstreamModel: "real", Enabled: false}
		}, want: ErrCustomModelNesting},
		{name: "acyclic custom target", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) {
			r.models["native"] = &CustomModel{ModelID: "native", UpstreamModel: "real", Enabled: true, DownstreamGroups: []int64{1}}
		}, want: ErrCustomModelNesting},
		{name: "initial lookup failure", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) { r.lookupErrors["custom"] = databaseError }, want: databaseError},
		{name: "upstream lookup failure", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) { r.lookupErrors["native"] = databaseError }, want: databaseError},
		{name: "wrapped native absence", groupID: 1, change: func(r *customModelTestRepo, _ *customModelTestGroups) {
			r.lookupErrors["native"] = fmt.Errorf("lookup: %w", ErrCustomModelNotFound)
		}},
		{name: "missing upstream group", groupID: 1, change: func(_ *customModelTestRepo, g *customModelTestGroups) { delete(g.groups, 100) }, want: ErrCustomModelUpstreamUnavailable},
		{name: "inactive upstream group", groupID: 1, change: func(_ *customModelTestRepo, g *customModelTestGroups) { g.groups[100].Status = StatusDisabled }, want: ErrCustomModelUpstreamUnavailable},
		{name: "group database failure", groupID: 1, change: func(_ *customModelTestRepo, g *customModelTestGroups) { g.err = databaseError }, want: databaseError},
		{name: "upstream allowlist denies", groupID: 1, change: func(_ *customModelTestRepo, g *customModelTestGroups) {
			g.groups[100].ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: []string{"other"}}
		}, want: ErrCustomModelUpstreamDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, groups := customModelFixture()
			repo.models["custom"] = &CustomModel{ModelID: "custom", UpstreamGroupID: 100, UpstreamModel: "native", SystemPrompt: "be helpful", Enabled: true, DownstreamGroups: []int64{1}}
			if tc.change != nil {
				tc.change(repo, groups)
			}
			resolved, err := svc.ResolveCustomModel(context.Background(), "custom", tc.groupID)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				require.Nil(t, resolved)
				return
			}
			require.NoError(t, err)
			require.Equal(t, &CustomModelResolution{ModelID: "custom", UpstreamGroupID: 100, UpstreamGroup: groups.groups[100], UpstreamModel: "native", SystemPrompt: "be helpful"}, resolved)
		})
	}
}

func TestCustomModelService_UpdateClearsOptionalFields(t *testing.T) {
	svc, repo, _ := customModelFixture()
	ctx := context.Background()
	enabled := true
	created, err := svc.Create(ctx, &CreateCustomModelInput{ModelID: "vendor/model-v1.2", UpstreamGroupID: 100, UpstreamModel: "native", SystemPrompt: "prompt", Description: "description", DownstreamGroups: []int64{1}, Enabled: &enabled})
	require.NoError(t, err)
	empty := ""
	emptyGroups := []int64{}
	disabled := false
	_, err = svc.Update(ctx, created.ID, &UpdateCustomModelInput{SystemPrompt: &empty, Description: &empty, DownstreamGroups: &emptyGroups, Enabled: &disabled})
	require.NoError(t, err)
	stored, err := repo.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Empty(t, stored.SystemPrompt)
	require.Empty(t, stored.Description)
	require.Empty(t, stored.DownstreamGroups)
	require.False(t, stored.Enabled)
	require.Equal(t, "vendor/model-v1.2", stored.ModelID)
	require.Equal(t, "native", stored.UpstreamModel)
}

func TestCustomModelService_ConfigurationAllowsCustomReferences(t *testing.T) {
	svc, _, _ := customModelFixture()
	ctx := context.Background()
	created, err := svc.Create(ctx, &CreateCustomModelInput{ModelID: "self", UpstreamGroupID: 100, UpstreamModel: "self", DownstreamGroups: []int64{1}})
	require.NoError(t, err)
	require.True(t, created.Enabled)
	_, err = svc.ResolveCustomModel(ctx, "self", 1)
	require.ErrorIs(t, err, ErrCustomModelNesting)
	_, err = svc.Create(ctx, &CreateCustomModelInput{ModelID: "other", UpstreamGroupID: 100, UpstreamModel: "self"})
	require.NoError(t, err)
	name := "other"
	updated, err := svc.Update(ctx, created.ID, &UpdateCustomModelInput{UpstreamModel: &name})
	require.NoError(t, err)
	require.Equal(t, "other", updated.UpstreamModel)
	_, err = svc.ResolveCustomModel(ctx, "self", 1)
	require.ErrorIs(t, err, ErrCustomModelNesting)
}

func TestCustomModelService_ValidatesNamesAndGroups(t *testing.T) {
	for _, name := range []string{"", " model", "model name", "model\tname", "model\u00a0name", strings.Repeat("m", 101), strings.Repeat("模", 34)} {
		t.Run(fmt.Sprintf("invalid_%q", name), func(t *testing.T) {
			svc, _, _ := customModelFixture()
			_, err := svc.Create(context.Background(), &CreateCustomModelInput{ModelID: name, UpstreamGroupID: 100, UpstreamModel: "native"})
			require.Error(t, err)
		})
	}
	svc, _, _ := customModelFixture()
	ctx := context.Background()
	model, err := svc.Create(ctx, &CreateCustomModelInput{ModelID: strings.Repeat("m", 100), UpstreamGroupID: 100, UpstreamModel: "vendor/model-v1.2", DownstreamGroups: []int64{1, 1}})
	require.NoError(t, err)
	require.Equal(t, []int64{1}, model.DownstreamGroups)
	for _, input := range []*CreateCustomModelInput{
		{ModelID: "missing-upstream", UpstreamGroupID: 999, UpstreamModel: "native"},
		{ModelID: "missing-downstream", UpstreamGroupID: 100, UpstreamModel: "native", DownstreamGroups: []int64{999}},
		{ModelID: "empty-upstream-model", UpstreamGroupID: 100},
	} {
		_, err := svc.Create(ctx, input)
		require.Error(t, err)
	}
}

func TestCustomModelService_ListZeroHasNoAdminBypass(t *testing.T) {
	// A zero group must not query the repository at all, let alone list admin data.
	svc := NewCustomModelService(nil, nil)
	models, err := svc.List(context.Background(), 0)
	require.NoError(t, err)
	require.Empty(t, models)
}
