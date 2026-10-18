package schema

import (
	"fmt"
	"sort"
	"strings"

	"github.com/andeshq/subquery/internal/pgrst"
)

// ArgMode mirrors pg_proc.proargmodes.
type ArgMode byte

const (
	ArgIn       ArgMode = 'i'
	ArgOut      ArgMode = 'o'
	ArgInOut    ArgMode = 'b'
	ArgVariadic ArgMode = 'v'
	ArgTable    ArgMode = 't'
)

// Input reports whether the argument is supplied by the caller.
func (m ArgMode) Input() bool {
	return m == ArgIn || m == ArgInOut || m == ArgVariadic
}

// Output reports whether the argument is part of the result row.
func (m ArgMode) Output() bool {
	return m == ArgOut || m == ArgInOut || m == ArgTable
}

// FunctionArg is one parameter or result column of a function.
type FunctionArg struct {
	Name     string
	Mode     ArgMode
	Position int
	TypeOID  uint32
	// CastType is a schema-qualified, quoted type name safe to splice into a
	// cast.
	CastType   string
	HasDefault bool
	// Relation is set when the argument's type is a relation's row type.
	Relation RelID
}

// ReturnKind classifies a function's result.
type ReturnKind int

const (
	ReturnScalar ReturnKind = iota
	ReturnTable
	ReturnRecord
	ReturnVoid
)

// Function is one overloaded function in an exposed schema.
type Function struct {
	ID RelID
	// Args includes IN, OUT, INOUT, VARIADIC, and TABLE parameters.
	Args []FunctionArg
	// Volatility is pg_proc.provolatile: 'i', 's', or 'v'.
	Volatility byte
	ReturnsSet bool
	Kind       ReturnKind
	// Returns is the row shape of a table-valued function.
	Returns *Relation
	// ScalarCast is the cast type of a scalar result.
	ScalarCast string
	// ResultTypeName is the declared type name of a scalar result.
	ResultTypeName string
}

// Mutable reports whether calling the function may modify the database.
func (f *Function) Mutable() bool { return f.Volatility == 'v' }

// InputArgs returns the caller-supplied arguments in position order.
func (f *Function) InputArgs() []FunctionArg {
	out := make([]FunctionArg, 0, len(f.Args))
	for _, arg := range f.Args {
		if arg.Mode.Input() {
			out = append(out, arg)
		}
	}
	return out
}

// Arg returns the named input argument.
func (f *Function) Arg(name string) (FunctionArg, bool) {
	for _, arg := range f.Args {
		if arg.Mode.Input() && arg.Name == name {
			return arg, true
		}
	}
	return FunctionArg{}, false
}

// Signature renders a human-readable signature for error hints.
func (f *Function) Signature() string {
	parts := make([]string, 0, len(f.Args))
	for _, arg := range f.Args {
		if !arg.Mode.Input() {
			continue
		}
		name := arg.Name
		if name == "" {
			name = "?"
		}
		parts = append(parts, fmt.Sprintf("%s => %s", name, arg.CastType))
	}
	return fmt.Sprintf("%s.%s(%s)", f.ID.Schema, f.ID.Name, strings.Join(parts, ", "))
}

// unnamedSingleArg reports whether the function takes exactly one unnamed input
// argument, which PostgREST calls with the raw request body.
func (f *Function) unnamedSingleArg() (FunctionArg, bool) {
	inputs := f.InputArgs()
	if len(inputs) != 1 || inputs[0].Name != "" {
		return FunctionArg{}, false
	}
	return inputs[0], true
}

// ResolveFunction picks the overload matching the supplied argument names.
func (s *Snapshot) ResolveFunction(name string, provided []string) (*Function, *SchemaError) {
	var candidates []*Function
	for _, schema := range s.Schemas {
		if overloads, ok := s.Functions[RelID{Schema: schema, Name: name}]; ok {
			candidates = append(candidates, overloads...)
		}
	}
	if len(candidates) == 0 {
		return nil, &SchemaError{
			Code:    pgrst.FunctionNotFound,
			Message: fmt.Sprintf("Could not find the function %s in the schema cache", name),
		}
	}

	providedSet := make(map[string]bool, len(provided))
	for _, p := range provided {
		providedSet[p] = true
	}

	var matched []*Function
candidateLoop:
	for _, candidate := range candidates {
		inputs := candidate.InputArgs()
		// A single unnamed argument is called with the whole body.
		if _, ok := candidate.unnamedSingleArg(); ok {
			matched = append(matched, candidate)
			continue
		}
		satisfied := 0
		for _, arg := range inputs {
			switch {
			case providedSet[arg.Name] && arg.Name != "":
				satisfied++
			case arg.HasDefault:
				// May be omitted.
			default:
				continue candidateLoop
			}
		}
		for _, name := range provided {
			if _, ok := candidate.Arg(name); !ok {
				continue candidateLoop
			}
		}
		if satisfied == len(provided) {
			matched = append(matched, candidate)
		}
	}

	switch len(matched) {
	case 0:
		return nil, &SchemaError{
			Code:    pgrst.FunctionNotFound,
			Message: fmt.Sprintf("Could not find the function %s with the given arguments in the schema cache", name),
			Hint:    s.functionHints(name),
		}
	case 1:
		return matched[0], nil
	default:
		hints := make([]string, 0, len(matched))
		for _, fn := range matched {
			hints = append(hints, "'"+fn.Signature()+"'")
		}
		return nil, &SchemaError{
			Code:    pgrst.AmbiguousFunction,
			Message: "Could not choose the best candidate function between the given overloads",
			Hint:    "Try changing the argument names or types. Candidates: " + strings.Join(hints, ", "),
		}
	}
}

