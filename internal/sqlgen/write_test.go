package sqlgen

import (
	"errors"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

// writeRelationSnapshot models a table with an identity key and a defaulted
// column, plus a natural-key table for upserts.
func writeRelationSnapshot(t testing.TB) *schema.Snapshot {
	t.Helper()
	notes := schema.RelID{Schema: "public", Name: "notes"}
	products := schema.RelID{Schema: "public", Name: "products"}
	return schema.Build(1, []string{"public"}, schema.Catalog{
		Relations: []schema.CatalogRelation{
			{Schema: "public", Name: "notes", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("id", "bigint", `"pg_catalog"."int8"`),
				column("title", "text", `"pg_catalog"."text"`),
				column("score", "integer", `"pg_catalog"."int4"`),
			}},
			{Schema: "public", Name: "products", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("sku", "text", `"pg_catalog"."text"`),
				column("name", "text", `"pg_catalog"."text"`),
				column("price", "numeric", `"pg_catalog"."numeric"`),
			}},
		},
		Constraints: []schema.CatalogConstraint{
			{Name: "notes_pkey", Type: 'p', Rel: notes, Columns: []string{"id"}},
			{Name: "products_pkey", Type: 'p', Rel: products, Columns: []string{"sku"}},
		},
	})
}

func notesRelation(t *testing.T, snap *schema.Snapshot) *schema.Relation {
	t.Helper()
	rel, ok := snap.Relation("notes")
	if !ok {
		t.Fatal("notes is not in the snapshot")
	}
	return rel
}

func TestWriteInsertSQL(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelation(t, snap)
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteInsert,
		Columns: []string{"title", "score"},
		Payload: []byte(`[{"title":"a","score":1}]`),
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertContains(t, &Statement{SQL: stmt.SQL, Args: stmt.Args},
		`insert into "public"."notes" ("title", "score")`,
		`select "title", "score" from json_populate_recordset(null::"public"."notes", $1::json)`,
	)
	if stmt.Scan {
		t.Error("a minimal insert must not scan")
	}
}

func TestWriteInsertConflictTargetDefaultsToPrimaryKey(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel, _ := snap.Relation("products")
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:       WriteInsert,
		Columns:    []string{"sku", "name"},
		Resolution: ResolutionMerge,
		Payload:    []byte(`[{"sku":"A","name":"n"}]`),
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(stmt.SQL, `on conflict ("sku") do update set "sku" = excluded."sku", "name" = excluded."name"`) {
		t.Errorf("SQL = %s", stmt.SQL)
	}
}

func TestWriteIgnoreDuplicates(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel, _ := snap.Relation("products")
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:       WriteInsert,
		Columns:    []string{"sku", "name"},
		Resolution: ResolutionIgnore,
		Payload:    []byte(`[{"sku":"A","name":"n"}]`),
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(stmt.SQL, `on conflict ("sku") do nothing`) {
		t.Errorf("SQL = %s", stmt.SQL)
	}
}

func TestWriteUpsertSQL(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel, _ := snap.Relation("products")
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteUpsert,
		Columns: []string{"sku", "name", "price"},
		Payload: []byte(`[{"sku":"A","name":"n","price":1}]`),
	}, WriteOptions{Return: ReturnMinimal})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertContains(t, &Statement{SQL: stmt.SQL, Args: stmt.Args},
		`insert into "public"."products"`,
		`on conflict ("sku") do update set`,
	)
	// A minimal PUT reports 204, so its outcome is unobservable and no
	// inserted flag is needed.
	if stmt.Scan || stmt.Inserted {
		t.Errorf("a minimal upsert should not scan: %+v", stmt)
	}
}

func TestWriteUpsertHeadersOnly(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel, _ := snap.Relation("notes")
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteUpsert,
		Columns: []string{"id", "title"},
		Payload: []byte(`[{"id":1,"title":"a"}]`),
	}, WriteOptions{Return: ReturnHeadersOnly})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertContains(t, &Statement{SQL: stmt.SQL, Args: stmt.Args},
		`returning "id", (xmax = 0) as pgrst_inserted`,
	)
	if len(stmt.ReturnedColumns) != 1 || stmt.ReturnedColumns[0] != "id" {
		t.Errorf("ReturnedColumns = %v", stmt.ReturnedColumns)
	}
}

