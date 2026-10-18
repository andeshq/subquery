package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

func TestParsePreferencesTable(t *testing.T) {
	tests := []struct {
		header  string
		returns sqlgen.ReturnMode
	}{
		{"", sqlgen.ReturnMinimal},
		{"return=minimal", sqlgen.ReturnMinimal},
		{"return=headers-only", sqlgen.ReturnHeadersOnly},
		{"return=representation", sqlgen.ReturnRepresentation},
		{"RETURN=REPRESENTATION", sqlgen.ReturnMinimal}, // values are case-sensitive
		{"return=bogus", sqlgen.ReturnMinimal},
		{"return=representation, count=exact", sqlgen.ReturnRepresentation},
	}
	for _, tc := range tests {
		prefs, apiErr := parsePreferences(tc.header, config.TxCommit)
		if apiErr != nil {
			t.Fatalf("parsePreferences(%q): %v", tc.header, apiErr)
		}
		if prefs.Return != tc.returns {
			t.Errorf("parsePreferences(%q).Return = %v, want %v", tc.header, prefs.Return, tc.returns)
		}
	}
}

func TestParsePreferencesResolutionAndMissing(t *testing.T) {
	prefs, apiErr := parsePreferences("resolution=merge-duplicates, missing=default, max-affected=5", config.TxCommit)
	if apiErr != nil {
		t.Fatalf("parsePreferences: %v", apiErr)
	}
	if prefs.Resolution != sqlgen.ResolutionMerge {
		t.Errorf("Resolution = %v", prefs.Resolution)
	}
	if !prefs.MissingDefault {
		t.Error("MissingDefault = false")
	}
	if prefs.MaxAffected == nil || *prefs.MaxAffected != 5 {
		t.Errorf("MaxAffected = %v", prefs.MaxAffected)
	}
	if len(prefs.applied) != 3 {
		t.Errorf("applied = %v, want three preferences", prefs.applied)
	}

	prefs, apiErr = parsePreferences("resolution=ignore-duplicates", config.TxCommit)
	if apiErr != nil || prefs.Resolution != sqlgen.ResolutionIgnore {
		t.Errorf("ignore-duplicates = %+v, %v", prefs, apiErr)
	}

	if _, apiErr := parsePreferences("resolution=bogus", config.TxCommit); apiErr != nil {
		t.Errorf("a bogus resolution should be ignored under lenient handling, got %v", apiErr)
	}
	if _, apiErr := parsePreferences("max-affected=x", config.TxCommit); apiErr != nil {
		t.Errorf("a bogus max-affected should be ignored under lenient handling, got %v", apiErr)
	}
}

func TestParsePreferencesStrict(t *testing.T) {
	_, apiErr := parsePreferences("handling=strict, nope, also-nope", config.TxCommit)
	if apiErr == nil {
		t.Fatal("strict handling should reject unknown preferences")
	}
	if apiErr.Code != "PGRST122" || apiErr.Status != http.StatusBadRequest {
		t.Errorf("error = %+v", apiErr)
	}
	// Unknown preferences are tolerated without strict handling.
	if _, apiErr := parsePreferences("nope, also-nope", config.TxCommit); apiErr != nil {
		t.Errorf("lenient handling should ignore unknown preferences, got %v", apiErr)
	}
	// A strict header with only valid preferences passes.
	if _, apiErr := parsePreferences("handling=strict, return=representation", config.TxCommit); apiErr != nil {
		t.Errorf("strict with valid preferences: %v", apiErr)
	}
}

func TestParsePreferencesTransactionEnd(t *testing.T) {
	tests := []struct {
		txEnd config.TxEnd
		want  bool
	}{
		{config.TxCommit, false},
		{config.TxCommitAllowOverride, true},
		{config.TxRollback, false},
		{config.TxRollbackAllowOverride, true},
	}
	for _, tc := range tests {
		prefs, apiErr := parsePreferences("tx=rollback", tc.txEnd)
		if apiErr != nil {
			t.Fatalf("parsePreferences(%q): %v", tc.txEnd, apiErr)
		}
		if prefs.TxRollback != tc.want {
			t.Errorf("db-tx-end %q: TxRollback = %v, want %v", tc.txEnd, prefs.TxRollback, tc.want)
		}
		// Only an honored preference is reported as applied.
		if tc.want && len(prefs.applied) != 1 {
			t.Errorf("db-tx-end %q: applied = %v", tc.txEnd, prefs.applied)
		}
		if !tc.want && len(prefs.applied) != 0 {
			t.Errorf("db-tx-end %q: applied = %v, want none", tc.txEnd, prefs.applied)
		}
	}

	if _, apiErr := parsePreferences("tx=sideways", config.TxCommit); apiErr != nil {
		t.Errorf("an unknown tx value should be ignored leniently, got %v", apiErr)
	}
}

