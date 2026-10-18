// Package openapi renders a Swagger 2.0 document describing the exposed
// relations and functions.
//
// The document is generated from the schema cache, so it stays in step with
// the database. It aims to describe the surface accurately rather than to be
// byte-identical to PostgREST's own document.
package openapi

import (
	"strings"

	"github.com/andeshq/subquery/internal/schema"
)

// Info is the Swagger info object.
type Info struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version"`
}

// Schema is a JSON Schema subset Swagger 2.0 accepts.
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Ref         string             `json:"$ref,omitempty"`
	Required    []string           `json:"required,omitempty"`
}

// Parameter is a Swagger 2.0 parameter.
type Parameter struct {
	Ref         string  `json:"$ref,omitempty"`
	Name        string  `json:"name,omitempty"`
	In          string  `json:"in,omitempty"`
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Type        string  `json:"type,omitempty"`
	Format      string  `json:"format,omitempty"`
	Items       *Schema `json:"items,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Response is a Swagger 2.0 response.
type Response struct {
	Description string  `json:"description"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Operation is a Swagger 2.0 operation.
type Operation struct {
	Tags       []string             `json:"tags,omitempty"`
	Summary    string               `json:"summary,omitempty"`
	Parameters []*Parameter         `json:"parameters,omitempty"`
	Responses  map[string]*Response `json:"responses"`
}

// PathItem groups the operations on one path.
type PathItem struct {
	Get    *Operation `json:"get,omitempty"`
	Post   *Operation `json:"post,omitempty"`
	Put    *Operation `json:"put,omitempty"`
	Patch  *Operation `json:"patch,omitempty"`
	Delete *Operation `json:"delete,omitempty"`
}

// Document is a Swagger 2.0 document.
type Document struct {
	Swagger     string                `json:"swagger"`
	Info        Info                  `json:"info"`
	Host        string                `json:"host,omitempty"`
	BasePath    string                `json:"basePath"`
	Schemes     []string              `json:"schemes,omitempty"`
	Consumes    []string              `json:"consumes,omitempty"`
	Produces    []string              `json:"produces,omitempty"`
	Paths       map[string]*PathItem  `json:"paths"`
	Definitions map[string]*Schema    `json:"definitions,omitempty"`
	Parameters  map[string]*Parameter `json:"parameters,omitempty"`
}

// Options configures generation.
type Options struct {
	Version     string
	Host        string
	Schemes     []string
	ProxyURI    string
	IncludeRel  func(*schema.Relation) bool
	IncludeFunc func(*schema.Function) bool
}

// The shared query and header parameters every relation path accepts.
func sharedParameters() map[string]*Parameter {
	return map[string]*Parameter{
		"select":      {Name: "select", In: "query", Type: "string", Description: "Vertical filtering: columns, aliases, casts, JSON paths, and embedded resources."},
		"order":       {Name: "order", In: "query", Type: "string", Description: "Order by columns, e.g. age.desc.nullslast."},
		"limit":       {Name: "limit", In: "query", Type: "integer", Format: "int64", Description: "Maximum number of rows."},
		"offset":      {Name: "offset", In: "query", Type: "integer", Format: "int64", Description: "Rows to skip."},
		"and":         {Name: "and", In: "query", Type: "string", Description: "Conjunction of grouped filters, e.g. and=(a.gt.1,b.lt.2)."},
		"or":          {Name: "or", In: "query", Type: "string", Description: "Disjunction of grouped filters, e.g. or=(a.eq.1,b.eq.2)."},
		"not.and":     {Name: "not.and", In: "query", Type: "string", Description: "Negated conjunction of grouped filters."},
		"not.or":      {Name: "not.or", In: "query", Type: "string", Description: "Negated disjunction of grouped filters."},
		"columns":     {Name: "columns", In: "query", Type: "string", Description: "Columns to insert from the payload."},
		"on_conflict": {Name: "on_conflict", In: "query", Type: "string", Description: "Conflict target for upserts."},
		"Prefer":      {Name: "Prefer", In: "header", Type: "string", Description: "Preferences such as return=representation, count=exact, or resolution=merge-duplicates."},
		"Range":       {Name: "Range", In: "header", Type: "string", Description: "Requested row range, e.g. 0-24."},
		"Range-Unit":  {Name: "Range-Unit", In: "header", Type: "string", Description: "Always items."},
	}
}

func ref(name string) string { return "#/parameters/" + name }

// Build renders the document for the snapshot.
func Build(snap *schema.Snapshot, opts Options) *Document {
	doc := &Document{
		Swagger: "2.0",
		Info: Info{
			Title:       "Subquery API",
			Description: "Dynamic API generated from the PostgreSQL schema.",
			Version:     opts.Version,
		},
		BasePath:    "/",
		Schemes:     opts.Schemes,
		Consumes:    []string{"application/json"},
		Produces:    []string{"application/json"},
		Paths:       map[string]*PathItem{},
		Definitions: map[string]*Schema{},
		Parameters:  sharedParameters(),
	}
	doc.Host = opts.Host
	if opts.ProxyURI != "" {
		applyProxyURI(doc, opts.ProxyURI)
	}

	includeRel := opts.IncludeRel
	if includeRel == nil {
		includeRel = func(*schema.Relation) bool { return true }
	}
	includeFunc := opts.IncludeFunc
	if includeFunc == nil {
		includeFunc = func(*schema.Function) bool { return true }
	}

	for _, id := range snap.Order {
		rel := snap.Relations[id]
		if !rel.Exposed || !includeRel(rel) {
			continue
		}
		doc.Definitions[rel.ID.Name] = relationSchema(rel)
		doc.Paths["/"+rel.ID.Name] = relationPath(rel)
	}

	// Functions are keyed by name, so merge overloads under one path.
	seen := map[string]bool{}
	for _, id := range snap.FunctionOrder() {
		overloads := snap.Functions[id]
		if len(overloads) == 0 || seen[id.Name] {
			continue
		}
		var included []*schema.Function
		for _, fn := range overloads {
			if includeFunc(fn) {
				included = append(included, fn)
			}
		}
		if len(included) == 0 {
			continue
		}
		seen[id.Name] = true
		doc.Paths["/rpc/"+id.Name] = functionPath(included)
	}
	return doc
}

func relationSchema(rel *schema.Relation) *Schema {
	properties := make(map[string]*Schema, len(rel.Columns))
	var required []string
	for _, col := range rel.Columns {
		typ, format, items := swaggerType(col.TypeName)
		properties[col.Name] = &Schema{Type: typ, Format: format, Items: items}
		if col.NotNull && !col.HasDefault && !col.Generated {
			required = append(required, col.Name)
		}
	}
	for name, fn := range rel.ComputedFields() {
		typ, format, items := swaggerType(fn.ResultTypeName)
		properties[name] = &Schema{Type: typ, Format: format, Items: items, Description: "Computed field."}
	}
	return &Schema{Type: "object", Properties: properties, Required: required}
}

func relationPath(rel *schema.Relation) *PathItem {
	item := &PathItem{}
	item.Get = &Operation{
		Tags:       []string{rel.ID.Name},
		Summary:    "Read rows from " + rel.ID.Name,
		Parameters: relationParameters(rel),
		Responses: map[string]*Response{
			"200": {Description: "Rows", Schema: &Schema{Type: "array", Items: &Schema{Ref: "#/definitions/" + rel.ID.Name}}},
		},
	}
	item.Post = &Operation{
		Tags:    []string{rel.ID.Name},
		Summary: "Insert rows into " + rel.ID.Name,
		Parameters: append(relationParameters(rel), &Parameter{
			Name: "payload", In: "body", Required: true,
			Schema: &Schema{Type: "array", Items: &Schema{Ref: "#/definitions/" + rel.ID.Name}},
		}),
		Responses: map[string]*Response{
			"201": {Description: "Created"},
		},
	}
	item.Patch = &Operation{
		Tags:    []string{rel.ID.Name},
		Summary: "Update rows in " + rel.ID.Name,
		Parameters: append(relationParameters(rel), &Parameter{
			Name: "payload", In: "body", Required: true, Schema: &Schema{Ref: "#/definitions/" + rel.ID.Name},
		}),
		Responses: map[string]*Response{"204": {Description: "Updated"}},
	}
	item.Delete = &Operation{
		Tags:       []string{rel.ID.Name},
		Summary:    "Delete rows from " + rel.ID.Name,
		Parameters: relationParameters(rel),
		Responses:  map[string]*Response{"204": {Description: "Deleted"}},
	}
	if len(rel.PrimaryKey) > 0 {
		item.Put = &Operation{
			Tags:    []string{rel.ID.Name},
			Summary: "Upsert one row in " + rel.ID.Name,
			Parameters: append(relationParameters(rel), &Parameter{
				Name: "payload", In: "body", Required: true, Schema: &Schema{Ref: "#/definitions/" + rel.ID.Name},
			}),
			Responses: map[string]*Response{
				"200": {Description: "Replaced"},
				"201": {Description: "Created"},
			},
		}
	}
	return item
}

func relationParameters(rel *schema.Relation) []*Parameter {
	params := []*Parameter{
		{Ref: ref("select")}, {Ref: ref("order")}, {Ref: ref("limit")}, {Ref: ref("offset")},
		{Ref: ref("and")}, {Ref: ref("or")}, {Ref: ref("not.and")}, {Ref: ref("not.or")},
		{Ref: ref("columns")}, {Ref: ref("on_conflict")},
		{Ref: ref("Prefer")}, {Ref: ref("Range")}, {Ref: ref("Range-Unit")},
	}
	for _, col := range rel.Columns {
		typ, format, items := swaggerType(col.TypeName)
		params = append(params, &Parameter{
			Name:        col.Name,
			In:          "query",
			Description: "Filter by " + col.Name + ", e.g. eq.value or in.(a,b).",
			Type:        typ,
			Format:      format,
			Items:       items,
		})
	}
	for name := range rel.ComputedFields() {
		params = append(params, &Parameter{
			Name:        name,
			In:          "query",
			Description: "Filter by the computed field " + name + ".",
			Type:        "string",
		})
	}
	return params
}

func functionPath(overloads []*schema.Function) *PathItem {
	item := &PathItem{}
	// The first overload shapes the GET document; POST accepts any of them.
	var stable *schema.Function
	for _, fn := range overloads {
		if !fn.Mutable() {
			stable = fn
			break
		}
	}
	if stable != nil {
		item.Get = &Operation{
			Tags:       []string{overloads[0].ID.Name},
			Summary:    "Call " + overloads[0].ID.Name,
			Parameters: functionParameters(stable, true),
			Responses:  map[string]*Response{"200": functionResponse(stable)},
		}
	}
	var postParams []*Parameter
	for _, fn := range overloads {
		postParams = functionParameters(fn, false)
	}
	item.Post = &Operation{
		Tags:       []string{overloads[0].ID.Name},
		Summary:    "Call " + overloads[0].ID.Name,
		Parameters: postParams,
		Responses:  map[string]*Response{"200": functionResponse(overloads[0])},
	}
	return item
}

func functionParameters(fn *schema.Function, query bool) []*Parameter {
	var params []*Parameter
	for _, arg := range fn.InputArgs() {
		if arg.Mode == schema.ArgTable || arg.Mode == schema.ArgOut {
			continue
		}
		typ, format, items := swaggerTypeFromCast(arg.CastType)
		param := &Parameter{
			Name:     arg.Name,
			Required: !arg.HasDefault,
			Type:     typ,
			Format:   format,
			Items:    items,
		}
		if query {
			param.In = "query"
		} else {
			param.In = "body"
		}
		if arg.Name == "" {
			// A single unnamed argument takes the whole payload.
			param.Name = "payload"
			param.In = "body"
			param.Type = ""
			param.Format = ""
			param.Items = nil
			param.Schema = &Schema{Type: "object"}
		}
		params = append(params, param)
	}
	if fn.Kind == schema.ReturnTable {
		params = append(params, &Parameter{Ref: ref("select")}, &Parameter{Ref: ref("order")},
			&Parameter{Ref: ref("limit")}, &Parameter{Ref: ref("offset")},
			&Parameter{Ref: ref("and")}, &Parameter{Ref: ref("or")},
			&Parameter{Ref: ref("not.and")}, &Parameter{Ref: ref("not.or")},
			&Parameter{Ref: ref("Prefer")}, &Parameter{Ref: ref("Range")}, &Parameter{Ref: ref("Range-Unit")})
	}
	return params
}

func functionResponse(fn *schema.Function) *Response {
	switch fn.Kind {
	case schema.ReturnVoid:
		return &Response{Description: "No content"}
	case schema.ReturnTable:
		if fn.Returns != nil && fn.Returns.Exposed && len(fn.Returns.PrimaryKey) > 0 {
			return &Response{Description: "Rows", Schema: &Schema{Type: "array", Items: &Schema{Ref: "#/definitions/" + fn.Returns.ID.Name}}}
		}
		return &Response{Description: "Rows", Schema: &Schema{Type: "array", Items: &Schema{Type: "object"}}}
	case schema.ReturnRecord:
		return &Response{Description: "Rows", Schema: &Schema{Type: "array", Items: &Schema{Type: "object"}}}
	default:
		typ, format, items := swaggerType(fn.ResultTypeName)
		return &Response{Description: "Result", Schema: &Schema{Type: typ, Format: format, Items: items}}
	}
}

func applyProxyURI(doc *Document, uri string) {
	rest := uri
	if body, ok := strings.CutPrefix(uri, "https://"); ok {
		doc.Schemes = []string{"https"}
		rest = body
	} else if body, ok := strings.CutPrefix(uri, "http://"); ok {
		doc.Schemes = []string{"http"}
		rest = body
	}
	host, path, found := strings.Cut(rest, "/")
	if host != "" {
		doc.Host = host
	}
	if found {
		doc.BasePath = "/" + strings.Trim(path, "/")
	}
}

// swaggerType maps a PostgreSQL type name to a Swagger type, format, and
// element schema for arrays.
func swaggerType(typeName string) (string, string, *Schema) {
	name := strings.TrimSpace(typeName)
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	if element, ok := strings.CutSuffix(name, "[]"); ok {
		elementType, elementFormat, elementItems := swaggerType(element)
		return "array", "", &Schema{Type: elementType, Format: elementFormat, Items: elementItems}
	}
	switch strings.ToLower(name) {
	case "smallint", "int2", "integer", "int4", "serial", "serial4":
		return "integer", "int32", nil
	case "bigint", "int8", "bigserial", "serial8":
		return "integer", "int64", nil
	case "numeric", "decimal", "money":
		return "number", "", nil
	case "real", "float4":
		return "number", "float", nil
	case "double precision", "float8":
		return "number", "double", nil
	case "boolean", "bool":
		return "boolean", "", nil
	case "date":
		return "string", "date", nil
	case "timestamp without time zone", "timestamp with time zone", "timestamp", "timestamptz":
		return "string", "date-time", nil
	case "time without time zone", "time with time zone", "time", "timetz", "interval":
		return "string", "", nil
	case "uuid":
		return "string", "uuid", nil
	case "json", "jsonb":
		return "object", "", nil
	case "bytea":
		return "string", "byte", nil
	default:
		return "string", "", nil
	}
}

// swaggerTypeFromCast maps a quoted cast type such as
// "pg_catalog"."int4"[] back to a Swagger type.
func swaggerTypeFromCast(cast string) (string, string, *Schema) {
	name := strings.ReplaceAll(cast, `"`, "")
	if idx := strings.LastIndexByte(name, '.'); idx >= 0 {
		name = name[idx+1:]
	}
	return swaggerType(name)
}