func TestWriteUpdateSQL(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelation(t, snap)
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteUpdate,
		Columns: []string{"score"},
		Payload: []byte(`{"score":9}`),
		Where: []query.Filter{&query.Comparison{
			Column: "id", Op: query.OpEq, Values: []string{"1"},
		}},
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertContains(t, &Statement{SQL: stmt.SQL, Args: stmt.Args},
		`update "public"."notes" as pgrst_src`,
		`set "score" = pgrst_body."score"`,
		`from (select * from json_populate_record(null::"public"."notes", $1::json)) as pgrst_body`,
		`where pgrst_src."id" = ($2::text)::"pg_catalog"."int8"`,
	)
	if len(stmt.Args) != 2 || stmt.Args[0] != `{"score":9}` || stmt.Args[1] != "1" {
		t.Errorf("Args = %#v, want the payload first", stmt.Args)
	}
}

func TestWriteDeleteSQL(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelation(t, snap)
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind: WriteDelete,
		Where: []query.Filter{&query.Comparison{
			Column: "title", Op: query.OpEq, Values: []string{"gone"},
		}},
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(stmt.Args) != 1 || stmt.Args[0] != "gone" {
		t.Errorf("Args = %#v", stmt.Args)
	}
	assertContains(t, &Statement{SQL: stmt.SQL}, `delete from "public"."notes" as pgrst_src`, `where pgrst_src."title" = ($1::text)::"pg_catalog"."text"`)
}

func TestWriteRepresentation(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelation(t, snap)
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteInsert,
		Columns: []string{"title"},
		Payload: []byte(`[{"title":"a"}]`),
		Select:  []query.SelectItem{{Column: "title"}},
	}, WriteOptions{Return: ReturnRepresentation})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertContains(t, &Statement{SQL: stmt.SQL, Args: stmt.Args},
		`with pgrst_write as (`,
		`returning "id", "title", "score"`,
		`select pgrst_src."title" from pgrst_write as pgrst_src`,
		`json_agg(row_to_json(pgrst_result))`,
		`count(*) as returned`,
	)
	if !stmt.Representation {
		t.Error("Representation = false")
	}
}

func TestWriteUnknownColumn(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelation(t, snap)
	_, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteInsert,
		Columns: []string{"nope"},
		Payload: []byte(`[{"nope":1}]`),
	}, WriteOptions{})
	var columnErr *ColumnError
	if !errors.As(err, &columnErr) {
		t.Fatalf("error = %v, want *ColumnError", err)
	}
}

func TestWriteOnConflictWithoutTarget(t *testing.T) {
	snap := schema.Build(1, []string{"public"}, schema.Catalog{
		Relations: []schema.CatalogRelation{{
			Schema: "public", Name: "log", Kind: schema.KindTable,
			Columns: []schema.CatalogColumn{column("message", "text", `"pg_catalog"."text"`)},
		}},
	})
	rel, _ := snap.Relation("log")
	_, err := Write(snap, rel, &WriteQuery{
		Kind:       WriteInsert,
		Columns:    []string{"message"},
		Resolution: ResolutionMerge,
		Payload:    []byte(`[{"message":"x"}]`),
	}, WriteOptions{})
	var columnErr *ColumnError
	if !errors.As(err, &columnErr) {
		t.Fatalf("error = %v, want *ColumnError", err)
	}
}

// TestWritePayloadIsBound asserts the payload never reaches the SQL text.
func TestWritePayloadIsBound(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelation(t, snap)
	payload := `[{"title":"x'); drop table notes; --"}]`
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteInsert,
		Columns: []string{"title"},
		Payload: []byte(payload),
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(stmt.SQL, "drop table") {
		t.Fatalf("payload leaked into SQL:\n%s", stmt.SQL)
	}
	if len(stmt.Args) != 1 || stmt.Args[0] != payload {
		t.Fatalf("Args = %#v", stmt.Args)
	}
}