func TestNegotiateAccept(t *testing.T) {
	array := []string{"", "*/*", "application/json", "application/json; charset=utf-8",
		"application/json;q=0.9", "application/vnd.pgrst.array+json", "application/*",
		"text/html, application/json;q=0.5"}
	for _, header := range array {
		shape, err := negotiateAccept(header)
		if err != nil {
			t.Errorf("negotiateAccept(%q) = %v, want an array", header, err)
			continue
		}
		if shape != shapeArray {
			t.Errorf("negotiateAccept(%q) = object, want array", header)
		}
	}
	object := "application/vnd.pgrst.object+json"
	if shape, err := negotiateAccept(object); err != nil || shape != shapeObject {
		t.Errorf("negotiateAccept(%q) = %v, %v, want object", object, shape, err)
	}
	if shape, err := negotiateAccept("text/html, application/vnd.pgrst.object+json"); err != nil || shape != shapeObject {
		t.Errorf("object after an ignored type = %v, %v", shape, err)
	}
	for _, header := range []string{"text/csv", "text/csv; charset=utf-8"} {
		if shape, err := negotiateAccept(header); err != nil || shape != shapeCSV {
			t.Errorf("negotiateAccept(%q) = %v, %v, want csv", header, shape, err)
		}
	}
	rejected := []string{"text/plain", "application/xml", "application/vnd.pgrst.plan"}
	for _, header := range rejected {
		shape, err := negotiateAccept(header)
		if err == nil {
			t.Errorf("negotiateAccept(%q) = nil, want an error", header)
			continue
		}
		if shape != shapeArray {
			t.Errorf("negotiateAccept(%q) selected a shape on rejection", header)
		}
		if err.Code != pgrst.UnsupportedMediaType || err.Status != http.StatusNotAcceptable {
			t.Errorf("negotiateAccept(%q) = %+v", header, err)
		}
	}
}

func TestInsertColumns(t *testing.T) {
	// The payload's keys are the insert list; anything else takes its default.
	if got := insertColumns([]string{"title", "score"}, nil); len(got) != 2 || got[0] != "title" {
		t.Errorf("insertColumns = %v", got)
	}
	// An explicit column list wins.
	if got := insertColumns([]string{"title"}, []string{"title", "score"}); len(got) != 2 {
		t.Errorf("insertColumns with explicit = %v", got)
	}
	if got := insertColumns(nil, nil); len(got) != 0 {
		t.Errorf("insertColumns(nil) = %v", got)
	}
}

func TestWriteStatus(t *testing.T) {
	tests := []struct {
		kind       sqlgen.WriteKind
		mode       sqlgen.ReturnMode
		resolution sqlgen.Resolution
		inserted   bool
		want       int
	}{
		{sqlgen.WriteInsert, sqlgen.ReturnMinimal, sqlgen.ResolutionNone, false, http.StatusCreated},
		{sqlgen.WriteInsert, sqlgen.ReturnRepresentation, sqlgen.ResolutionMerge, true, http.StatusCreated},
		{sqlgen.WriteInsert, sqlgen.ReturnRepresentation, sqlgen.ResolutionMerge, false, http.StatusOK},
		{sqlgen.WriteInsert, sqlgen.ReturnRepresentation, sqlgen.ResolutionIgnore, false, http.StatusCreated},
		{sqlgen.WriteUpsert, sqlgen.ReturnMinimal, sqlgen.ResolutionNone, false, http.StatusNoContent},
		{sqlgen.WriteUpsert, sqlgen.ReturnRepresentation, sqlgen.ResolutionNone, true, http.StatusCreated},
		{sqlgen.WriteUpsert, sqlgen.ReturnRepresentation, sqlgen.ResolutionNone, false, http.StatusOK},
		{sqlgen.WriteUpdate, sqlgen.ReturnMinimal, sqlgen.ResolutionNone, false, http.StatusNoContent},
		{sqlgen.WriteUpdate, sqlgen.ReturnRepresentation, sqlgen.ResolutionNone, false, http.StatusOK},
		{sqlgen.WriteDelete, sqlgen.ReturnMinimal, sqlgen.ResolutionNone, false, http.StatusNoContent},
		{sqlgen.WriteDelete, sqlgen.ReturnRepresentation, sqlgen.ResolutionNone, false, http.StatusOK},
	}
	for _, tc := range tests {
		got := writeStatus(tc.kind, tc.mode, tc.resolution, tc.inserted)
		if got != tc.want {
			t.Errorf("writeStatus(kind=%v, mode=%v, resolution=%v, inserted=%v) = %d, want %d",
				tc.kind, tc.mode, tc.resolution, tc.inserted, got, tc.want)
		}
	}
}

