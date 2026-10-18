package schema

import (
	"errors"
	"testing"
)

func rel(schema, name string, kind RelationKind, cols ...string) CatalogRelation {
	r := CatalogRelation{Schema: schema, Name: name, Kind: kind}
	for i, c := range cols {
		r.Columns = append(r.Columns, CatalogColumn{Name: c, Position: i + 1, TypeName: "text"})
	}
	return r
}

func id(schema, name string) RelID { return RelID{Schema: schema, Name: name} }

func pk(name string, r RelID, cols ...string) CatalogConstraint {
	return CatalogConstraint{Name: name, Type: 'p', Rel: r, Columns: cols}
}

func unique(name string, r RelID, cols ...string) CatalogConstraint {
	return CatalogConstraint{Name: name, Type: 'u', Rel: r, Columns: cols}
}

func fk(name string, r RelID, cols []string, ref RelID, refCols []string) CatalogConstraint {
	return CatalogConstraint{Name: name, Type: 'f', Rel: r, Columns: cols, RefRel: ref, RefCols: refCols}
}

func soSchema() string { return "public" }

func buildCatalog(t *testing.T, cat Catalog) *Snapshot {
	t.Helper()
	return Build(1, []string{soSchema()}, cat)
}

func TestForeignKeyDirections(t *testing.T) {
	films, directors := id("public", "films"), id("public", "directors")
	snap := buildCatalog(t, Catalog{
		Relations: []CatalogRelation{
			rel("public", "directors", KindTable, "id", "last_name"),
			rel("public", "films", KindTable, "id", "director_id", "title"),
		},
		Constraints: []CatalogConstraint{
			pk("directors_pkey", directors, "id"),
			pk("films_pkey", films, "id"),
			fk("films_director_id_fkey", films, []string{"director_id"}, directors, []string{"id"}),
		},
	})

	f, _ := snap.Relation("films")
	d, _ := snap.Relation("directors")

	toDirector, err := f.ResolveEmbed("directors", "")
	if err != nil {
		t.Fatalf("films -> directors: %v", err)
	}
	if toDirector.Cardinality != ToOne {
		t.Errorf("films -> directors cardinality = %v, want to-one", toDirector.Cardinality)
	}
	if !toDirector.Forward {
		t.Error("films should be the referencing side of the FK")
	}

	toFilms, err := d.ResolveEmbed("films", "")
	if err != nil {
		t.Fatalf("directors -> films: %v", err)
	}
	if toFilms.Cardinality != ToMany {
		t.Errorf("directors -> films cardinality = %v, want to-many", toFilms.Cardinality)
	}
}

func TestOneToOneWhenForeignKeyIsUnique(t *testing.T) {
	films, specs := id("public", "films"), id("public", "technical_specs")
	snap := buildCatalog(t, Catalog{
		Relations: []CatalogRelation{
			rel("public", "films", KindTable, "id"),
			rel("public", "technical_specs", KindTable, "id", "film_id", "camera"),
		},
		Constraints: []CatalogConstraint{
			pk("films_pkey", films, "id"),
			pk("specs_pkey", specs, "id"),
			unique("specs_film_id_key", specs, "film_id"),
			fk("specs_film_id_fkey", specs, []string{"film_id"}, films, []string{"id"}),
		},
	})

	f, _ := snap.Relation("films")
	embed, err := f.ResolveEmbed("technical_specs", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if embed.Cardinality != ToOne {
		t.Errorf("films -> technical_specs cardinality = %v, want to-one", embed.Cardinality)
	}
}

func TestManyToManyViaJunction(t *testing.T) {
	films, actors, roles := id("public", "films"), id("public", "actors"), id("public", "roles")
	snap := buildCatalog(t, Catalog{
		Relations: []CatalogRelation{
			rel("public", "films", KindTable, "id", "title"),
			rel("public", "actors", KindTable, "id", "last_name"),
			rel("public", "roles", KindTable, "film_id", "actor_id", "character"),
		},
		Constraints: []CatalogConstraint{
			pk("films_pkey", films, "id"),
			pk("actors_pkey", actors, "id"),
			pk("roles_pkey", roles, "film_id", "actor_id"),
			fk("roles_film_id_fkey", roles, []string{"film_id"}, films, []string{"id"}),
			fk("roles_actor_id_fkey", roles, []string{"actor_id"}, actors, []string{"id"}),
		},
	})

	f, _ := snap.Relation("films")
	embed, err := f.ResolveEmbed("actors", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !embed.M2M {
		t.Error("films -> actors should be a many-to-many edge")
	}
	if embed.Cardinality != ToMany {
		t.Errorf("cardinality = %v, want to-many", embed.Cardinality)
	}
	if embed.Junction != roles {
		t.Errorf("junction = %v, want roles", embed.Junction)
	}
	if embed.FK.Name != "roles_actor_id_fkey" {
		t.Errorf("FK = %q, want the junction's key to the target", embed.FK.Name)
	}
	if embed.SourceFK.Name != "roles_film_id_fkey" {
		t.Errorf("source FK = %q, want the junction's key back to films", embed.SourceFK.Name)
	}

	// The junction is still directly embeddable.
	if _, err := f.ResolveEmbed("roles", ""); err != nil {
		t.Fatalf("films -> roles should remain embeddable: %v", err)
	}
}

func TestAmbiguousEmbedding(t *testing.T) {
	orders, addresses := id("public", "orders"), id("public", "addresses")
	snap := buildCatalog(t, Catalog{
		Relations: []CatalogRelation{
			rel("public", "addresses", KindTable, "id", "name"),
			rel("public", "orders", KindTable, "id", "billing_address_id", "shipping_address_id"),
		},
		Constraints: []CatalogConstraint{
			pk("addresses_pkey", addresses, "id"),
			pk("orders_pkey", orders, "id"),
			fk("billing", orders, []string{"billing_address_id"}, addresses, []string{"id"}),
			fk("shipping", orders, []string{"shipping_address_id"}, addresses, []string{"id"}),
		},
	})

	o, _ := snap.Relation("orders")
	_, err := o.ResolveEmbed("addresses", "")
	if err == nil {
		t.Fatal("expected an ambiguous embedding error")
	}
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error type = %T, want *SchemaError", err)
	}
	if schemaErr.Code != "PGRST201" {
		t.Errorf("code = %q, want PGRST201", schemaErr.Code)
	}

	billing, err := o.ResolveEmbed("addresses", "billing")
	if err != nil {
		t.Fatalf("resolve with hint: %v", err)
	}
	if billing.FK.Name != "billing" {
		t.Errorf("resolved FK = %q, want billing", billing.FK.Name)
	}

	if _, err := o.ResolveEmbed("addresses", "nope"); err == nil || !errors.As(err, &schemaErr) || schemaErr.Code != "PGRST200" {
		t.Fatalf("unknown hint should yield PGRST200, got %v", err)
	}
}

func TestUnknownTargetIsPGRST200(t *testing.T) {
	films := id("public", "films")
	snap := buildCatalog(t, Catalog{
		Relations:   []CatalogRelation{rel("public", "films", KindTable, "id")},
		Constraints: []CatalogConstraint{pk("films_pkey", films, "id")},
	})
	f, _ := snap.Relation("films")
	_, err := f.ResolveEmbed("unicorns", "")
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) || schemaErr.Code != "PGRST200" {
		t.Fatalf("error = %v, want PGRST200", err)
	}
}

