package httpapi

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/pg"
	"github.com/andeshq/subquery/internal/schema"
)

// relationAllow derives the Allow header from the request role's privileges
// rather than from the route table, as PostgREST does.
func (s *Server) relationAllow(ctx context.Context, rel *schema.Relation, role string) (string, error) {
	qualified := pgx.Identifier{rel.ID.Schema, rel.ID.Name}.Sanitize()
	notTable := rel.Kind != schema.KindTable && rel.Kind != schema.KindPartitioned

	priv, err := pg.RelationPrivileges(ctx, s.pool, role, qualified, notTable)
	if err != nil {
		return "", err
	}

	verbs := []string{"OPTIONS"}
	if priv.Select {
		verbs = append(verbs, "GET", "HEAD")
	}
	if priv.Insert {
		verbs = append(verbs, "POST")
	}
	// PUT replaces one row named by its key, so it needs a primary key.
	if priv.Update && len(rel.PrimaryKey) > 0 {
		verbs = append(verbs, "PUT")
	}
	if priv.Update {
		verbs = append(verbs, "PATCH")
	}
	if priv.Delete {
		verbs = append(verbs, "DELETE")
	}
	return strings.Join(verbs, ","), nil
}
