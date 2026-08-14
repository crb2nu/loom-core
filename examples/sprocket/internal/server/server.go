package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crb2nu/sprocket/internal/config"
	"github.com/crb2nu/sprocket/internal/handler"
	"github.com/crb2nu/sprocket/internal/repository"
	"github.com/crb2nu/sprocket/internal/service"
)

func Run(ctx context.Context) error {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	repo := repository.NewMemorySprocketRepository()
	svc := service.NewSprocketService(repo)
	sprocketHandler := handler.NewSprocketHandler(svc, logger)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           sprocketHandler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Info("starting sprocket service", "addr", cfg.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case <-runCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errc
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
