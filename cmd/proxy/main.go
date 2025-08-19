package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ReanSn0w/go-udp-proxy/internal/proxy"
	"github.com/ReanSn0w/gokit/pkg/app"
)

var (
	revision = "debug"
	opts     = struct {
		app.Debug

		Options proxy.Options `group:"options" namespace:"options" env-namespace:"OPTIONS"`

		Host struct {
			Port int `long:"port" env:"PORT" default:"8080" description:"host port"`
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
		opts.Host.Port,
		opts.Proxy.Host,
		opts.Proxy.Port,
		app.Log(),
		opts.Options,
	)

	// Обработка сигналов для graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		app.Log().Logf("[INFO] Received shutdown signal")
		server.Stop()
		app.Cancel()()
	}()

	// Запускаем сервер
	if err := server.Start(app.Context()); err != nil && err != context.Canceled {
		app.Log().Logf("[ERROR] Server error: %v", err)
		os.Exit(1)
	}
}
