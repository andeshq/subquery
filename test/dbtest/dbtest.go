// Package dbtest provisions the fixture PostgreSQL that the database-backed
// tests run against, so that `go test ./...` needs nothing but Docker.
//
// Each package that has database tests calls Main from its TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }
//
// That gives the package its own container, and therefore its own pristine
// database, so packages running concurrently cannot interfere with each other.
//
//	go test ./...        provisions a container per package
//	go test -short ./... provisions nothing; database tests skip
//
// The fixture schema is embedded from fixtures/00-fixtures.sql, which is the
// same file the test compose stack mounts, so the development stack and the
// tests can never drift apart.
package dbtest

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

//go:embed fixtures/00-fixtures.sql
var fixtureSQL []byte

const (
	// image is pinned to match the test compose stack.
	image = "postgres:18"
	// database is created by the image; the fixture script fills it in.
	database = "app"
	// The tests connect as the fixture's own low-privilege role, never as the
	// container superuser, so row-level security and grants behave as in
	// production.
	runningUser     = "authenticator"
	runningPassword = "secret"

	startupTimeout  = 3 * time.Minute
	shutdownTimeout = time.Minute
	logDumpLimit    = 8 << 10
)

// dsn is the connection string for the package currently running. It is written
// once before m.Run and only read afterwards, so no synchronisation is needed.
var dsn string

// Main provisions the fixture database, runs the tests, and tears the container
// down. It returns a process exit code.
func Main(m *testing.M) int {
	// The testing flags are registered before TestMain runs but not yet parsed,
	// and testing.Short panics unless they have been.
	flag.Parse()

	if testing.Short() {
		// Database tests skip themselves, so provisioning would be wasted work.
		return m.Run()
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	connection, cleanup, err := Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dbtest:", err)
		return 1
	}
	defer cleanup()

	dsn = connection
	return m.Run()
}

// DSN returns the fixture database's connection string, skipping the test when
// the suite is running in short mode.
func DSN(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("database tests are skipped in short mode; run without -short to provision one")
	}
	if dsn == "" {
		t.Fatal("dbtest: the fixture database was not provisioned; call dbtest.Main from TestMain")
	}
	return dsn
}

// Start provisions a PostgreSQL container with the fixture schema loaded and
// returns its connection string plus a cleanup function.
func Start(ctx context.Context) (string, func(), error) {
	noop := func() {}

	dir, err := os.MkdirTemp("", "subquery-fixtures-")
	if err != nil {
		return "", noop, fmt.Errorf("create fixture directory: %w", err)
	}
	// testcontainers copies init scripts from the host, so the embedded schema
	// is materialised once per container.
	script := filepath.Join(dir, "00-fixtures.sql")
	if err := os.WriteFile(script, fixtureSQL, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", noop, fmt.Errorf("write fixture script: %w", err)
	}

	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase(database),
		postgres.WithInitScripts(script),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", noop, fmt.Errorf("start %s: %w%s", image, err, logsOf(container))
	}

	connection, err := connectionString(ctx, container)
	if err != nil {
		terminate(container)
		_ = os.RemoveAll(dir)
		return "", noop, err
	}

	cleanup := func() {
		terminate(container)
		_ = os.RemoveAll(dir)
	}
	return connection, cleanup, nil
}

// connectionString builds the DSN the tests use: the fixture's own role, not
// the container's superuser.
func connectionString(ctx context.Context, container *postgres.PostgresContainer) (string, error) {
	host, err := container.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return "", fmt.Errorf("resolve container port: %w", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		runningUser, runningPassword, host, port.Port(), database), nil
}

// terminate stops the container and always completes, even if the caller's
// context has already been cancelled.
func terminate(container *postgres.PostgresContainer) {
	if container == nil {
		return
	}
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintln(os.Stderr, "dbtest: terminate container:", err)
	}
}

// logsOf returns the start of the container's logs, which is where a failed
// init script reports its error.
func logsOf(container *postgres.PostgresContainer) string {
	if container == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	reader, err := container.Logs(ctx)
	if err != nil {
		return ""
	}
	defer reader.Close()

	tail, err := io.ReadAll(io.LimitReader(reader, logDumpLimit))
	if err != nil || len(tail) == 0 {
		return ""
	}
	return "\n--- container logs ---\n" + string(tail)
}
