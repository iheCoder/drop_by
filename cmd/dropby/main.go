package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"drop_by/internal/dropby"
	"drop_by/internal/observe"
)

func main() {
	if err := run(); err != nil {
		slog.Error("drop_by 未能正常运行", "error", err)
		os.Exit(1)
	}
}
func run() error {
	c, err := dropby.LoadConfig()
	if err != nil {
		return err
	}
	store, err := dropby.OpenStore(c.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	obs, openErr := observe.OpenLog(filepath.Join(c.DataDir, "observe.sqlite"))
	if openErr != nil {
		slog.Error("观测库没有打开，聊天继续", "error", openErr)
		obs = nil
	} else {
		go obs.Serve()
		defer obs.Close()
	}
	model := &dropby.HTTPModel{Config: c, Store: store, Client: &http.Client{Timeout: 4 * time.Minute}}
	engine := dropby.NewEngine(store, model, c)
	engine.Observe = obs
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: c.Address, Handler: dropby.NewHandler(engine), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	done := make(chan struct{})
	go func() { engine.Run(ctx); close(done) }()
	slog.Info("drop_by 已启动", "url", "http://"+c.Address, "model", c.Model, "model_ready", c.APIKey != "", "search_ready", c.SearchKey != "")
	select {
	case err = <-errCh:
		stop()
	case <-ctx.Done():
	}
	// 先取消后台生成并保存工作状态，再关闭连接和数据库。SSE 不会拖住退出。
	stop()
	<-done
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
