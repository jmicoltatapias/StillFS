package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jmicoltatapias/stillfs/internal/config"
	"github.com/jmicoltatapias/stillfs/internal/httpapi"
	"github.com/jmicoltatapias/stillfs/internal/logging"
	"github.com/jmicoltatapias/stillfs/internal/postgres"
	"github.com/jmicoltatapias/stillfs/internal/storage/local"
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	logger := logging.New(os.Stdout)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration failed", map[string]interface{}{"error": err.Error()})
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupCtx, cancelStartup := context.WithTimeout(ctx, cfg.Database.ConnectTimeout)
	db, err := postgres.Open(startupCtx, cfg.Database)
	cancelStartup()
	if err != nil {
		logger.Error("database connection failed", map[string]interface{}{"error": err.Error()})
		return 1
	}
	defer db.Close()
	if err := postgres.Migrate(ctx, db); err != nil {
		logger.Error("database migration failed", map[string]interface{}{"error": err.Error()})
		return 1
	}
	if _, err := local.New(cfg.StorageRoot); err != nil {
		logger.Error("object storage initialization failed", map[string]interface{}{"error": err.Error()})
		return 1
	}
	if err := run(ctx, cfg, logger, postgres.Checker{DB: db}); err != nil {
		logger.Error("server stopped with error", map[string]interface{}{"error": err.Error()})
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg config.Config, logger *logging.Logger, database httpapi.HealthChecker) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      httpapi.NewHandler(database),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", map[string]interface{}{"address": listener.Addr().String()})
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
