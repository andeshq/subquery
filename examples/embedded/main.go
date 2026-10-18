// Command embedded runs Subquery as a library instead of through the CLI: it
// builds the configuration in code, starts an HTTP server around the handler,
// and shuts down on SIGINT or SIGTERM.
//
// It uses the demo schema from ../sql-only/schema.sql:
//
//	createdb app
//	psql -d app -f examples/sql-only/schema.sql
//	go run ./examples/embedded
//
// From another shell:
//
//	curl localhost:3000/books?select=title,year&order=year.desc
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andeshq/subquery/pkg/subquery"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Start from the documented defaults and override in code. To read a
	// PostgREST-style file and apply PGRST_* variables instead, use
	// subquery.LoadConfig(path).
	cfg := subquery.DefaultConfig()
	cfg.DBURI = env("DATABASE_URL", "postgres://authenticator:secret@localhost:5432/app")
	cfg.DBSchemas = []string{"public"}
	cfg.DBAnonRole = "anon"
	cfg.JWTSecret = env("JWT_SECRET", "reallyreallyreallyreallyverysafe!")
	cfg.LogLevel = "info"

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := subquery.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	server := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s, schemas %v", server.Addr, cfg.DBSchemas)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}
	log.Print("shutting down")

	// A host application can also reach srv.Pool() to share the connection pool
	// and srv.TriggerSchemaReload() to refresh the schema cache after DDL.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
