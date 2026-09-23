// Command irp runs the Intuitive Response Proxy between Claude Code and the Anthropic API.
//
//	ANTHROPIC_BASE_URL=http://127.0.0.1:18766 claude
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/McKean/intuitive-response/internal/admin"
	"github.com/McKean/intuitive-response/internal/config"
	"github.com/McKean/intuitive-response/internal/logx"
	"github.com/McKean/intuitive-response/internal/presets"
	"github.com/McKean/intuitive-response/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "irp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logFile, err := logx.OpenFile(filepath.Join(cfg.StateDir, "proxy.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	log := logx.NewLogger(logFile)
	stats := &logx.Stats{}

	var traffic *logx.Traffic
	if cfg.TrafficLog {
		if traffic, err = logx.NewTraffic(filepath.Join(cfg.StateDir, "traffic")); err != nil {
			return err
		}
	}

	store := presets.NewStore(cfg.MaxPresets, cfg.ReplaceMode)
	px, err := proxy.New(cfg, store, log, stats, traffic)
	if err != nil {
		return err
	}
	go px.WarmJev()
	mux := http.NewServeMux()
	admin.Register(mux, store, stats)
	mux.Handle("/", px)

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	log.Log("start", map[string]any{
		"addr": addr, "upstream": cfg.Upstream, "inject": cfg.Inject,
		"replace_mode": cfg.ReplaceMode, "max_presets": cfg.MaxPresets,
		"jev_enabled": cfg.TypeSafeAPIKey != "", "jev_model": cfg.JevModel,
		"jev_timeout_ms": cfg.JevTimeout.Milliseconds(), "match_mode": cfg.MatchMode, "traffic_log": cfg.TrafficLog,
	})
	fmt.Fprintf(os.Stderr, "irp listening on http://%s (upstream %s, log %s)\n",
		addr, cfg.Upstream, logFile.Name())

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}
	return nil
}
