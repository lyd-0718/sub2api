package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/custommodel"
	"github.com/Wei-Shaw/sub2api/ent/custommodeldownstreamgroup"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type customModelRepository struct{ client *dbent.Client }

func NewCustomModelRepository(client *dbent.Client) service.CustomModelRepository {
	return &customModelRepository{client: client}
}

func (r *customModelRepository) Create(ctx context.Context, model *service.CustomModel) error {
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if dbent.TxFromContext(ctx) == nil {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		client = tx.Client()
	}
	created, err := client.CustomModel.Create().
		SetModelID(model.ModelID).
		SetUpstreamGroupID(model.UpstreamGroupID).
		SetUpstreamModel(model.UpstreamModel).
		SetSystemPrompt(model.SystemPrompt).
		SetInjectionMode(model.InjectionMode).
		SetEnabled(model.Enabled).
		SetDescription(model.Description).
		Save(ctx)
	if err != nil {
		return translatePersistenceError(err, nil, service.ErrCustomModelExists)
	}
	if len(model.DownstreamGroups) > 0 {
		bulk := make([]*dbent.CustomModelDownstreamGroupCreate, len(model.DownstreamGroups))
		for i, groupID := range model.DownstreamGroups {
			bulk[i] = client.CustomModelDownstreamGroup.Create().SetCustomModelID(created.ID).SetGroupID(groupID)
		}
		if _, err := client.CustomModelDownstreamGroup.CreateBulk(bulk...).Save(ctx); err != nil {
			return err
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	model.ID, model.CreatedAt, model.UpdatedAt = created.ID, created.CreatedAt, created.UpdatedAt
	return nil
}

func (r *customModelRepository) Get(ctx context.Context, modelID string) (*service.CustomModel, error) {
	entity, err := clientFromContext(ctx, r.client).CustomModel.Query().
		Where(custommodel.ModelIDEQ(modelID), custommodel.DeletedAtIsNil()).
		WithDownstreamGroups().Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrCustomModelNotFound, nil)
	}
	return customModelEntityToService(entity), nil
}

func (r *customModelRepository) GetByID(ctx context.Context, id int64) (*service.CustomModel, error) {
	entity, err := clientFromContext(ctx, r.client).CustomModel.Query().
		Where(custommodel.IDEQ(id), custommodel.DeletedAtIsNil()).
		WithDownstreamGroups().Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrCustomModelNotFound, nil)
	}
	return customModelEntityToService(entity), nil
}

func (r *customModelRepository) Update(ctx context.Context, model *service.CustomModel) error {
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if dbent.TxFromContext(ctx) == nil {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		client = tx.Client()
	}
	updated, err := client.CustomModel.UpdateOneID(model.ID).
		Where(custommodel.DeletedAtIsNil()).
		SetModelID(model.ModelID).
		SetUpstreamGroupID(model.UpstreamGroupID).
		SetUpstreamModel(model.UpstreamModel).
		SetSystemPrompt(model.SystemPrompt).
		SetInjectionMode(model.InjectionMode).
		SetEnabled(model.Enabled).
		SetDescription(model.Description).
		Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrCustomModelNotFound, service.ErrCustomModelExists)
	}
	if _, err := client.CustomModelDownstreamGroup.Delete().Where(custommodeldownstreamgroup.CustomModelIDEQ(model.ID)).Exec(ctx); err != nil {
		return err
	}
	if len(model.DownstreamGroups) > 0 {
		bulk := make([]*dbent.CustomModelDownstreamGroupCreate, len(model.DownstreamGroups))
		for i, groupID := range model.DownstreamGroups {
			bulk[i] = client.CustomModelDownstreamGroup.Create().SetCustomModelID(model.ID).SetGroupID(groupID)
		}
		if _, err := client.CustomModelDownstreamGroup.CreateBulk(bulk...).Save(ctx); err != nil {
			return err
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	model.UpdatedAt = updated.UpdatedAt
	return nil
}

func (r *customModelRepository) Delete(ctx context.Context, id int64) error {
	_, err := clientFromContext(ctx, r.client).CustomModel.UpdateOneID(id).
		Where(custommodel.DeletedAtIsNil()).SetDeletedAt(time.Now()).Save(ctx)
	return translatePersistenceError(err, service.ErrCustomModelNotFound, nil)
}

func (r *customModelRepository) List(ctx context.Context, groupID int64) ([]service.CustomModel, error) {
	if groupID <= 0 {
		return []service.CustomModel{}, nil
	}
	entities, err := clientFromContext(ctx, r.client).CustomModel.Query().
		Where(custommodel.DeletedAtIsNil(), custommodel.EnabledEQ(true),
			custommodel.HasDownstreamGroupsWith(group.IDEQ(groupID))).
		WithDownstreamGroups().Order(dbent.Asc(custommodel.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]service.CustomModel, len(entities))
	for i, entity := range entities {
		models[i] = *customModelEntityToService(entity)
	}
	return models, nil
}

func (r *customModelRepository) ListAdmin(ctx context.Context, params pagination.PaginationParams, search string, enabled *bool) ([]service.CustomModel, *pagination.PaginationResult, error) {
	query := clientFromContext(ctx, r.client).CustomModel.Query().Where(custommodel.DeletedAtIsNil())
	if search != "" {
		query = query.Where(custommodel.Or(custommodel.ModelIDContainsFold(search), custommodel.UpstreamModelContainsFold(search), custommodel.DescriptionContainsFold(search)))
	}
	if enabled != nil {
		query = query.Where(custommodel.EnabledEQ(*enabled))
	}
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	entities, err := query.WithDownstreamGroups().Order(dbent.Desc(custommodel.FieldID)).
		Offset(params.Offset()).Limit(params.Limit()).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	models := make([]service.CustomModel, len(entities))
	for i, entity := range entities {
		models[i] = *customModelEntityToService(entity)
	}
	return models, paginationResultFromTotal(int64(total), params), nil
}

func customModelEntityToService(entity *dbent.CustomModel) *service.CustomModel {
	model := &service.CustomModel{
		ID: entity.ID, ModelID: entity.ModelID, UpstreamGroupID: entity.UpstreamGroupID,
		UpstreamModel: entity.UpstreamModel, SystemPrompt: entity.SystemPrompt,
		InjectionMode: entity.InjectionMode, Enabled: entity.Enabled, Description: entity.Description,
		CreatedAt: entity.CreatedAt, UpdatedAt: entity.UpdatedAt,
		DownstreamGroups: make([]int64, len(entity.Edges.DownstreamGroups)),
	}
	for i, downstream := range entity.Edges.DownstreamGroups {
		model.DownstreamGroups[i] = downstream.ID
	}
	return model
}