func (s *Snapshot) functionHints(name string) any {
	var hints []string
	for _, schema := range s.Schemas {
		for _, fn := range s.Functions[RelID{Schema: schema, Name: name}] {
			hints = append(hints, "'"+fn.Signature()+"'")
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return "Perhaps you meant one of: " + strings.Join(hints, ", ")
}

// buildFunctions materializes the function index from the catalog.
func buildFunctions(snap *Snapshot, cat []CatalogFunction) {
	snap.Functions = make(map[RelID][]*Function, len(cat))
	for _, cf := range cat {
		fn := &Function{
			ID:         RelID{Schema: cf.Schema, Name: cf.Name},
			Volatility: cf.Volatility,
			ReturnsSet: cf.ReturnsSet,
		}
		for _, arg := range cf.Args {
			fn.Args = append(fn.Args, FunctionArg{
				Name:       arg.Name,
				Mode:       ArgMode(arg.Mode),
				Position:   arg.Position,
				TypeOID:    arg.TypeOID,
				CastType:   arg.CastType,
				HasDefault: arg.HasDefault,
				Relation:   arg.Relation,
			})
		}
		// pronargdefaults counts the trailing input arguments that carry
		// defaults.
		remaining := cf.DefaultCount
		for j := len(fn.Args) - 1; j >= 0 && remaining > 0; j-- {
			if fn.Args[j].Mode.Input() {
				fn.Args[j].HasDefault = true
				remaining--
			}
		}
		fn.Kind, fn.Returns, fn.ScalarCast = classifyReturn(snap, cf, fn)
		fn.ResultTypeName = cf.ReturnTypeName
		snap.Functions[fn.ID] = append(snap.Functions[fn.ID], fn)

		// A function taking exactly one relation row type and returning a
		// scalar is a computed field of that relation.
		registerComputedField(snap, fn)
	}

	for id := range snap.Functions {
		snap.functionOrder = append(snap.functionOrder, id)
	}
	sort.Slice(snap.functionOrder, func(i, j int) bool {
		if snap.functionOrder[i].Schema != snap.functionOrder[j].Schema {
			return snap.functionOrder[i].Schema < snap.functionOrder[j].Schema
		}
		return snap.functionOrder[i].Name < snap.functionOrder[j].Name
	})
}

// registerComputedField records a scalar function of a single row argument as a
// virtual column of the argument's relation.
func registerComputedField(snap *Snapshot, fn *Function) {
	if fn.Kind != ReturnScalar || fn.ReturnsSet {
		return
	}
	inputs := fn.InputArgs()
	if len(inputs) != 1 || inputs[0].Mode != ArgIn {
		return
	}
	rel, ok := snap.Relations[inputs[0].Relation]
	if !ok {
		return
	}
	if rel.computed == nil {
		rel.computed = make(map[string]*Function)
	}
	rel.computed[fn.ID.Name] = fn
}

// classifyReturn decides how a function's result is shaped.
func classifyReturn(snap *Snapshot, cf CatalogFunction, fn *Function) (ReturnKind, *Relation, string) {
	if cf.ReturnsVoid {
		return ReturnVoid, nil, ""
	}
	// A composite return type that is a known relation is table-valued.
	if cf.ReturnRelation != (RelID{}) {
		if rel, ok := snap.Relations[cf.ReturnRelation]; ok {
			return ReturnTable, rel, ""
		}
	}
	// RETURNS TABLE(...) and OUT parameters define the row shape.
	var outCols []CatalogColumn
	for _, arg := range cf.Args {
		if ArgMode(arg.Mode).Output() {
			name := arg.Name
			if name == "" {
				name = fmt.Sprintf("column%d", arg.Position)
			}
			outCols = append(outCols, CatalogColumn{
				Name:     name,
				Position: arg.Position,
				TypeOID:  arg.TypeOID,
				CastType: arg.CastType,
				TypeName: strings.Trim(arg.CastType, `"`),
			})
		}
	}
	if len(outCols) > 0 {
		return ReturnTable, newResultRelation(fn.ID, outCols), ""
	}
	if cf.ReturnTypeName == "record" {
		return ReturnRecord, nil, ""
	}
	return ReturnScalar, nil, cf.ReturnCastType
}

// newResultRelation synthesizes a relation for RETURNS TABLE(...) output.
func newResultRelation(id RelID, cols []CatalogColumn) *Relation {
	rel := &Relation{ID: id, Kind: KindTable, Exposed: true, columnIndex: make(map[string]int, len(cols))}
	for _, col := range cols {
		rel.columnIndex[col.Name] = len(rel.Columns)
		rel.Columns = append(rel.Columns, Column{
			Name:     col.Name,
			Position: col.Position,
			TypeOID:  col.TypeOID,
			TypeName: col.TypeName,
			CastType: col.CastType,
		})
	}
	return rel
}
