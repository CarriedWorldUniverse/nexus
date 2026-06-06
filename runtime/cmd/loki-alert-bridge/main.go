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
	"strings"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/google/uuid"
)

type Config struct {
	ListenAddr  string
	LokiURL     string
	BrokerAddr  string
	KeyfilePath string
	DedupWindow time.Duration
	QueryWindow time.Duration
	LineLimit   int
	ByteLimit   int
	Mention     string
}

func main() {
	cfg := parseConfig()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	if err := run(context.Background(), cfg, log); err != nil {
		log.Error("loki-alert-bridge: exit", "err", err)
		os.Exit(1)
	}
}

func parseConfig() Config {
	cfg := Config{}
	flag.StringVar(&cfg.ListenAddr, "listen", env("NEXUS_LOKI_ALERT_BRIDGE_LISTEN", ":8080"), "HTTP listen address")
	flag.StringVar(&cfg.LokiURL, "loki-url", env("NEXUS_LOKI_URL", "http://loki:3100"), "Loki base URL")
	flag.StringVar(&cfg.BrokerAddr, "broker-addr", env("NEXUS_BROKER_ADDR", ""), "broker WS URL override; defaults to keyfile envelope")
	flag.StringVar(&cfg.KeyfilePath, "keyfile", env("NEXUS_OBSERVER_KEYFILE", ""), "observer keyfile path")
	flag.DurationVar(&cfg.DedupWindow, "dedup-window", envDuration("NEXUS_LOKI_ALERT_DEDUP_WINDOW", 30*time.Minute), "deduplication window")
	flag.DurationVar(&cfg.QueryWindow, "query-window", envDuration("NEXUS_LOKI_ALERT_QUERY_WINDOW", 10*time.Minute), "recent log window to query")
	flag.IntVar(&cfg.LineLimit, "line-limit", envInt("NEXUS_LOKI_ALERT_LINE_LIMIT", defaultMaxExcerptLines), "max log lines in chat excerpt")
	flag.IntVar(&cfg.ByteLimit, "byte-limit", envInt("NEXUS_LOKI_ALERT_BYTE_LIMIT", defaultMaxExcerptBytes), "max bytes in chat excerpt")
	flag.StringVar(&cfg.Mention, "mention", env("NEXUS_LOKI_ALERT_MENTION", "keel"), "aspect to @-mention")
	flag.Parse()
	return cfg
}

func run(parent context.Context, cfg Config, log *slog.Logger) error {
	if cfg.KeyfilePath == "" {
		return errors.New("missing --keyfile or NEXUS_OBSERVER_KEYFILE")
	}
	if cfg.LokiURL == "" {
		return errors.New("missing --loki-url or NEXUS_LOKI_URL")
	}
	kf, err := keyfile.Load(cfg.KeyfilePath)
	if err != nil {
		return fmt.Errorf("load keyfile: %w", err)
	}
	tlsCfg, err := kf.BrokerTLSConfig()
	if err != nil {
		return fmt.Errorf("broker TLS config: %w", err)
	}
	bootCtx, cancel := context.WithTimeout(parent, 30*time.Second)
	res, err := (&keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(tlsCfg)}).Validate(bootCtx, kf)
	cancel()
	if err != nil {
		return fmt.Errorf("validate keyfile: %w", err)
	}
	wsURL := cfg.BrokerAddr
	if wsURL == "" {
		wsURL = res.NexusURL
	}
	wsURL = ensureConnectPath(wsURL)

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	sessionID := uuid.NewString()
	tokenProvider := func(ctx context.Context) (string, error) {
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		fresh, err := (&keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(tlsCfg)}).Validate(refreshCtx, kf)
		if err != nil {
			return "", err
		}
		return fresh.SessionJWT, nil
	}
	wsClient, err := wsasp.NewClient(wsasp.Config{
		URL:           wsURL,
		AuthToken:     res.SessionJWT,
		TokenProvider: tokenProvider,
		TLSConfig:     tlsCfg,
		AspectName:    res.AspectName,
		OnDeliver:     func(wsasp.DeliveredMessage) {},
		Register: schemas.RegisterRequest{
			Name:           res.AspectName,
			ContextMode:    schemas.ContextThread,
			Provider:       "loki-alert-bridge",
			PID:            os.Getpid(),
			StartedAt:      time.Now().UTC(),
			Model:          "observer",
			SessionID:      sessionID,
			PrimarySurface: schemas.SurfaceFunnel,
		},
		Logger: log,
	})
	if err != nil {
		return fmt.Errorf("ws client: %w", err)
	}
	go func() {
		if err := wsClient.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("loki-alert-bridge: ws client stopped", "err", err)
			stop()
		}
	}()
	service := &Service{
		Loki:      &LokiClient{BaseURL: cfg.LokiURL, HTTP: &http.Client{Timeout: 10 * time.Second}},
		Chat:      &wsChatPoster{client: wsClient},
		Dedup:     NewDeduper(cfg.DedupWindow),
		Clock:     realClock{},
		Log:       log,
		Window:    cfg.QueryWindow,
		LineLimit: cfg.LineLimit,
		ByteLimit: cfg.ByteLimit,
		Mention:   cfg.Mention,
	}
	if err := service.Validate(); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/alertmanager", service)
	mux.Handle("/webhook", service)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Info("loki-alert-bridge: listening",
		"addr", cfg.ListenAddr,
		"loki_url", cfg.LokiURL,
		"broker", wsURL,
		"aspect", res.AspectName)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type wsChatPoster struct {
	client *wsasp.Client
}

func (p *wsChatPoster) PostChat(ctx context.Context, content string) error {
	_, err := p.client.SendChat(ctx, content, 0, "loki-alert")
	return err
}

func ensureConnectPath(raw string) string {
	if strings.HasSuffix(raw, "/connect") || strings.HasSuffix(raw, "/connect/") {
		return raw
	}
	return strings.TrimRight(raw, "/") + "/connect"
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		var out int
		if _, err := fmt.Sscanf(v, "%d", &out); err == nil {
			return out
		}
	}
	return fallback
}
