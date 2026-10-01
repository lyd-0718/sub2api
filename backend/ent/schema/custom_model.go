package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// CustomModel holds the schema definition for custom models with injected system prompts.
type CustomModel struct {
	ent.Schema
}

func (CustomModel) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "custom_models"},
	}
}

func (CustomModel) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (CustomModel) Fields() []ent.Field {
	return []ent.Field{
		field.String("model_id").
			MaxLen(200).
			NotEmpty().
			Comment("Virtual model name exposed to downstream."),
		field.Int64("upstream_group_id"),
		field.String("upstream_model").
			MaxLen(200).
			NotEmpty().
			Comment("Real upstream model name."),
		field.String("system_prompt").
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Comment("System prompt to inject into requests."),
		field.String("injection_mode").
			MaxLen(20).
			Default("prepend").
			Comment("System prompt injection strategy: prepend, append, or replace"),
		field.Bool("enabled").
			Default(true).
			Comment("Whether this custom model is active."),
		field.String("description").
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Comment("Description or notes about this custom model."),
	}
}

func (CustomModel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("upstream_group", Group.Type).
			Ref("custom_models_upstream").
			Unique().
			Required().
			Field("upstream_group_id"),
		edge.To("downstream_groups", Group.Type),
	}
}

func (CustomModel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("model_id").
			Unique().
			StorageKey("idx_custom_models_model_id_unique_active").
			Annotations(
				entsql.IndexWhere("deleted_at IS NULL"),
			),
		index.Fields("upstream_group_id").
			StorageKey("idx_custom_models_upstream_group_id").
			Annotations(
				entsql.IndexWhere("deleted_at IS NULL"),
			),
		index.Fields("enabled").
			StorageKey("idx_custom_models_enabled").
			Annotations(
				entsql.IndexWhere("deleted_at IS NULL"),
			),
		index.Fields("deleted_at").StorageKey("idx_custom_models_deleted_at"),
	}
}
