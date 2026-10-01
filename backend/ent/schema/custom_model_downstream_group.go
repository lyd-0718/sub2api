package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// CustomModelDownstreamGroup holds the schema for the custom_model_downstream_groups association table.
type CustomModelDownstreamGroup struct {
	ent.Schema
}

func (CustomModelDownstreamGroup) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "custom_model_downstream_groups"},
		field.ID("custom_model_id", "group_id"),
	}
}

func (CustomModelDownstreamGroup) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("custom_model_id"),
		field.Int64("group_id"),
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (CustomModelDownstreamGroup) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("custom_model", CustomModel.Type).
			Unique().
			Required().
			Field("custom_model_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("group", Group.Type).
			Unique().
			Required().
			Field("group_id").
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

func (CustomModelDownstreamGroup) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id").StorageKey("idx_custom_model_downstream_groups_group_id"),
	}
}
