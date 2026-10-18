// Package schema maintains the catalog snapshot that describes exposed
// relations, their columns, keys, and the relationships embeddings can follow.
package schema

import (
	"fmt"
	"strings"
	"time"

	"github.com/andeshq/subquery/internal/pgrst"
)

// RelID identifies a relation by schema and name.
type RelID struct {
	Schema string
	Name   string
}

func (r RelID) String() string { return r.Schema + "." + r.Name }

// RelationKind mirrors the pg_class relkind values Subquery serves.
type RelationKind string

const (
	KindTable        RelationKind = "table"
	KindPartitioned  RelationKind = "partitioned table"
	KindView         RelationKind = "view"
	KindMaterialized RelationKind = "materialized view"
	KindForeign      RelationKind = "foreign table"
)

// Cardinality describes the shape of an embedded resource.
type Cardinality int

const (
	ToOne Cardinality = iota
	ToMany
)

func (c Cardinality) String() string {
	if c == ToMany {
		return "to-many"
	}
	return "to-one"
}

// Column is a single relation column.
type Column struct {
	Name     string
	Position int
	TypeOID  uint32
	TypeName string
	// CastType is a schema-qualified, quoted type name safe to splice into a
	// cast, for example "pg_catalog"."int4" or "public"."mood"[].
	CastType   string
	NotNull    bool
	HasDefault bool
	Generated  bool
	PrimaryKey bool
}

// IsJSON reports whether the column's declared type is json or jsonb.
func (c Column) IsJSON() bool { return IsJSONTypeName(c.TypeName) }

// IsTextSearch reports whether the column's declared type is tsvector.
func (c Column) IsTextSearch() bool { return IsTextSearchTypeName(c.TypeName) }

// IsArray reports whether the column's declared type is an array type.
func (c Column) IsArray() bool { return IsArrayTypeName(c.TypeName) }

// IsJSONTypeName reports whether a PostgreSQL type name is json or jsonb.
func IsJSONTypeName(name string) bool {
	switch strings.ToLower(name) {
	case "json", "jsonb":
		return true
	}
	return false
}

// IsTextSearchTypeName reports whether a PostgreSQL type name is tsvector.
func IsTextSearchTypeName(name string) bool { return strings.EqualFold(name, "tsvector") }

// IsArrayTypeName reports whether a PostgreSQL type name is an array type.
func IsArrayTypeName(name string) bool { return strings.HasSuffix(name, "[]") }

// ForeignKey is a referential constraint.
type ForeignKey struct {
	Name    string
	Rel     RelID // referencing relation
	Columns []string
	RefRel  RelID // referenced relation
	RefCols []string
}

// Relationship is an embeddable edge from the relation it is attached to.
type Relationship struct {
	// Name is the constraint (or junction) name usable as a !hint.
	Name string
	// Source is the relation this edge starts from.
	Source RelID
	// Target is the relation an embed resolves to.
	Target RelID
	// Cardinality is Target relative to Source.
	Cardinality Cardinality
	// FK is the constraint connecting the two sides. For a m2m edge it is the
	// junction's foreign key to Target.
	FK ForeignKey
	// Forward reports whether Source is the referencing side of FK. It is
	// meaningless for m2m edges, which always join through Junction.
	Forward bool
	// M2M marks an edge that joins through a junction table.
	M2M bool
	// Junction is the link table for m2m edges.
	Junction RelID
	// SourceFK is the junction's foreign key back to Source for m2m edges.
	SourceFK ForeignKey
	// SelfReferential reports whether Source and Target are the same relation.
	SelfReferential bool
}

// Relation is one exposed or embeddable database relation.
type Relation struct {
	ID          RelID
	Kind        RelationKind
	Exposed     bool
	Columns     []Column
	PrimaryKey  []string
	Uniques     [][]string
	Outgoing    []*Relationship
	columnIndex map[string]int
	// computed maps virtual column names to the function that produces them.
	computed map[string]*Function
}

// ComputedField returns the function backing a computed field.
func (r *Relation) ComputedField(name string) (*Function, bool) {
	fn, ok := r.computed[name]
	return fn, ok
}

// ComputedFields returns the relation's computed fields, keyed by name.
func (r *Relation) ComputedFields() map[string]*Function { return r.computed }

// FieldCastType returns the cast type of a column or computed field.
func (r *Relation) FieldCastType(name string) (string, bool) {
	if col, ok := r.Column(name); ok {
		return col.CastType, true
	}
	if fn, ok := r.computed[name]; ok {
		return fn.ScalarCast, true
	}
	return "", false
}

// FieldTypeName returns the declared type name of a column or computed field.
func (r *Relation) FieldTypeName(name string) (string, bool) {
	if col, ok := r.Column(name); ok {
		return col.TypeName, true
	}
	if fn, ok := r.computed[name]; ok {
		return fn.ResultTypeName, true
	}
	return "", false
}

// Column returns the named column.
func (r *Relation) Column(name string) (Column, bool) {
	i, ok := r.columnIndex[name]
	if !ok {
		return Column{}, false
	}
	return r.Columns[i], true
}

