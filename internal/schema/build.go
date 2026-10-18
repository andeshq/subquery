package schema

import (
	"slices"
	"sort"
	"time"
)

// Catalog is the raw result of introspection, independent of any database
// driver so that graph building can be unit tested.
type Catalog struct {
	Relations     []CatalogRelation
	Constraints   []CatalogConstraint
	UniqueIndexes []CatalogUniqueIndex
	ViewSources   []CatalogViewSource
	Functions     []CatalogFunction
}

// CatalogFunction is one overloaded function.
type CatalogFunction struct {
	Schema         string
	Name           string
	Volatility     byte
	ReturnsSet     bool
	ReturnsVoid    bool
	ReturnTypeName string
	ReturnCastType string
	DefaultCount   int
	// ReturnRelation is set when the return type is a relation's row type.
	ReturnRelation RelID
	Args           []CatalogFunctionArg
}

// CatalogFunctionArg is one function parameter or result column.
type CatalogFunctionArg struct {
	Name       string
	Mode       byte
	Position   int
	TypeOID    uint32
	CastType   string
	HasDefault bool
	// Relation is set when the argument's type is a relation's row type, which
	// is how computed fields are recognized.
	Relation RelID
}

// CatalogRelation is one relation and its columns in attnum order.
type CatalogRelation struct {
	Schema  string
	Name    string
	Kind    RelationKind
	Columns []CatalogColumn
}

// CatalogColumn is one column.
type CatalogColumn struct {
	Name       string
	Position   int
	TypeOID    uint32
	TypeName   string
	CastType   string
	NotNull    bool
	HasDefault bool
	Generated  bool
}

// CatalogConstraint is a primary key, unique constraint, or foreign key.
type CatalogConstraint struct {
	Name    string
	Type    byte // 'p', 'u', or 'f'
	Rel     RelID
	Columns []string
	RefRel  RelID
	RefCols []string
}

// CatalogUniqueIndex is a single-column-set unique index.
type CatalogUniqueIndex struct {
	Name    string
	Rel     RelID
	Columns []string
}

// CatalogViewSource records that a view reads from a base relation.
type CatalogViewSource struct {
	View RelID
	Base RelID
}

// maxViewDepth bounds recursive view dependency resolution.
const maxViewDepth = 16

// Build turns a catalog into an immutable snapshot, deriving primary keys,
// unique sets, the relationship graph, and view-inherited foreign keys.
func Build(generation int64, exposed []string, cat Catalog) *Snapshot {
	snap := buildRelations(generation, exposed, cat.Relations)
	fks := buildConstraints(snap, cat.Constraints)
	addUniqueIndexes(snap, cat.UniqueIndexes)

	viewBases := groupViewSources(cat.ViewSources)
	g := &graph{builder: snap}
	for _, fk := range fks {
		g.addForeignKey(fk, RelID{}, RelID{})
	}
	g.addViewEdges(fks, viewBases)
	g.addManyToMany(fks)

	inheritViewPrimaryKeys(snap, viewBases)
	sortRelationships(snap)
	snap.Order = buildOrder(snap)
	buildFunctions(snap, cat.Functions)
	return snap
}

// buildRelations materializes the relations and their columns.
func buildRelations(generation int64, exposed []string, relations []CatalogRelation) *Snapshot {
	snap := &Snapshot{
		Generation: generation,
		LoadedAt:   time.Now(),
		Schemas:    exposed,
		Relations:  make(map[RelID]*Relation, len(relations)),
	}
	exposedSet := make(map[string]bool, len(exposed))
	for _, s := range exposed {
		exposedSet[s] = true
	}
	for _, cr := range relations {
		rel := &Relation{
			ID:          RelID{Schema: cr.Schema, Name: cr.Name},
			Kind:        cr.Kind,
			Exposed:     exposedSet[cr.Schema],
			columnIndex: make(map[string]int, len(cr.Columns)),
		}
		for _, cc := range cr.Columns {
			rel.columnIndex[cc.Name] = len(rel.Columns)
			rel.Columns = append(rel.Columns, Column{
				Name:       cc.Name,
				Position:   cc.Position,
				TypeOID:    cc.TypeOID,
				TypeName:   cc.TypeName,
				CastType:   cc.CastType,
				NotNull:    cc.NotNull,
				HasDefault: cc.HasDefault,
				Generated:  cc.Generated,
			})
		}
		snap.Relations[rel.ID] = rel
	}
	return snap
}

