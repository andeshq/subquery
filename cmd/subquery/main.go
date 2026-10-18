// Command subquery serves a PostgREST-compatible API over PostgreSQL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andeshq/subquery/internal/observe"
	"github.com/andeshq/subquery/pkg/subquery"
)

var version = "0.1.0-dev"

// Server timeouts bound slow and idle clients. readTimeout covers the request
// line, headers, and body; writeTimeout bounds the response, including a large
// JSON body; idleTimeout reaps keep-alive connections that go quiet.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "subquery:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("subquery", flag.ExitOnError)
	configPath := fs.String("config", "", "path to a PostgREST-style configuration file")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("subquery", version)
		return nil
	}

	cfg, err := subquery.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	log := observe.NewLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := subquery.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(reload)
	go func() {
		for sig := range reload {
			switch sig {
			case syscall.SIGUSR1:
				log.Info("schema cache reload requested")
				srv.TriggerSchemaReload()
			case syscall.SIGUSR2:
				log.Info("configuration reload requested", "implemented", false)
			}
		}
	}()

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpServer.Addr, "schemas", cfg.DBSchemas)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
