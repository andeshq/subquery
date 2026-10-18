package query

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) *Query {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	q, err := Parse(values)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	return q
}

func TestParseSelect(t *testing.T) {
	q := mustParse(t, "select=id,fullName:full_name,salary::text,json_data->>blood_type,json_data->phones->0->>number")
	if len(q.Select) != 5 {
		t.Fatalf("len(Select) = %d, want 5", len(q.Select))
	}
	if q.Select[0].Column != "id" {
		t.Errorf("Select[0] = %+v", q.Select[0])
	}
	if q.Select[1].Column != "full_name" || q.Select[1].Alias != "fullName" {
		t.Errorf("Select[1] = %+v, want alias fullName over full_name", q.Select[1])
	}
	if q.Select[2].Column != "salary" || q.Select[2].Cast != "text" {
		t.Errorf("Select[2] = %+v, want a text cast", q.Select[2])
	}
	if q.Select[3].Column != "json_data" || len(q.Select[3].Path) != 1 ||
		q.Select[3].Path[0].Key != "blood_type" || !q.Select[3].Path[0].Text {
		t.Errorf("Select[3] = %+v", q.Select[3])
	}
	if len(q.Select[4].Path) != 3 {
		t.Fatalf("Select[4].Path = %+v, want three steps", q.Select[4].Path)
	}
	if !q.Select[4].Path[1].Numeric || q.Select[4].Path[1].Key != "0" {
		t.Errorf("Select[4].Path[1] = %+v, want a numeric index", q.Select[4].Path[1])
	}
	if !q.Select[4].Path[2].Text || q.Select[4].Path[2].Key != "number" {
		t.Errorf("Select[4].Path[2] = %+v", q.Select[4].Path[2])
	}
}

func TestParseSelectStarAndCastAfterAlias(t *testing.T) {
	q := mustParse(t, "select=*,nick:name::text")
	if !q.Select[0].Star {
		t.Errorf("Select[0] = %+v, want a star", q.Select[0])
	}
	if q.Select[1].Alias != "nick" || q.Select[1].Column != "name" || q.Select[1].Cast != "text" {
		t.Errorf("Select[1] = %+v", q.Select[1])
	}
}

func TestParseOrder(t *testing.T) {
	q := mustParse(t, "order=age.desc.nullslast,height.asc")
	if len(q.Order) != 2 {
		t.Fatalf("len(Order) = %d", len(q.Order))
	}
	if q.Order[0].Column != "age" || !q.Order[0].Desc || q.Order[0].Nulls != "last" {
		t.Errorf("Order[0] = %+v", q.Order[0])
	}
	if q.Order[1].Column != "height" || q.Order[1].Desc || q.Order[1].Nulls != "" {
		t.Errorf("Order[1] = %+v", q.Order[1])
	}
}

func TestParseLimits(t *testing.T) {
	q := mustParse(t, "limit=15&offset=30")
	if q.Limit == nil || *q.Limit != 15 || q.Offset != 30 {
		t.Errorf("Limit = %v, Offset = %d", q.Limit, q.Offset)
	}
	if _, err := Parse(url.Values{"limit": {"-1"}}); err == nil {
		t.Error("negative limit should be rejected")
	}
	if _, err := Parse(url.Values{"limit": {"abc"}}); err == nil {
		t.Error("non-numeric limit should be rejected")
	}
}

func TestParseComparisonOperators(t *testing.T) {
	q := mustParse(t, "age=gte.18&student=is.true&name=like.*an*&id=in.(1,2,3)&deleted=not.is.null")
	if len(q.Where) != 5 {
		t.Fatalf("len(Where) = %d, want 5", len(q.Where))
	}
	byName := map[string]*Comparison{}
	for _, f := range q.Where {
		c, ok := f.(*Comparison)
		if !ok {
			t.Fatalf("filter %T is not a comparison", f)
		}
		byName[c.Column] = c
	}

	if c := byName["age"]; c.Op != OpGte || c.Values[0] != "18" {
		t.Errorf("age filter = %+v", c)
	}
	if c := byName["student"]; c.Op != OpIs || c.Values[0] != "true" {
		t.Errorf("student filter = %+v", c)
	}
	if c := byName["name"]; c.Op != OpLike || c.Values[0] != "%an%" {
		t.Errorf("name filter = %+v, want %%an%%", c)
	}
	if c := byName["id"]; c.Op != OpIn || len(c.Values) != 3 || c.Values[2] != "3" {
		t.Errorf("id filter = %+v", c)
	}
	if c := byName["deleted"]; c.Op != OpIs || !c.Negated || c.Values[0] != "null" {
		t.Errorf("deleted filter = %+v", c)
	}
}

func TestParseQuotedInList(t *testing.T) {
	q := mustParse(t, "a=in.(%22hi,there%22,%22yes,you%22)")
	c := q.Where[0].(*Comparison)
	if len(c.Values) != 2 || c.Values[0] != "hi,there" || c.Values[1] != "yes,you" {
		t.Errorf("Values = %q, want commas preserved inside quotes", c.Values)
	}
}