func TestWriteContentRange(t *testing.T) {
	if got := writeContentRange(sqlgen.WriteUpdate, 3); got != "0-2/*" {
		t.Errorf("update range = %q, want 0-2/*", got)
	}
	if got := writeContentRange(sqlgen.WriteUpdate, 0); got != "*/*" {
		t.Errorf("empty update range = %q, want */*", got)
	}
	for _, kind := range []sqlgen.WriteKind{sqlgen.WriteInsert, sqlgen.WriteUpsert, sqlgen.WriteDelete} {
		if got := writeContentRange(kind, 2); got != "*/*" {
			t.Errorf("kind %v range = %q, want */*", kind, got)
		}
	}
}

func TestContentRange(t *testing.T) {
	zero := int64(0)
	ten := int64(10)
	tests := []struct {
		offset, returned int64
		total            *int64
		want             string
	}{
		{0, 3, nil, "0-2/*"},
		{5, 2, nil, "5-6/*"},
		{0, 0, nil, "*/*"},
		{0, 0, &zero, "*/0"},
		{0, 25, &ten, "0-24/10"},
	}
	for _, tc := range tests {
		if got := contentRange(tc.offset, tc.returned, tc.total); got != tc.want {
			t.Errorf("contentRange(%d, %d, %v) = %q, want %q", tc.offset, tc.returned, tc.total, got, tc.want)
		}
	}
}

func TestWwwAuthenticate(t *testing.T) {
	if got := wwwAuthenticate("PGRST301"); got != `Bearer error="invalid_token"` {
		t.Errorf("PGRST301 = %q", got)
	}
	if got := wwwAuthenticate("PGRST303"); got != `Bearer error="invalid_token"` {
		t.Errorf("PGRST303 = %q", got)
	}
	if got := wwwAuthenticate("PGRST302"); got != "Bearer" {
		t.Errorf("PGRST302 = %q, want a bare scheme", got)
	}
}

func TestAPIErrorFromSchema(t *testing.T) {
	tests := []struct {
		code   string
		status int
	}{
		{"PGRST205", http.StatusNotFound},
		{"PGRST202", http.StatusNotFound},
		{"PGRST201", http.StatusMultipleChoices},
		{"PGRST203", http.StatusMultipleChoices},
		{"PGRST200", http.StatusBadRequest},
		{"unexpected", http.StatusBadRequest},
	}
	for _, tc := range tests {
		apiErr := apiErrorFromSchema(&schema.SchemaError{Code: tc.code, Message: "m"})
		if apiErr.Status != tc.status || apiErr.Code != tc.code {
			t.Errorf("%s mapped to %+v, want status %d", tc.code, apiErr, tc.status)
		}
	}
}

func TestFilterEquality(t *testing.T) {
	where := []query.Filter{
		&query.Comparison{Column: "id", Op: query.OpEq, Values: []string{"4"}},
		&query.Comparison{Column: "title", Op: query.OpLike, Values: []string{"x"}},
		&query.Logical{Op: query.LogicalOr},
	}
	if got, ok := filterEquality(where, "id"); !ok || got != "4" {
		t.Errorf("filterEquality(id) = %q, %v", got, ok)
	}
	if _, ok := filterEquality(where, "title"); ok {
		t.Error("a non-equality filter must not resolve")
	}
	if _, ok := filterEquality(where, "absent"); ok {
		t.Error("a missing filter must not resolve")
	}
	if _, ok := filterEquality(nil, "id"); ok {
		t.Error("an empty filter list must not resolve")
	}
	negated := []query.Filter{&query.Comparison{Column: "id", Op: query.OpEq, Negated: true, Values: []string{"4"}}}
	if _, ok := filterEquality(negated, "id"); ok {
		t.Error("a negated equality must not resolve")
	}
	inList := []query.Filter{&query.Comparison{Column: "id", Op: query.OpIn, Values: []string{"1", "2"}}}
	if _, ok := filterEquality(inList, "id"); ok {
		t.Error("an in-list must not resolve as equality")
	}
}

func TestScalarString(t *testing.T) {
	object := map[string]any{"s": "x", "n": float64(4), "b": true, "z": nil}
	tests := []struct {
		key   string
		want  string
		found bool
	}{
		{"s", "x", true},
		{"n", "4", true},
		{"b", "true", true},
		{"z", "", true},
		{"absent", "", false},
	}
	for _, tc := range tests {
		got, found := scalarString(object, tc.key)
		if found != tc.found || got != tc.want {
			t.Errorf("scalarString(%q) = (%q, %v), want (%q, %v)", tc.key, got, found, tc.want, tc.found)
		}
	}
}

