# Subquery

A PostgREST-compatible API over PostgreSQL, written in Go. Same routes, query
syntax, request/response shapes, headers, and `PGRST*` error codes.

The reference is **PostgREST 16.4** (the latest release); compatibility is
verified by replaying requests against stock PostgREST. It runs on **PostgreSQL
18**.

## Quick start

```sh
docker compose -f test/conformance/compose.yaml up --build
curl localhost:3000/_health
curl localhost:3000/todos
```

Pushing a `v*` tag publishes a `linux/amd64` + `linux/arm64` image to
`ghcr.io/andeshq/subquery`.

Reload the schema cache after DDL:

```sh
psql "$DB_URI" -c "notify pgrst, 'reload schema'"
# or send SIGUSR1 to the process
```

See [`examples/`](examples) for a runnable walkthrough: one example embeds the
[`pkg/subquery`](pkg/subquery) package from Go, the other is a SQL schema and
`curl` only.

## Configuration

Configuration is a PostgREST-style `key = value` file passed with `-config`,
then overridden by `PGRST_*` environment variables. See `subquery.conf.example`.

```sh
subquery -config /etc/subquery/subquery.conf
```

`jwt-secret` is auto-detected from its value:

| value | meaning |
| --- | --- |
| 32+ character string | HMAC passphrase (HS256/384/512) |
| base64 value + `jwt-secret-is-base64 = true` | decoded HMAC secret |
| `@/path/to/file` | a secret or a JWK/JWKS file |
| `{"kty":...}` or `{"keys":[...]}` | a literal JWK or JWKS |
| `https://issuer/.well-known/jwks.json` | a remote JWKS (a Subquery extension) |

RSA, EC (P-256/384/521), Ed25519, and `oct` keys are supported. Remote key sets
are cached by `Cache-Control` and served stale if the identity provider is
briefly unavailable.

A few parameters are accepted for compatibility but have no effect yet;
`internal/config/inert_test.go` lists them, so a deployment can never appear
configured when it is not.

## API

**Reads**

- `select` with columns, aliases (`fullName:full_name`), casts (`salary::text`),
  JSON paths (`meta->>genre`), and `*`.
- Filters: `eq neq gt gte lt lte like ilike match imatch in is isdistinct
  fts plfts phfts wfts cs cd ov sl sr nxr nxl adj`, `not.` negation,
  `any`/`all`, and `and`/`or`/`not.and`/`not.or` trees.
- `order` (`asc`/`desc`/`nullsfirst`/`nullslast`), `limit`/`offset`, the `Range`
  header, and `db-max-rows` capping.
- Embedding: foreign-key joins for many-to-one, one-to-many, one-to-one, and
  many-to-many; `alias:target(...)`, nested embeds, `!hint`, `!inner`,
  `is.null`/`not.is.null`, per-embed `order`/`limit`/filters, and `...spread`.
- `Prefer: count=exact`, with `Content-Range` and `200`/`206`.
- `Accept: application/vnd.pgrst.object+json` for one object (`PGRST116`/406 if
  the result is not exactly one row).
- `Accept: text/csv` for plain columns, aliases, and casts.
- Computed fields (a one-row-type scalar function becomes a virtual column),
  aggregates (`count`/`sum`/`avg`/`min`/`max` with an implicit `GROUP BY`), and
  full-text search on text and `json` columns.

**Writes**

- `POST` insert (single object or array), `columns=`, `on_conflict=`, and
  `Prefer: resolution=merge-duplicates`/`ignore-duplicates`.
- `PUT` single-row upsert (`201` insert / `200` replace), `PATCH`, and `DELETE`.
- `Prefer: return=minimal`/`headers-only`/`representation`; `representation`
  supports projection and embedding exactly as reads do. Also `missing=default`,
  `max-affected` with `handling=strict`, and `tx=rollback`.

**RPC** — `/rpc/{fn}` for functions the request role may execute: overload
resolution, named arguments, defaults, GET query parameters or a POST JSON body,
a single unnamed `json`/`jsonb` argument, and variadic arrays. Scalar, table,
record, and `void` results; `POST` only for volatile functions; `OPTIONS`
reports `Allow`.

