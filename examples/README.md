# Examples

Two ways to run Subquery over the same demo schema:

- **[`embedded/`](embedded/)** — a Go program that uses the
  [`pkg/subquery`](../pkg/subquery) package directly: it builds the
  configuration in code and mounts the handler itself.
- **[`sql-only/`](sql-only/)** — no Go at all: a SQL schema, a config file, and a
  `curl` walkthrough against the `subquery` binary.

Both use the schema in `sql-only/schema.sql`, so pick whichever fits and start
there.
