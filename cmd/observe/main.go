package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"drop_by/internal/observe"
)

func main() {
	if err := run(); err != nil {
		slog.Error("测光未能正常运行", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr, err := observe.ListenAddress()
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: addr, Handler: observe.NewHandler(),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("测光已启动", "url", "http://"+addr, "source", "sample")
	select {
	case err = <-errCh:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
