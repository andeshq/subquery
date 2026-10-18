package httpapi

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"

	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// buildArgs converts request values into bound arguments.
func buildArgs(fn *schema.Function, r *http.Request, form map[string][]string, body map[string]jsontext.Value, rawBody []byte) ([]sqlgen.ArgValue, *APIError) {
	inputs := fn.InputArgs()

	// A single unnamed argument takes the whole payload.
	if arg, ok := unnamedArg(fn); ok {
		text := ""
		if r.Method == http.MethodPost {
			text = string(rawBody)
		} else if values := form[""]; len(values) > 0 {
			text = values[0]
		}
		return []sqlgen.ArgValue{{Name: arg.Name, Text: text, Null: text == ""}}, nil
	}

	out := make([]sqlgen.ArgValue, 0, len(inputs))
	for _, input := range inputs {
		if r.Method == http.MethodPost {
			value, ok := body[input.Name]
			if !ok {
				continue
			}
			converted, apiErr := convertJSONArg(input, []byte(value))
			if apiErr != nil {
				return nil, apiErr
			}
			out = append(out, converted)
			continue
		}
		values, ok := form[input.Name]
		if !ok {
			continue
		}
		if input.Mode == schema.ArgVariadic {
			out = append(out, sqlgen.ArgValue{Name: input.Name, Text: sqlgen.ArrayLiteral(form[input.Name])})
			continue
		}
		if len(values) > 0 {
			out = append(out, sqlgen.ArgValue{Name: input.Name, Text: values[0]})
		}
	}
	return out, nil
}

func unnamedArg(fn *schema.Function) (schema.FunctionArg, bool) {
	inputs := fn.InputArgs()
	if len(inputs) == 1 && inputs[0].Name == "" {
		return inputs[0], true
	}
	return schema.FunctionArg{}, false
}

// convertJSONArg turns a raw JSON value into a bound argument. Arrays become
// PostgreSQL array literals; json/jsonb arguments keep their exact bytes.
func convertJSONArg(arg schema.FunctionArg, raw []byte) (sqlgen.ArgValue, *APIError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return sqlgen.ArgValue{Name: arg.Name, Null: true}, nil
	}

	if strings.HasSuffix(arg.CastType, "[]") && trimmed[0] == '[' {
		var elements []jsontext.Value
		if err := json.Unmarshal(trimmed, &elements); err != nil {
			return sqlgen.ArgValue{}, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "Failed to parse array argument " + arg.Name}
		}
		texts := make([]string, 0, len(elements))
		for _, element := range elements {
			texts = append(texts, jsonScalarText(element))
		}
		return sqlgen.ArgValue{Name: arg.Name, Text: sqlgen.ArrayLiteral(texts)}, nil
	}

	switch arg.CastType {
	case `"pg_catalog"."json"`, `"pg_catalog"."jsonb"`:
		return sqlgen.ArgValue{Name: arg.Name, Text: string(trimmed)}, nil
	}
	return sqlgen.ArgValue{Name: arg.Name, Text: jsonScalarText(trimmed)}, nil
}

// jsonScalarText renders a JSON value as PostgreSQL text input.
func jsonScalarText(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var decoded string
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return decoded
		}
	}
	return string(trimmed)
}

// readRPCBody reads a POST payload. An empty body is allowed, since a function
// may take only defaulted arguments.
func readRPCBody(r *http.Request) ([]byte, *APIError) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil {
		return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "Failed to read the request body"}
	}
	if len(raw) > maxWriteBody {
		return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Code: pgrst.InvalidBody, Message: "Request body is too large"}
	}
	return raw, nil
}

// argumentNames selects the query parameters that name a function argument.
func argumentNames(values map[string][]string, argNames map[string]bool) map[string][]string {
	out := map[string][]string{}
	for key, vals := range values {
		if argNames[key] {
			out[key] = vals
		}
	}
	return out
}

// snapshotArgNames is the union of argument names across a function's overloads.
func snapshotArgNames(snap *schema.Snapshot, name string) map[string]bool {
	out := map[string]bool{}
	for _, fn := range snapshotFunctions(snap, name) {
		for _, arg := range fn.InputArgs() {
			if arg.Name != "" {
				out[arg.Name] = true
			}
		}
	}
	return out
}

func snapshotFunctions(snap *schema.Snapshot, name string) []*schema.Function {
	var out []*schema.Function
	for _, schemaName := range snap.Schemas {
		out = append(out, snap.Functions[schema.RelID{Schema: schemaName, Name: name}]...)
	}
	return out
}

func rpcAllowFor(fn *schema.Function) string {
	if fn.Mutable() {
		return "OPTIONS,POST"
	}
	return "OPTIONS,GET,HEAD,POST"
}
