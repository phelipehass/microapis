// Command server sobe a API. Um binário, um processo, sem dependência externa:
// é o que permite rodar todos os produtos num VPS de 5 dólares.
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

	"github.com/phelipehass/microapis/internal/api"
	"github.com/phelipehass/microapis/internal/config"
	"github.com/phelipehass/microapis/internal/safefetch"
)

func main() {
	if err := run(); err != nil {
		slog.Error("servidor encerrado com erro", "erro", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	setupLogger(cfg)

	fetcher := safefetch.New(safefetch.Config{
		MaxBytes:        cfg.FetchMaxBytes,
		Timeout:         cfg.FetchTimeout,
		MaxRedirects:    cfg.MaxRedirects,
		UserAgent:       cfg.UserAgent,
		AllowPrivateIPs: cfg.AllowPrivateIPs,
	})

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: api.NewRouter(cfg, api.NewHandler(fetcher)),
		// ReadHeaderTimeout fecha o slowloris: conexão que manda cabeçalho
		// byte a byte e nunca termina, prendendo o servidor de graça.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("servidor no ar", "porta", cfg.Port, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Shutdown gracioso: no deploy, o container recebe SIGTERM e os requests
	// em voo terminam em vez de virar 502 na cara do assinante.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		slog.Info("encerrando", "sinal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

func setupLogger(cfg config.Config) {
	level := slog.LevelDebug
	var handler slog.Handler
	if cfg.IsProduction() {
		level = slog.LevelInfo
		// JSON em produção para o log virar consulta em vez de grep.
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(handler))
}
