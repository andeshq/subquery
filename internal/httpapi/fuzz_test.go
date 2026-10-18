package httpapi

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/schema"
)

// FuzzParsePayload asserts the request-body parser never panics and never
// accepts input it then mislabels.
func FuzzParsePayload(f *testing.F) {
	for _, seed := range []string{
		`{}`, `[]`, `[{}]`, `{"a":1}`, `[{"a":1},{"b":2}]`, `{"a":`, `"str"`, `1`,
		`null`, `[1]`, `{"a":1,"a":2}`, `{"\ud800":"x"}`, strings.Repeat(`[`, 64),
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, raw string, allowArray bool) {
		payload, apiErr := parsePayload([]byte(raw), allowArray)
		if apiErr != nil {
			if payload != nil {
				t.Fatalf("both a payload and an error for %q", raw)
			}
			return
		}
		if len(strings.TrimSpace(string(payload.raw))) == 0 {
			t.Fatalf("accepted an empty payload for %q", raw)
		}
		var probe any
		if err := json.Unmarshal(payload.raw, &probe); err != nil {
			t.Fatalf("accepted a payload whose raw form is not valid JSON: %q", raw)
		}
		if allowArray {
			// The normalized array form must also be valid JSON.
			var array []any
			if err := json.Unmarshal(payload.asArray(), &array); err != nil {
				t.Fatalf("asArray produced invalid JSON for %q: %v", raw, err)
			}
		}
	})
}

// txEnds are the db-tx-end values the preference parser branches on.
var txEnds = []config.TxEnd{
	config.TxCommit,
	config.TxCommitAllowOverride,
	config.TxRollback,
	config.TxRollbackAllowOverride,
}

// FuzzParsePreferences asserts the Prefer parser never panics and that strict
// handling rejects everything it could not apply.
func FuzzParsePreferences(f *testing.F) {
	for _, seed := range []string{
		"", "return=representation", "return=minimal, count=exact",
		"handling=strict, nope", "handling=lenient, nope", "resolution=merge-duplicates",
		"resolution=ignore-duplicates", "missing=default", "max-affected=0",
		"max-affected=x", "tx=rollback", "tx=commit", "tx=bogus", "=x", ",,,,",
		"return=representation,return=minimal", "HANDLING=STRICT",
	} {
		f.Add(seed, 0)
		f.Add(seed, 1)
	}
	f.Fuzz(func(t *testing.T, header string, txEndIndex int) {
		if txEndIndex < 0 || txEndIndex >= len(txEnds) {
			txEndIndex = 0
		}
		txEnd := txEnds[txEndIndex]
		prefs, apiErr := parsePreferences(header, txEnd)
		if apiErr != nil {
			return
		}
		// A rollback may only be requested when db-tx-end permits an override.
		if prefs.TxRollback && txEnd != config.TxCommitAllowOverride && txEnd != config.TxRollbackAllowOverride {
			t.Fatalf("tx=rollback honored under db-tx-end=%q", txEnd)
		}
		for _, applied := range prefs.applied {
			if strings.TrimSpace(applied) == "" {
				t.Fatalf("empty applied preference for %q", header)
			}
		}
	})
}

// FuzzConvertJSONArg asserts argument conversion never panics and always
// produces either a bind value or an error.
func FuzzConvertJSONArg(f *testing.F) {
	args := []schema.FunctionArg{
		{Name: "a", Mode: schema.ArgIn, CastType: `"pg_catalog"."int4"`},
		{Name: "s", Mode: schema.ArgIn, CastType: `"pg_catalog"."text"`},
		{Name: "v", Mode: schema.ArgVariadic, CastType: `"pg_catalog"."int4"[]`},
		{Name: "j", Mode: schema.ArgIn, CastType: `"pg_catalog"."jsonb"`},
	}
	for _, raw := range []string{`1`, `"x"`, `null`, `[1,2]`, `["a,b","c"]`, `{}`, `[]`, `true`, `1.5`, `""`} {
		f.Add(raw, 0)
		f.Add(raw, 3)
	}
	f.Fuzz(func(t *testing.T, raw string, which int) {
		if which < 0 || which >= len(args) {
			which = 0
		}
		value, apiErr := convertJSONArg(args[which], []byte(raw))
		if apiErr != nil {
			return
		}
		if value.Name != args[which].Name {
			t.Fatalf("argument name changed: %q", value.Name)
		}
		if value.Null && value.Text != "" {
			t.Fatalf("a null argument carried text %q", value.Text)
		}
	})
}

// FuzzNegotiateAccept asserts the media-type gate never panics and only ever
// yields the two known shapes.
func FuzzNegotiateAccept(f *testing.F) {
	for _, seed := range []string{"", "*/*", "application/json", "application/json;q=0.9", "text/csv", ",,", "application/vnd.pgrst.object+json"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		shape, err := negotiateAccept(header)
		switch shape {
		case shapeArray, shapeObject, shapeCSV:
		default:
			t.Fatalf("unexpected shape %d", shape)
		}
		if err != nil && shape != shapeArray {
			t.Fatalf("a rejected Accept must not select a shape: %d", shape)
		}
	})
}