func TestSameKeySet(t *testing.T) {
	a := map[string]bool{"x": true, "y": true}
	if !sameKeySet(a, map[string]bool{"y": true, "x": true}) {
		t.Error("equivalent key sets should match")
	}
	if sameKeySet(a, map[string]bool{"x": true}) {
		t.Error("different sizes should not match")
	}
	if sameKeySet(a, map[string]bool{"x": true, "z": true}) {
		t.Error("different members should not match")
	}
}

// TestParsePayloadRejectsHeterogeneousArrays asserts a bulk payload whose
// elements do not all carry the same keys is refused, as PostgREST refuses it.
func TestParsePayloadRejectsHeterogeneousArrays(t *testing.T) {
	_, apiErr := parsePayload([]byte(`[{"a":1},{"a":2,"b":3}]`), true)
	if apiErr == nil {
		t.Fatal("expected a heterogeneous array to be rejected")
	}
	if apiErr.Code != pgrst.InvalidBody || apiErr.Status != http.StatusBadRequest {
		t.Errorf("error = %+v", apiErr)
	}
	for _, raw := range []string{`[{"a":1},{"a":2}]`, `{"a":1}`, `[]`} {
		if _, apiErr := parsePayload([]byte(raw), true); apiErr != nil {
			t.Errorf("parsePayload(%s) = %v, want nil", raw, apiErr)
		}
	}
}

func TestJSONScalarText(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{`"quoted"`, "quoted"},
		{`4`, "4"},
		{`true`, "true"},
		{`null`, "null"},
		{` "spaced" `, "spaced"},
		{`"esc\"aped"`, `esc"aped`},
	}
	for _, tc := range tests {
		if got := jsonScalarText([]byte(tc.raw)); got != tc.want {
			t.Errorf("jsonScalarText(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIdentityTextAndRequestSchemes(t *testing.T) {
	if got := identityText(schema.RelID{Schema: "public", Name: "films"}); got != `"public"."films"` {
		t.Errorf("identityText = %q", got)
	}
	if got := identityText(schema.RelID{Schema: "Weird Schema", Name: `Odd"Name`}); got != `"Weird Schema"."Odd""Name"` {
		t.Errorf("identityText did not escape: %q", got)
	}

	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := requestSchemes(plain); len(got) != 1 || got[0] != "http" {
		t.Errorf("plain schemes = %v", got)
	}
	forwarded := httptest.NewRequest(http.MethodGet, "/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	if got := requestSchemes(forwarded); len(got) != 1 || got[0] != "https" {
		t.Errorf("forwarded schemes = %v", got)
	}
	list := httptest.NewRequest(http.MethodGet, "/", nil)
	list.Header.Set("X-Forwarded-Proto", "https, http")
	if got := requestSchemes(list); len(got) != 1 || got[0] != "https" {
		t.Errorf("multi-value schemes = %v", got)
	}
	tls := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	if got := requestSchemes(tls); len(got) != 1 || got[0] != "https" {
		t.Errorf("tls schemes = %v", got)
	}
}

func TestNumeric(t *testing.T) {
	for _, ok := range []string{"", "0", "123"} {
		if !numeric(ok) {
			t.Errorf("numeric(%q) = false", ok)
		}
	}
	for _, bad := range []string{"-1", "1a", "a", "1.5", " "} {
		if numeric(bad) {
			t.Errorf("numeric(%q) = true", bad)
		}
	}
}

func TestUnnamedArg(t *testing.T) {
	named := &schema.Function{Args: []schema.FunctionArg{{Name: "a", Mode: schema.ArgIn, CastType: "text"}}}
	if _, ok := unnamedArg(named); ok {
		t.Error("a named argument is not the unnamed form")
	}
	unnamed := &schema.Function{Args: []schema.FunctionArg{{Name: "", Mode: schema.ArgIn, CastType: "jsonb"}}}
	arg, ok := unnamedArg(unnamed)
	if !ok || arg.CastType != "jsonb" {
		t.Errorf("unnamedArg = %+v, %v", arg, ok)
	}
	two := &schema.Function{Args: []schema.FunctionArg{
		{Name: "", Mode: schema.ArgIn, CastType: "jsonb"},
		{Name: "", Mode: schema.ArgIn, CastType: "text"},
	}}
	if _, ok := unnamedArg(two); ok {
		t.Error("two arguments are not the single unnamed form")
	}
}
