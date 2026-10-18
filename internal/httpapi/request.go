package httpapi

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// maxWriteBody bounds how much of a request body is read.
const maxWriteBody = 32 << 20

// preferences is the parsed Prefer header.
type preferences struct {
	Return         sqlgen.ReturnMode
	Resolution     sqlgen.Resolution
	MissingDefault bool
	MaxAffected    *int64
	Strict         bool
	TxRollback     bool
	applied        []string
}

// eachPreference calls fn for every non-empty, trimmed entry of a Prefer header.
// It is the single place the header's `a, b=c` shape is scanned; callers decide
// how to interpret each entry.
func eachPreference(header string, fn func(entry string)) {
	for pref := range strings.SplitSeq(header, ",") {
		if pref = strings.TrimSpace(pref); pref != "" {
			fn(pref)
		}
	}
}

func parsePreferences(header string, txEnd config.TxEnd) (preferences, *APIError) {
	prefs := preferences{Return: sqlgen.ReturnMinimal}
	var invalid []string

	eachPreference(header, func(entry string) {
		name, value, _ := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)

		switch name {
		case "return":
			switch value {
			case "minimal":
				prefs.Return = sqlgen.ReturnMinimal
			case "headers-only":
				prefs.Return = sqlgen.ReturnHeadersOnly
			case "representation":
				prefs.Return = sqlgen.ReturnRepresentation
			default:
				invalid = append(invalid, entry)
				return
			}
			prefs.applied = append(prefs.applied, "return="+value)
		case "resolution":
			switch value {
			case "merge-duplicates":
				prefs.Resolution = sqlgen.ResolutionMerge
			case "ignore-duplicates":
				prefs.Resolution = sqlgen.ResolutionIgnore
			default:
				invalid = append(invalid, entry)
				return
			}
			prefs.applied = append(prefs.applied, "resolution="+value)
		case "missing":
			if value != "default" {
				invalid = append(invalid, entry)
				return
			}
			prefs.MissingDefault = true
			prefs.applied = append(prefs.applied, "missing=default")
		case "max-affected":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				invalid = append(invalid, entry)
				return
			}
			prefs.MaxAffected = new(n)
			prefs.applied = append(prefs.applied, "max-affected="+value)
		case "handling":
			switch value {
			case "strict":
				prefs.Strict = true
			case "lenient":
			default:
				invalid = append(invalid, entry)
				return
			}
			prefs.applied = append(prefs.applied, "handling="+value)
		case "tx":
			honored := false
			switch {
			case value == "rollback" && (txEnd == config.TxCommitAllowOverride || txEnd == config.TxRollbackAllowOverride):
				prefs.TxRollback = true
				honored = true
			case value == "commit" && (txEnd == config.TxCommitAllowOverride || txEnd == config.TxRollbackAllowOverride):
				prefs.TxRollback = false
				honored = true
			case value != "commit" && value != "rollback":
				invalid = append(invalid, entry)
				return
			}
			// Honoring a transaction preference requires db-tx-end to allow it.
			if honored {
				prefs.applied = append(prefs.applied, "tx="+value)
			}
		case "count":
			// Meaningful for reads only.
		default:
			invalid = append(invalid, entry)
		}
	})

	if prefs.Strict && len(invalid) > 0 {
		return prefs, &APIError{
			Status:  http.StatusBadRequest,
			Code:    pgrst.InvalidPreferences,
			Message: "Invalid preferences given with handling=strict",
			Details: "Invalid preferences: " + strings.Join(invalid, ", "),
		}
	}
	return prefs, nil
}

// payload is a parsed request body.
type payload struct {
	raw     []byte
	keys    []string
	isArray bool
	object  map[string]any
}

func readWriteBody(r *http.Request) ([]byte, *APIError) {
	if media, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(media) != "" &&
		strings.TrimSpace(media) != "application/json" {
		return nil, &APIError{
			Status:  http.StatusUnsupportedMediaType,
			Code:    pgrst.UnsupportedMediaType,
			Message: fmt.Sprintf("Unsupported Content-Type %q; subquery currently accepts application/json", media),
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil {
		return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "Failed to read the request body"}
	}
	if len(raw) > maxWriteBody {
		return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Code: pgrst.InvalidBody, Message: "Request body is too large"}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "The request body is empty"}
	}
	return raw, nil
}