func TestParseModifiersAndFTS(t *testing.T) {
	q := mustParse(t, "last_name=like(any).{O*,P*}&tsv=fts(french).amusant&x=eq(all).{1,2}")
	byName := map[string]*Comparison{}
	for _, f := range q.Where {
		c := f.(*Comparison)
		byName[c.Column] = c
	}
	if c := byName["last_name"]; c.Modifier != ModAny || len(c.Values) != 2 || c.Values[0] != "O%" {
		t.Errorf("last_name = %+v", c)
	}
	if c := byName["tsv"]; c.Op != OpFts || c.Lang != "french" || c.Values[0] != "amusant" {
		t.Errorf("tsv = %+v", c)
	}
	if c := byName["x"]; c.Modifier != ModAll || len(c.Values) != 2 {
		t.Errorf("x = %+v", c)
	}
}

func TestParseLogicalFilters(t *testing.T) {
	q := mustParse(t, "or=(age.lt.18,age.gt.21)&grade=gte.90")
	if len(q.Where) != 2 {
		t.Fatalf("len(Where) = %d, want 2", len(q.Where))
	}
	var logical *Logical
	for _, f := range q.Where {
		if l, ok := f.(*Logical); ok {
			logical = l
		}
	}
	if logical == nil || logical.Op != LogicalOr || len(logical.Children) != 2 {
		t.Fatalf("logical = %+v", logical)
	}
}

func TestParseNestedLogical(t *testing.T) {
	q := mustParse(t, "or=(age.eq.14,not.and(age.gte.11,age.lte.17))")
	logical := q.Where[0].(*Logical)
	if len(logical.Children) != 2 {
		t.Fatalf("children = %d", len(logical.Children))
	}
	nested, ok := logical.Children[1].(*Logical)
	if !ok {
		t.Fatalf("child = %T, want *Logical", logical.Children[1])
	}
	if nested.Op != LogicalAnd || !nested.Negated || len(nested.Children) != 2 {
		t.Errorf("nested = %+v", nested)
	}
}

func TestParseNotAtTopLevel(t *testing.T) {
	q := mustParse(t, "not.or=(a.eq.1,b.eq.2)")
	logical, ok := q.Where[0].(*Logical)
	if !ok || !logical.Negated || logical.Op != LogicalOr {
		t.Fatalf("Where[0] = %+v", q.Where[0])
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for _, raw := range []string{
		"age=gte",
		"age=bogus.1",
		"a=is.maybe",
		"a=like(bogus).x",
		"order=age.sideways",
		"a=in.1,2",
	} {
		values, err := url.ParseQuery(raw)
		if err != nil {
			continue
		}
		if _, err := Parse(values); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", raw)
		}
	}
}

func TestParseAggregates(t *testing.T) {
	q := mustParse(t, "select=status,count(),sum(quantity),total:max(price)")
	if len(q.Select) != 4 {
		t.Fatalf("len(Select) = %d, want 4", len(q.Select))
	}
	if q.Select[0].Aggregate != nil {
		t.Errorf("Select[0] should be a plain column: %+v", q.Select[0])
	}
	count := q.Select[1].Aggregate
	if count == nil || count.Fn != "count" || count.Column != "" {
		t.Errorf("Select[1] = %+v, want count(*)", q.Select[1])
	}
	if sum := q.Select[2].Aggregate; sum == nil || sum.Fn != "sum" || sum.Column != "quantity" {
		t.Errorf("Select[2] = %+v", q.Select[2])
	}
	if max := q.Select[3].Aggregate; max == nil || max.Alias != "total" || max.Column != "price" {
		t.Errorf("Select[3] = %+v", q.Select[3])
	}
	if !q.HasAggregate() {
		t.Error("HasAggregate = false")
	}
	if !mustParse(t, "select=id").HasAggregate() == false {
		t.Error("HasAggregate should be false without aggregates")
	}
	// An embed named like a non-aggregate is still an embed.
	if embed := mustParse(t, "select=directors(last_name)").Select[0].Embed; embed == nil {
		t.Error("directors(last_name) should parse as an embed")
	}
	// Aggregates requiring a column are rejected when empty.
	values, _ := url.ParseQuery("select=sum()")
	if _, err := Parse(values); err == nil {
		t.Error("sum() should be rejected")
	}
}