// HasColumns reports whether every name is a column of the relation.
func (r *Relation) HasColumns(names []string) bool {
	for _, name := range names {
		if _, ok := r.columnIndex[name]; !ok {
			return false
		}
	}
	return true
}

// IsUniqueColumnSet reports whether names exactly matches the primary key or a
// unique constraint, which distinguishes one-to-one from many-to-one.
func (r *Relation) IsUniqueColumnSet(names []string) bool {
	if sameSet(r.PrimaryKey, names) {
		return true
	}
	for _, uniq := range r.Uniques {
		if sameSet(uniq, names) {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; !ok {
			return false
		}
	}
	return true
}

// Snapshot is an immutable view of the catalog at one generation.
type Snapshot struct {
	Generation int64
	LoadedAt   time.Time
	Schemas    []string
	Relations  map[RelID]*Relation
	// Functions indexes overloads by name.
	Functions map[RelID][]*Function
	// functionOrder is the deterministic function ordering for documents.
	functionOrder []RelID
	// Order is deterministic, for stable OpenAPI output later.
	Order []RelID
}

// FunctionOrder lists the function identifiers in deterministic order.
func (s *Snapshot) FunctionOrder() []RelID { return s.functionOrder }

// Relation returns an exposed relation by name, searching the configured
// schemas in order.
func (s *Snapshot) Relation(name string) (*Relation, bool) {
	if s == nil {
		return nil, false
	}
	for _, schema := range s.Schemas {
		if rel, ok := s.Relations[RelID{Schema: schema, Name: name}]; ok && rel.Exposed {
			return rel, true
		}
	}
	return nil, false
}

// SchemaError describes a resolution failure and carries the PostgREST code
// the HTTP layer should report.
type SchemaError struct {
	Code    string // PGRST200, PGRST201, PGRST205
	Message string
	Details any
	Hint    any
}

func (e *SchemaError) Error() string { return e.Code + ": " + e.Message }

// ResolveEmbed finds the relationship a select item refers to. name is the
// requested resource, hint is the !hint disambiguator (empty when absent).
func (r *Relation) ResolveEmbed(name, hint string) (*Relationship, *SchemaError) {
	var candidates []*Relationship
	for _, rel := range r.Outgoing {
		if rel.matchesName(name) {
			candidates = append(candidates, rel)
		}
	}
	if hint != "" {
		matched := candidates[:0:0]
		for _, rel := range candidates {
			if rel.matchesHint(hint) {
				matched = append(matched, rel)
			}
		}
		if len(matched) == 0 {
			return nil, &SchemaError{
				Code:    pgrst.RelationshipNotFound,
				Message: fmt.Sprintf("Could not find a relationship between '%s' and '%s' in the schema cache", r.ID.Name, name),
			}
		}
		candidates = matched
	}
	switch len(candidates) {
	case 0:
		return nil, &SchemaError{
			Code:    pgrst.RelationshipNotFound,
			Message: fmt.Sprintf("Could not find a relationship between '%s' and '%s' in the schema cache", r.ID.Name, name),
			Hint:    r.embedHint(name),
		}
	case 1:
		return candidates[0], nil
	default:
		details := make([]map[string]string, 0, len(candidates))
		options := make([]string, 0, len(candidates))
		for _, rel := range candidates {
			details = append(details, map[string]string{
				"cardinality":  rel.Cardinality.String(),
				"embedding":    r.ID.Name + " with " + rel.Target.Name,
				"relationship": rel.describe(),
			})
			options = append(options, fmt.Sprintf("'%s!%s'", name, rel.Name))
		}
		return nil, &SchemaError{
			Code:    pgrst.AmbiguousEmbed,
			Message: fmt.Sprintf("Could not embed because more than one relationship was found for '%s' and '%s'", r.ID.Name, name),
			Details: details,
			Hint:    fmt.Sprintf("Try changing '%s' to one of the following: %s. Find the desired relationship in the 'details' key.", name, strings.Join(options, ", ")),
		}
	}
}

func (r *Relationship) matchesName(name string) bool {
	return r.Target.Name == name || r.Name == name
}

func (r *Relationship) matchesHint(hint string) bool {
	return r.Name == hint || r.Target.Name == hint
}

func (r *Relationship) describe() string {
	if r.M2M {
		return fmt.Sprintf("%s using %s and %s", r.Name, r.Junction.String(), r.Target.String())
	}
	if r.Forward {
		return fmt.Sprintf("%s using %s(%s) and %s(%s)", r.Name, r.Source.Name, strings.Join(r.FK.Columns, ", "), r.Target.Name, strings.Join(r.FK.RefCols, ", "))
	}
	return fmt.Sprintf("%s using %s(%s) and %s(%s)", r.Name, r.Target.Name, strings.Join(r.FK.Columns, ", "), r.Source.Name, strings.Join(r.FK.RefCols, ", "))
}

func (r *Relation) embedHint(name string) any {
	var options []string
	for _, rel := range r.Outgoing {
		if rel.matchesName(name) {
			options = append(options, fmt.Sprintf("%s!%s", name, rel.Name))
		}
	}
	if len(options) == 0 {
		return nil
	}
	return "Try changing '" + name + "' to one of the following: '" + strings.Join(options, "', '") + "'"
}