func parsePayload(raw []byte, allowArray bool) (*payload, *APIError) {
	trimmed := bytes.TrimSpace(raw)
	var doc any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "Failed to parse the request body as JSON"}
	}

	keys := map[string]bool{}
	parsed := &payload{raw: trimmed, isArray: bytes.HasPrefix(trimmed, []byte("["))}
	switch value := doc.(type) {
	case map[string]any:
		parsed.object = value
		for key := range value {
			keys[key] = true
		}
	case []any:
		if !allowArray {
			return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "Expected a JSON object, not an array"}
		}
		var first map[string]bool
		for _, item := range value {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "Each array element must be a JSON object"}
			}
			rowKeys := make(map[string]bool, len(object))
			for key := range object {
				keys[key] = true
				rowKeys[key] = true
			}
			if first == nil {
				first = rowKeys
			} else if !sameKeySet(first, rowKeys) {
				// PostgREST requires every element to carry the same keys; a key
				// missing from some rows would otherwise become NULL instead of
				// its column DEFAULT.
				return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "All object keys must match"}
			}
		}
	default:
		return nil, &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidBody, Message: "The request body must be a JSON object or array of objects"}
	}
	parsed.keys = slices.Sorted(maps.Keys(keys))
	return parsed, nil
}

// asArray renders the payload as a JSON array without re-serializing it.
func (p *payload) asArray() []byte {
	if p.isArray {
		return p.raw
	}
	out := make([]byte, 0, len(p.raw)+2)
	out = append(out, '[')
	out = append(out, p.raw...)
	out = append(out, ']')
	return out
}

func sameKeySet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func unknownColumn(rel *schema.Relation, name string) *APIError {
	return &APIError{
		Status:  http.StatusBadRequest,
		Code:    pgrst.ColumnNotFound,
		Message: fmt.Sprintf("Could not find the '%s' column of '%s' in the schema cache", name, rel.ID.Name),
	}
}

func validateKeys(rel *schema.Relation, keys []string) *APIError {
	for _, key := range keys {
		if _, ok := rel.Column(key); !ok {
			return unknownColumn(rel, key)
		}
	}
	return nil
}

// insertColumns resolves the insert list: explicit columns win, otherwise the
// payload's key union. Columns outside the list keep their DEFAULT, which is
// what makes `created_at default now()` behave as users expect.
func insertColumns(keys, explicit []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	return keys
}

func writeKindFor(method string) (sqlgen.WriteKind, *APIError) {
	switch method {
	case http.MethodPost:
		return sqlgen.WriteInsert, nil
	case http.MethodPatch:
		return sqlgen.WriteUpdate, nil
	case http.MethodPut:
		return sqlgen.WriteUpsert, nil
	case http.MethodDelete:
		return sqlgen.WriteDelete, nil
	}
	return 0, errMethodNotAllowed
}

// validatePut enforces PostgREST's PUT contract: the primary key must be
// filtered and the body must carry the same key values.
func validatePut(rel *schema.Relation, body *payload, filters []query.Filter) *APIError {
	if len(rel.PrimaryKey) == 0 {
		return &APIError{Status: http.StatusMethodNotAllowed, Code: pgrst.InvalidPut, Message: fmt.Sprintf("PUT requires a primary key, and %s has none", rel.ID.Name)}
	}
	invalid := &APIError{Status: http.StatusBadRequest, Code: pgrst.InvalidPut, Message: "Invalid PUT request"}
	for _, column := range rel.PrimaryKey {
		want, ok := filterEquality(filters, column)
		if !ok {
			return invalid.WithDetails(fmt.Sprintf("The query string must filter %s with eq", column))
		}
		got, present := scalarString(body.object, column)
		if !present {
			return invalid.WithDetails(fmt.Sprintf("The body must contain the %s column", column))
		}
		if got != want {
			return &APIError{
				Status:  http.StatusBadRequest,
				Code:    pgrst.PutKeyMismatch,
				Message: "The primary key in the query string and the body do not match",
				Details: fmt.Sprintf("%s: query=%s body=%s", column, want, got),
			}
		}
	}
	return nil
}

// filterEquality finds a top-level `column=eq.value` filter.
func filterEquality(filters []query.Filter, column string) (string, bool) {
	for _, filter := range filters {
		comparison, ok := filter.(*query.Comparison)
		if !ok || comparison.Column != column || comparison.Op != query.OpEq || comparison.Negated || len(comparison.Values) != 1 {
			continue
		}
		return comparison.Values[0], true
	}
	return "", false
}

func scalarString(object map[string]any, key string) (string, bool) {
	value, ok := object[key]
	if !ok {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	case nil:
		return "", true
	default:
		return fmt.Sprint(typed), true
	}
}
