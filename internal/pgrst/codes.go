// Package pgrst defines the PostgREST error codes Subquery returns, so the wire
// contract has a single source of truth across the query, schema, and HTTP
// layers.
//
// The values mirror PostgREST's own numbering: PGRST0xx is server startup,
// PGRST1xx request shape, PGRST2xx schema-cache resolution, PGRST3xx
// authentication, and PGRSTX00 an unexpected internal failure.
package pgrst

// Server startup and internal failures.
const (
	SchemaNotLoaded = "PGRST002"
	Internal        = "PGRSTX00"
)

// Request shape.
const (
	ParseError           = "PGRST100"
	InvalidBody          = "PGRST102"
	InvalidRange         = "PGRST103"
	InvalidPut           = "PGRST105"
	UnsupportedMediaType = "PGRST107"
	EmbedNotFound        = "PGRST108"
	PutKeyMismatch       = "PGRST115"
	MethodNotAllowed     = "PGRST117"
	InvalidEmbedFilter   = "PGRST120"
	InvalidPreferences   = "PGRST122"
	AggregatesDisabled   = "PGRST123"
	MaxAffected          = "PGRST124"
	InvalidPath          = "PGRST125"
	OpenAPIDisabled      = "PGRST126"
	NotImplemented       = "PGRST127"
	MaxAffectedRPC       = "PGRST128"
	NotSingular          = "PGRST116"
)

// Schema-cache resolution.
const (
	RelationshipNotFound = "PGRST200"
	AmbiguousEmbed       = "PGRST201"
	FunctionNotFound     = "PGRST202"
	AmbiguousFunction    = "PGRST203"
	ColumnNotFound       = "PGRST204"
	RelationNotFound     = "PGRST205"
)

// Authentication.
const (
	SecretMissing = "PGRST300"
	InvalidToken  = "PGRST301"
	AnonDisabled  = "PGRST302"
	InvalidClaims = "PGRST303"
)
