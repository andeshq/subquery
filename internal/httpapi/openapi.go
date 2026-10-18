package httpapi

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/openapi"
	"github.com/andeshq/subquery/internal/pg"
	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/schema"
)

// apiVersion is reported in the generated OpenAPI document.
const apiVersion = "0.1.0"

// handleRoot serves the generated Swagger 2.0 document.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.writeError(w, r, errMethodNotAllowed)
		return
	}
	if s.cfg.OpenAPIMode == config.OpenAPIDisabled {
		s.writeError(w, r, errOpenAPIDisabled)
		return
	}

	r, principal, apiErr := s.authenticate(r)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}
	snap := s.store.Snapshot()
	if snap == nil {
		s.writeError(w, r, &APIError{
			Status:  http.StatusInternalServerError,
			Code:    pgrst.SchemaNotLoaded,
			Message: "The schema cache has not been loaded",
		})
		return
	}

	opts := openapi.Options{
		Version:  apiVersion,
		Host:     r.Host,
		Schemes:  requestSchemes(r),
		ProxyURI: s.cfg.OpenAPIServerProxyURI,
	}

	// follow-privileges describes only what the requesting role may reach.
	if s.cfg.OpenAPIMode == config.OpenAPIFollowPrivileges {
		allowedRelations, allowedSchemas, err := s.allowedObjects(r.Context(), snap, principal.Role)
		if err != nil {
			s.writeDBError(w, r, err)
			return
		}
		opts.IncludeRel = func(rel *schema.Relation) bool {
			return allowedRelations[identityText(rel.ID)]
		}
		opts.IncludeFunc = func(fn *schema.Function) bool {
			return allowedSchemas[fn.ID.Schema]
		}
	}

	document := openapi.Build(snap, opts)
	payload, err := json.Marshal(document)
	if err != nil {
		s.writeError(w, r, &APIError{Status: http.StatusInternalServerError, Code: pgrst.Internal, Message: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// allowedObjects reports which relations the role may read and which schemas it
// may use.
func (s *Server) allowedObjects(ctx context.Context, snap *schema.Snapshot, role string) (map[string]bool, map[string]bool, error) {
	var (
		relationNames []string
		schemaNames   []string
	)
	seenSchema := map[string]bool{}
	for _, id := range snap.Order {
		if !snap.Relations[id].Exposed {
			continue
		}
		relationNames = append(relationNames, identityText(id))
		if !seenSchema[id.Schema] {
			seenSchema[id.Schema] = true
			schemaNames = append(schemaNames, id.Schema)
		}
	}

	relations, err := pg.AllowedRelations(ctx, s.pool, role, relationNames)
	if err != nil {
		return nil, nil, err
	}
	schemas, err := pg.AllowedSchemas(ctx, s.pool, role, schemaNames)
	if err != nil {
		return nil, nil, err
	}
	return relations, schemas, nil
}

// identityText renders a relation as the quoted text has_table_privilege wants.
func identityText(id schema.RelID) string {
	return pgx.Identifier{id.Schema, id.Name}.Sanitize()
}

// requestSchemes reports the schemes the client reached us over.
func requestSchemes(r *http.Request) []string {
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		if scheme, _, found := strings.Cut(forwarded, ","); found || scheme != "" {
			return []string{strings.TrimSpace(scheme)}
		}
	}
	if r.TLS != nil {
		return []string{"https"}
	}
	return []string{"http"}
}