// buildConstraints records primary keys and unique constraints, and returns the
// foreign keys whose endpoints are both known.
func buildConstraints(snap *Snapshot, constraints []CatalogConstraint) []ForeignKey {
	var fks []ForeignKey
	for _, con := range constraints {
		rel, ok := snap.Relations[con.Rel]
		if !ok {
			continue
		}
		switch con.Type {
		case 'p':
			rel.PrimaryKey = con.Columns
			for _, name := range con.Columns {
				if i, ok := rel.columnIndex[name]; ok {
					rel.Columns[i].PrimaryKey = true
				}
			}
		case 'u':
			rel.Uniques = append(rel.Uniques, con.Columns)
		case 'f':
			if _, ok := snap.Relations[con.RefRel]; !ok {
				continue
			}
			fks = append(fks, ForeignKey{
				Name:    con.Name,
				Rel:     con.Rel,
				Columns: con.Columns,
				RefRel:  con.RefRel,
				RefCols: con.RefCols,
			})
		}
	}
	return fks
}

// addUniqueIndexes adds single-column-set unique indexes to their relation.
func addUniqueIndexes(snap *Snapshot, indexes []CatalogUniqueIndex) {
	for _, idx := range indexes {
		if rel, ok := snap.Relations[idx.Rel]; ok {
			rel.Uniques = append(rel.Uniques, idx.Columns)
		}
	}
}

// groupViewSources maps each view to the relations its definition reads.
func groupViewSources(sources []CatalogViewSource) map[RelID][]RelID {
	viewBases := make(map[RelID][]RelID)
	for _, vs := range sources {
		viewBases[vs.View] = append(viewBases[vs.View], vs.Base)
	}
	return viewBases
}

// inheritViewPrimaryKeys lets a view inherit its base table's primary key when
// it projects the key columns, which is what lets PUT be offered on an
// updatable view.
func inheritViewPrimaryKeys(snap *Snapshot, viewBases map[RelID][]RelID) {
	for viewID := range viewBases {
		view, ok := snap.Relations[viewID]
		if !ok || len(view.PrimaryKey) > 0 {
			continue
		}
		for _, base := range transitiveBases(viewID, viewBases) {
			baseRel, ok := snap.Relations[base]
			if !ok || len(baseRel.PrimaryKey) == 0 || !view.HasColumns(baseRel.PrimaryKey) {
				continue
			}
			view.PrimaryKey = baseRel.PrimaryKey
			for _, name := range baseRel.PrimaryKey {
				if i, ok := view.columnIndex[name]; ok {
					view.Columns[i].PrimaryKey = true
				}
			}
			break
		}
	}
}

// sortRelationships de-duplicates and orders each relation's outgoing edges so
// embeddings resolve deterministically.
func sortRelationships(snap *Snapshot) {
	for _, rel := range snap.Relations {
		rel.Outgoing = dedupeEdges(rel.Outgoing)
		sort.Slice(rel.Outgoing, func(i, j int) bool {
			a, b := rel.Outgoing[i], rel.Outgoing[j]
			if a.Target.Name != b.Target.Name {
				return a.Target.Name < b.Target.Name
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Cardinality < b.Cardinality
		})
	}
}

// buildOrder lists the relation identifiers in deterministic order.
func buildOrder(snap *Snapshot) []RelID {
	order := make([]RelID, 0, len(snap.Relations))
	for id := range snap.Relations {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].Schema != order[j].Schema {
			return order[i].Schema < order[j].Schema
		}
		return order[i].Name < order[j].Name
	})
	return order
}

type graph struct {
	builder *Snapshot
}

