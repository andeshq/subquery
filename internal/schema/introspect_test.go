package schema

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andeshq/subquery/test/dbtest"
)

// TestMain provisions the package's own fixture database.
func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestIntrospectFixtureSchema(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool, discardLogger(), Options{
		Schemas: []string{"public"},
		Exposed: []string{"public"},
	})
	if err := store.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	snap := store.Snapshot()
	if snap == nil {
		t.Fatal("snapshot is nil after a successful reload")
	}

	films, ok := snap.Relation("films")
	if !ok {
		t.Fatal("films is not exposed")
	}
	if got := films.PrimaryKey; len(got) != 1 || got[0] != "id" {
		t.Errorf("films primary key = %v, want [id]", got)
	}
	if _, ok := films.Column("rating"); !ok {
		t.Error("films.rating is missing")
	}

	directors, _ := snap.Relation("directors")
	toDirector, err := films.ResolveEmbed("directors", "")
	if err != nil {
		t.Fatalf("films -> directors: %v", err)
	}
	if toDirector.Cardinality != ToOne || !toDirector.Forward {
		t.Errorf("films -> directors = %+v, want a forward to-one edge", toDirector)
	}
	toFilms, err := directors.ResolveEmbed("films", "")
	if err != nil {
		t.Fatalf("directors -> films: %v", err)
	}
	if toFilms.Cardinality != ToMany {
		t.Errorf("directors -> films cardinality = %v, want to-many", toFilms.Cardinality)
	}

	// technical_specs has a unique foreign key, so films embeds it as an object.
	toSpecs, err := films.ResolveEmbed("technical_specs", "")
	if err != nil {
		t.Fatalf("films -> technical_specs: %v", err)
	}
	if toSpecs.Cardinality != ToOne {
		t.Errorf("films -> technical_specs cardinality = %v, want to-one", toSpecs.Cardinality)
	}

	// roles is a junction whose composite primary key holds both foreign keys.
	toActors, err := films.ResolveEmbed("actors", "")
	if err != nil {
		t.Fatalf("films -> actors: %v", err)
	}
	if !toActors.M2M || toActors.Cardinality != ToMany {
		t.Errorf("films -> actors = %+v, want a to-many many-to-many edge", toActors)
	}
	if toActors.Junction != id("public", "roles") {
		t.Errorf("junction = %v, want public.roles", toActors.Junction)
	}
	roles, _ := snap.Relation("roles")
	if len(roles.PrimaryKey) != 2 {
		t.Errorf("roles primary key = %v, want a composite key", roles.PrimaryKey)
	}

	// films_view projects director_id, so it inherits the foreign key.
	view, ok := snap.Relation("films_view")
	if !ok || view.Kind != KindView {
		t.Fatalf("films_view = %+v, want an exposed view", view)
	}
	viewEmbed, err := view.ResolveEmbed("directors", "")
	if err != nil {
		t.Fatalf("films_view -> directors: %v", err)
	}
	if viewEmbed.Target != id("public", "directors") {
		t.Errorf("films_view -> directors target = %v", viewEmbed.Target)
	}
}

func TestStoreReloadsOnNotify(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool, discardLogger(), Options{
		Schemas:        []string{"public"},
		Exposed:        []string{"public"},
		Channel:        "subquery_test",
		ChannelEnabled: true,
	})
	store.Start(t.Context())
	defer store.Close()

	if err := store.Reload(t.Context()); err != nil {
		t.Fatalf("initial Reload: %v", err)
	}
	before := store.Generation()

	if _, err := pool.Exec(t.Context(), "notify subquery_test, 'reload schema'"); err != nil {
		t.Fatalf("notify: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if store.Generation() > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("generation stayed at %d; the listener did not reload", before)
}
