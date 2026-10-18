package sqlgen

import (
	"fmt"
	"strings"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

// ArgValue is one bound function argument. Null renders an untyped NULL.
type ArgValue struct {
	Name string
	Text string
	Null bool
}

// RPC generates a function call. Table-valued functions are projected through
// the read pipeline so filtering, ordering, pagination, and embeddings behave
// exactly as they do for relations.
func RPC(snap *schema.Snapshot, fn *schema.Function, q *query.Query, args []ArgValue, opts ReadOptions) (*Statement, error) {
	g := &generator{b: &Builder{}, snap: snap, opts: opts}
	call, err := g.callExpression(fn, args)
	if err != nil {
		return nil, err
	}

	switch fn.Kind {
	case schema.ReturnTable:
		if fn.Returns == nil {
			return nil, fmt.Errorf("function %s has no result shape", fn.ID.Name)
		}
		opts.From = call
		g.opts = opts
		return g.generate(fn.Returns, q)

	case schema.ReturnVoid:
		return &Statement{SQL: "select " + call + "::text as body", Args: g.b.Args()}, nil

	case schema.ReturnRecord:
		if fn.ReturnsSet {
			return &Statement{
				SQL:  "select coalesce(json_agg(to_jsonb(pgrst_r)), '[]'::json)::text as body from " + call + " as pgrst_r",
				Args: g.b.Args(),
			}, nil
		}
		return &Statement{SQL: "select to_jsonb(" + call + ")::text as body", Args: g.b.Args()}, nil

	default:
		return &Statement{SQL: "select to_jsonb(" + call + ")::text as body", Args: g.b.Args()}, nil
	}
}

// callExpression renders `"schema"."name"("arg" => value, ...)`. Argument names
// come from the catalog and values are always bind parameters.
func (g *generator) callExpression(fn *schema.Function, args []ArgValue) (string, error) {
	qualified := Qualified(fn.ID.Schema, fn.ID.Name)
	inputs := fn.InputArgs()

	byName := make(map[string]ArgValue, len(args))
	for _, arg := range args {
		byName[arg.Name] = arg
	}

	parts := make([]string, 0, len(inputs))
	for _, input := range inputs {
		value, ok := byName[input.Name]
		if !ok {
			// Omitted arguments use their declared default.
			continue
		}
		rendered, err := g.bindArg(input, value)
		if err != nil {
			return "", err
		}
		// A VARIADIC parameter accepts an array only with the keyword, and it
		// is always the last parameter.
		if input.Mode == schema.ArgVariadic {
			parts = append(parts, "variadic "+rendered)
			continue
		}
		if input.Name == "" {
			// A single unnamed argument is passed positionally.
			parts = append(parts, rendered)
			continue
		}
		parts = append(parts, Ident(input.Name)+" => "+rendered)
	}
	return qualified + "(" + strings.Join(parts, ", ") + ")", nil
}

func (g *generator) bindArg(input schema.FunctionArg, value ArgValue) (string, error) {
	if value.Null {
		return "null::" + input.CastType, nil
	}
	placeholder := g.b.Param(value.Text)
	return "(" + placeholder + "::text)::" + input.CastType, nil
}