func TestViewInheritsForeignKey(t *testing.T) {
	people, cities := id("public", "people"), id("public", "cities")
	view := id("public", "people_view")
	snap := buildCatalog(t, Catalog{
		Relations: []CatalogRelation{
			rel("public", "cities", KindTable, "id", "name"),
			rel("public", "people", KindTable, "id", "name", "city_id"),
			rel("public", "people_view", KindView, "id", "name", "city_id"),
		},
		Constraints: []CatalogConstraint{
			pk("cities_pkey", cities, "id"),
			pk("people_pkey", people, "id"),
			fk("people_city_id_fkey", people, []string{"city_id"}, cities, []string{"id"}),
		},
		ViewSources: []CatalogViewSource{{View: view, Base: people}},
	})

	v, _ := snap.Relation("people_view")
	embed, err := v.ResolveEmbed("cities", "")
	if err != nil {
		t.Fatalf("view -> cities: %v", err)
	}
	if embed.Forward != true || embed.Target != cities {
		t.Errorf("view edge = %+v, want a forward edge to cities", embed)
	}
}

func TestViewDoesNotInheritAbsentColumns(t *testing.T) {
	people, cities := id("public", "people"), id("public", "cities")
	view := id("public", "people_view")
	snap := buildCatalog(t, Catalog{
		Relations: []CatalogRelation{
			rel("public", "cities", KindTable, "id", "name"),
			rel("public", "people", KindTable, "id", "name", "city_id"),
			// The view hides city_id, so the foreign key cannot be followed.
			rel("public", "people_view", KindView, "id", "name"),
		},
		Constraints: []CatalogConstraint{
			pk("cities_pkey", cities, "id"),
			pk("people_pkey", people, "id"),
			fk("people_city_id_fkey", people, []string{"city_id"}, cities, []string{"id"}),
		},
		ViewSources: []CatalogViewSource{{View: view, Base: people}},
	})

	v, _ := snap.Relation("people_view")
	if _, err := v.ResolveEmbed("cities", ""); err == nil {
		t.Fatal("expected no relationship when the view omits the foreign key column")
	}
}

func TestExposedSchemas(t *testing.T) {
	snap := Build(3, []string{"api"}, Catalog{
		Relations: []CatalogRelation{
			rel("api", "things", KindTable, "id"),
			rel("internal", "secrets", KindTable, "id"),
		},
	})
	if rel, ok := snap.Relation("things"); !ok || !rel.Exposed {
		t.Error("api.things should be exposed and routable")
	}
	if _, ok := snap.Relation("secrets"); ok {
		t.Error("internal.secrets must not be routable")
	}
	internal := snap.Relations[id("internal", "secrets")]
	if internal == nil || internal.Exposed {
		t.Error("internal.secrets should be present for embedding but not exposed")
	}
	if snap.Generation != 3 {
		t.Errorf("Generation = %d, want 3", snap.Generation)
	}
}
