package httpapi

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/andeshq/subquery/internal/config"
)

// openapiDoc fetches and decodes the generated document.
func openapiDoc(t *testing.T, s *Server, headers map[string]string) map[string]any {
	t.Helper()
	rec := get(t, s, "/", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	return doc
}

func pathsOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths missing from the document: %v", doc)
	}
	return paths
}

func TestLiveOpenAPIDocument(t *testing.T) {
	s := newLiveServer(t)
	doc := openapiDoc(t, s, nil)

	if doc["swagger"] != "2.0" {
		t.Errorf("swagger = %v, want 2.0", doc["swagger"])
	}
	if doc["basePath"] != "/" {
		t.Errorf("basePath = %v", doc["basePath"])
	}

	paths := pathsOf(t, doc)
	for _, want := range []string{"/films", "/todos", "/notes", "/products", "/rpc/add_them", "/rpc/best_films"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("path %s is missing", want)
		}
	}

	definitions, ok := doc["definitions"].(map[string]any)
	if !ok {
		t.Fatalf("definitions missing: %v", doc)
	}
	films, ok := definitions["films"].(map[string]any)
	if !ok {
		t.Fatalf("films definition missing: %v", definitions)
	}
	properties, ok := films["properties"].(map[string]any)
	if !ok {
		t.Fatalf("films properties missing: %v", films)
	}
	if _, ok := properties["title"]; !ok {
		t.Errorf("films is missing the title property: %v", properties)
	}
	// The computed field is described as a property.
	if _, ok := properties["film_label"]; !ok {
		t.Errorf("computed field film_label is missing: %v", properties)
	}

	// A table-valued function advertises the read parameters.
	titles, ok := paths["/rpc/film_titles"].(map[string]any)
	if !ok {
		t.Fatalf("/rpc/film_titles missing")
	}
	if _, ok := titles["get"]; !ok {
		t.Errorf("film_titles should answer GET: %v", titles)
	}
}

func TestLiveOpenAPIFollowsPrivileges(t *testing.T) {
	s := newLiveServer(t)

	// Anonymous can read secrets, so it is described.
	if _, ok := pathsOf(t, openapiDoc(t, s, nil))["/secrets"]; !ok {
		t.Error("/secrets should be described for anon")
	}

	// The viewer role holds SELECT on films and notes only.
	viewer := token(t, map[string]any{"role": "viewer", "sub": "user_123"})
	paths := pathsOf(t, openapiDoc(t, s, bearer(viewer)))
	if _, ok := paths["/secrets"]; ok {
		t.Error("/secrets should be hidden from a role without SELECT")
	}
	if _, ok := paths["/films"]; !ok {
		t.Error("/films should be described for a role with SELECT")
	}
}

func TestLiveOpenAPIIgnoresPrivileges(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) {
		c.OpenAPIMode = config.OpenAPIIgnorePrivileges
	})
	viewer := token(t, map[string]any{"role": "viewer", "sub": "user_123"})
	if _, ok := pathsOf(t, openapiDoc(t, s, bearer(viewer)))["/secrets"]; !ok {
		t.Error("/secrets should be described when privileges are ignored")
	}
}

func TestLiveOpenAPIProxyURI(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) {
		c.OpenAPIServerProxyURI = "https://api.example.com/v1"
	})
	doc := openapiDoc(t, s, nil)
	if doc["host"] != "api.example.com" || doc["basePath"] != "/v1" {
		t.Errorf("host = %v, basePath = %v", doc["host"], doc["basePath"])
	}
}
