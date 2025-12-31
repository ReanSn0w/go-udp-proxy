package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ReanSn0w/go-udp-proxy/internal/proxy"
	"github.com/ReanSn0w/gokit/pkg/app"
)

var (
	revision = "debug"
	opts     = struct {
		app.Debug

		Options proxy.Options `group:"options" namespace:"options" env-namespace:"OPTIONS"`

		Host struct {
			Port int    `long:"port" env:"PORT" default:"8080" description:"host port"`
			Addr string `long:"addr" env:"ADDR" default:"127.0.0.1" description:"host address to listen on"`
		} `group:"host" namespace:"host" env-namespace:"HOST"`

		Proxy struct {
			Host string `long:"host" env:"HOST" default:"127.0.0.1" description:"proxy host"`
			Port int    `long:"port" env:"PORT" default:"9090" description:"proxy port"`
		} `group:"proxy" namespace:"proxy" env-namespace:"PROXY"`
	}{}
)

func main() {
	app := app.New("UDP Proxy Server", revision, &opts)

	// Создаем UDP прокси сервер
	server := proxy.NewServer(
		opts.Host.Addr,
		opts.Host.Port,
		opts.Proxy.Host,
		opts.Proxy.Port,
		app.Log(),
		opts.Options,
	)

	ctx, cancel := context.WithCancel(app.Context())
	var shutdownOnce sync.Once
	var wg sync.WaitGroup

	// Обработка сигналов для graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		app.Log().Logf("[INFO] Received shutdown signal, gracefully shutting down...")

		shutdownOnce.Do(func() {
			cancel()
			server.Stop()
		})
	}()

	// Запускаем сервер
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Start(ctx); err != nil && err != context.Canceled {
			app.Log().Logf("[ERROR] Server error: %v", err)
			shutdownOnce.Do(func() {
				cancel()
				server.Stop()
			})
		}
	}()

	// Ожидаем завершения сервера
	wg.Wait()

	// Даем некоторое время на завершение горутин
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	server.WaitShutdown(shutdownCtx)
	app.Log().Logf("[INFO] Server stopped")
}