**Auth** — the JWT role claim drives `SET LOCAL ROLE`, so RLS policies and grants
apply per request. The request transaction exposes `request.jwt.claims`,
`request.headers`, `request.cookies`, `request.method`, `request.path`, and
`app.settings.*`; `db-pre-request` runs inside it as the impersonated role.

**OpenAPI** — a Swagger 2.0 document generated from the schema cache at `/`.
`openapi-mode` is `follow-privileges` (default), `ignore-privileges`, or
`disabled`; `openapi-server-proxy-uri` overrides host, base path, and scheme.

## Query safety

`internal/sqlgen` is the only place that builds SQL, and it keeps three lanes
apart: request values become `$n` bind parameters cast to the resolved column
type, request names are resolved against the schema cache and quoted as
identifiers, and everything else comes from a fixed keyword set. No request text
is ever interpolated.

Reads are assembled inside PostgreSQL (`row_to_json`/`json_agg`) and streamed
through unchanged, so numeric, timestamp, and duplicate-key formatting match
PostgREST exactly.

## Architecture

```
cmd/subquery/        CLI and signal handling
internal/config/     file + PGRST_* loading and validation
internal/auth/       JWT verification and JWKS (static, file, remote)
internal/schema/     catalog introspection, relationship graph, snapshot
internal/query/      query-string parser
internal/sqlgen/     parameterized SQL generation
internal/pg/         pool and the per-request transaction
internal/httpapi/    router, content negotiation, PostgREST-shaped errors
internal/openapi/    generated Swagger 2.0
internal/observe/    logging
pkg/subquery/        embeddable server (New, Handler, Close)
test/dbtest/         embedded fixture schema and container provisioning
test/conformance/    request replay and diff against stock PostgREST
```

## Testing

```sh
go test ./...          # provisions a PostgreSQL container per package
go test -short ./...   # no database
```

The database-backed packages (`internal/pg`, `internal/schema`,
`internal/httpapi`) each start their own container and load the embedded fixture
schema in `test/dbtest/fixtures/`, so every package sees a pristine database and
packages cannot interfere. Docker is the only requirement.

Also in the suite: fuzz targets over the request parsers (asserting generated SQL
has no statement separator or escape and is deterministic for a given query),
race-detector tests over the schema cache and the remote JWKS cache, and
benchmarks over the request hot paths.

### Conformance

`test/conformance` replays a case set over HTTP against Subquery and against
stock PostgREST, each from a fresh database, then diffs at four levels: status,
`+body`, `+headers`, and `+Content-Range`. Prose is excluded (only the `PGRST`
code is compared for errors) and timestamps are normalized.

```sh
./test/conformance/run.sh
```

Current result: **100% on all 95 cases at every level**, with two deliberate
divergences recorded in the case files:

- `RETURNS TABLE(...)` functions: Subquery filters and orders them by their
  declared columns; PostgREST treats them as an untyped record (`42703`).
- CSV output covers plain columns, aliases, and casts only; a richer selection
  answers `406` where PostgREST renders it as CSV.

## Limitations

- Not implemented: computed-field *set* functions, the
  `application/vnd.pgrst.plan` media type, GeoJSON output (needs PostGIS), and
  `openapi-security-active`. `count=planned`/`estimated` compute an exact count.
- Aggregates cannot be combined with embeddings, and you cannot order by an
  aggregate's alias.
- Request bodies must be `application/json` (otherwise `415`). A bulk payload
  whose elements do not all share the same keys is refused with `PGRST102`, as
  PostgREST refuses it.
- `limit`/`offset`/`order` on `PATCH`/`DELETE` return `PGRST127` (PostgREST 16.4
  silently ignores them, which would update unbounded rows).
- RPC: stored procedures are not exposed; a single unnamed argument must be
  `json`/`jsonb`; a bare `RETURNS SETOF record` cannot be projected.
- The OpenAPI document describes Subquery accurately but is not byte-identical to
  PostgREST's.

## Not in v1

GraphQL, realtime subscriptions, pgvector helpers, custom routes, AWS Lambda and
Cloudflare Workers adapters, an admin server, and an admin UI.