func TestParseEmbedTree(t *testing.T) {
	q := mustParse(t, "select=title,director:directors!inner(last_name),roles(character,actor:actors(first_name))"+
		"&director.order=last_name&roles.limit=2&roles.character=eq.x")

	if len(q.Select) != 3 {
		t.Fatalf("len(Select) = %d, want 3", len(q.Select))
	}
	director := q.Select[1].Embed
	if director == nil || director.Name != "directors" || director.Alias != "director" || !director.Inner {
		t.Fatalf("director embed = %+v", director)
	}
	if len(director.Select) != 1 || director.Select[0].Column != "last_name" {
		t.Errorf("director select = %+v", director.Select)
	}
	if len(director.Order) != 1 || director.Order[0].Column != "last_name" {
		t.Errorf("director order = %+v", director.Order)
	}

	roles := q.Select[2].Embed
	if roles == nil || roles.Name != "roles" {
		t.Fatalf("roles embed = %+v", roles)
	}
	if roles.Limit == nil || *roles.Limit != 2 {
		t.Errorf("roles limit = %v", roles.Limit)
	}
	if len(roles.Where) != 1 {
		t.Errorf("roles where = %+v", roles.Where)
	}
	if len(roles.Select) != 2 || roles.Select[1].Embed == nil {
		t.Fatalf("roles select = %+v", roles.Select)
	}
	nested := roles.Select[1].Embed
	if nested.Name != "actors" || nested.Alias != "actor" {
		t.Errorf("nested embed = %+v", nested)
	}
}

func TestParseEmbedNullFilters(t *testing.T) {
	q := mustParse(t, "select=title,specs(*)&specs=is.null")
	embed := q.Select[1].Embed
	if embed.Null != NullAnti {
		t.Errorf("is.null should be an anti-join, got %v", embed.Null)
	}
	q = mustParse(t, "select=title,specs(*)&specs=not.is.null")
	if embed := q.Select[1].Embed; embed.Null != NullInner {
		t.Errorf("not.is.null should be an inner join, got %v", embed.Null)
	}
}

func TestParseEmbedRejectsOtherOperators(t *testing.T) {
	values, err := url.ParseQuery("select=title,specs(*)&specs=eq.5")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	_, err = Parse(values)
	var parseErr *Error
	if !errors.As(err, &parseErr) || parseErr.Code != "PGRST120" {
		t.Fatalf("error = %v, want PGRST120", err)
	}
}

func TestParseEmbedUnknownParamTarget(t *testing.T) {
	values, err := url.ParseQuery("select=title&nope.order=id")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	_, err = Parse(values)
	var parseErr *Error
	if !errors.As(err, &parseErr) || parseErr.Code != "PGRST108" {
		t.Fatalf("error = %v, want PGRST108", err)
	}
}

func TestParseIgnoresReservedParamsAsFilters(t *testing.T) {
	q := mustParse(t, "select=id&limit=1&offset=2&order=id")
	if len(q.Where) != 0 {
		t.Errorf("Where = %+v, want no filters", q.Where)
	}
}

func TestParseWriteMode(t *testing.T) {
	values, err := url.ParseQuery("columns=title,body&on_conflict=sku&select=id&title=eq.x&limit=1")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}

	// In write mode, columns and on_conflict are parameters, not filters.
	q, err := ParseWith(values, ParseOptions{Write: true})
	if err != nil {
		t.Fatalf("ParseWith: %v", err)
	}
	if want := []string{"title", "body"}; !equalStrings(q.Columns, want) {
		t.Errorf("Columns = %v, want %v", q.Columns, want)
	}
	if want := []string{"sku"}; !equalStrings(q.OnConflict, want) {
		t.Errorf("OnConflict = %v, want %v", q.OnConflict, want)
	}
	if len(q.Where) != 1 {
		t.Errorf("Where = %+v, want only the title filter", q.Where)
	}

	// In read mode those names are not parameters, so their values are parsed
	// as filters and rejected as malformed rather than silently ignored.
	if _, err := Parse(values); err == nil {
		t.Error("read mode should reject the write-only columns and on_conflict parameters")
	}

	// Read-only query strings still parse in write mode.
	readOnly, err := ParseWith(url.Values{"select": {"id"}, "limit": {"1"}}, ParseOptions{Write: true})
	if err != nil {
		t.Fatalf("ParseWith: %v", err)
	}
	if readOnly.Limit == nil || *readOnly.Limit != 1 || len(readOnly.Columns) != 0 {
		t.Errorf("ParseWith = %+v", readOnly)
	}
}

func TestSplitIdentListIgnoresBlanks(t *testing.T) {
	if got := splitIdentList("a, b ,,c"); !equalStrings(got, []string{"a", "b", "c"}) {
		t.Errorf("splitIdentList = %v", got)
	}
	if got := splitIdentList("  "); len(got) != 0 {
		t.Errorf("splitIdentList(blank) = %v", got)
	}
}

func TestErrorString(t *testing.T) {
	err := errf("select", "bad %s", "thing")
	if err.Code != "PGRST100" {
		t.Errorf("Code = %q", err.Code)
	}
	if !strings.Contains(err.Error(), "PGRST100") || !strings.Contains(err.Error(), "bad thing") {
		t.Errorf("Error() = %q", err.Error())
	}
	var asError error = err
	if _, ok := errors.AsType[*Error](asError); !ok {
		t.Error("errors.AsType should find *Error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