// addForeignKey records both directions of a constraint. When override is set,
// the referencing and referenced relations are replaced (view propagation).
func (g *graph) addForeignKey(fk ForeignKey, rel, refRel RelID) {
	if rel != (RelID{}) {
		fk.Rel = rel
	}
	if refRel != (RelID{}) {
		fk.RefRel = refRel
	}
	src, ok := g.builder.Relations[fk.Rel]
	if !ok {
		return
	}
	tgt, ok := g.builder.Relations[fk.RefRel]
	if !ok {
		return
	}
	unique := src.IsUniqueColumnSet(fk.Columns)

	src.Outgoing = append(src.Outgoing, &Relationship{
		Name:            fk.Name,
		Source:          src.ID,
		Target:          tgt.ID,
		Cardinality:     ToOne,
		FK:              fk,
		Forward:         true,
		SelfReferential: src.ID == tgt.ID,
	})
	tgt.Outgoing = append(tgt.Outgoing, &Relationship{
		Name:            fk.Name,
		Source:          tgt.ID,
		Target:          src.ID,
		Cardinality:     cardinality(!unique),
		FK:              fk,
		Forward:         false,
		SelfReferential: src.ID == tgt.ID,
	})
}

func cardinality(toMany bool) Cardinality {
	if toMany {
		return ToMany
	}
	return ToOne
}

// addViewEdges lets views inherit foreign keys of their (transitive) base
// relations when the referencing columns are projected by the view.
func (g *graph) addViewEdges(fks []ForeignKey, viewBases map[RelID][]RelID) {
	for viewID := range viewBases {
		view, ok := g.builder.Relations[viewID]
		if !ok {
			continue
		}
		for _, base := range transitiveBases(viewID, viewBases) {
			for _, fk := range fks {
				// The view acts as the referencing side.
				if fk.Rel == base && view.HasColumns(fk.Columns) {
					g.addForeignKey(fk, viewID, RelID{})
				}
				// The view acts as the referenced side.
				if fk.RefRel == base && view.HasColumns(fk.RefCols) {
					g.addForeignKey(fk, RelID{}, viewID)
				}
			}
		}
	}
}

// transitiveBases resolves a view's base relations recursively, guarding
// against cycles.
func transitiveBases(view RelID, viewBases map[RelID][]RelID) []RelID {
	var out []RelID
	seen := map[RelID]bool{view: true}
	queue := slices.Clone(viewBases[view])
	for depth := 0; len(queue) > 0 && depth < maxViewDepth; depth++ {
		var next []RelID
		for _, base := range queue {
			if seen[base] {
				continue
			}
			seen[base] = true
			out = append(out, base)
			next = append(next, viewBases[base]...)
		}
		queue = next
	}
	return out
}

// addManyToMany detects junction tables whose composite primary key contains
// two foreign keys, and records the resulting edges on both endpoints.
func (g *graph) addManyToMany(fks []ForeignKey) {
	byJunction := make(map[RelID][]ForeignKey)
	for _, fk := range fks {
		junction, ok := g.builder.Relations[fk.Rel]
		if !ok || len(junction.PrimaryKey) == 0 {
			continue
		}
		if !subsetOf(fk.Columns, junction.PrimaryKey) {
			continue
		}
		byJunction[fk.Rel] = append(byJunction[fk.Rel], fk)
	}
	for junctionID, links := range byJunction {
		if len(links) < 2 {
			continue
		}
		for _, from := range links {
			for _, to := range links {
				if from.Name == to.Name || from.RefRel == junctionID || to.RefRel == junctionID {
					continue
				}
				a, okA := g.builder.Relations[from.RefRel]
				b, okB := g.builder.Relations[to.RefRel]
				if !okA || !okB {
					continue
				}
				a.Outgoing = append(a.Outgoing, &Relationship{
					Name:            to.Name,
					Source:          a.ID,
					Target:          b.ID,
					Cardinality:     ToMany,
					FK:              to,
					M2M:             true,
					Junction:        junctionID,
					SourceFK:        from,
					SelfReferential: a.ID == b.ID,
				})
			}
		}
	}
}

func subsetOf(sub, super []string) bool {
	set := make(map[string]struct{}, len(super))
	for _, v := range super {
		set[v] = struct{}{}
	}
	for _, v := range sub {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}

// dedupeEdges removes edges made identical by view propagation across several
// base relations.
func dedupeEdges(in []*Relationship) []*Relationship {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, e := range in {
		key := e.edgeKey()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}

func (r *Relationship) edgeKey() string {
	return r.Source.String() + "|" + r.Target.String() + "|" + r.Name +
		"|" + r.Cardinality.String() + "|" + r.FK.Name + "|" +
		r.Junction.String() + "|" + r.SourceFK.Name
}
