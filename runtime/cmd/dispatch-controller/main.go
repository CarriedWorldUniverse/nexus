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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	keyfilePath := flag.String("k", "/etc/nexus/keyfile.json", "controller keyfile")
	cursorDir := flag.String("cursor-dir", "", "directory for the Lock 6 message-cursor file (defaults to <cwd>/cursor)")
	contextMode := flag.String("context-mode", string(schemas.ContextThread), "context mode: global, thread, or stateless")
	namespace := flag.String("namespace", "nexus", "Job namespace")
	image := flag.String("image", "localhost/nexus-builder:dev", "worker image")
	nodeIP := flag.String("node-ip", "192.168.143.133", "dMon node IP for hostAliases")
	brokerHost := flag.String("broker-host", "dmonextreme.tail41686e.ts.net", "broker tailnet host")
	briefTimeout := flag.String("brief-timeout", "30m", "max builder wall-clock timeout passed to agentfunnel")
	gitCredName := flag.String("git-cred-name", os.Getenv("NEXUS_DISPATCH_GIT_CRED_NAME"), "git credential name to grant to dispatched builders (env: NEXUS_DISPATCH_GIT_CRED_NAME; empty skips grant)")
	maxConc := flag.Int("max-concurrent", 4, "max concurrent builder Jobs")
	healthAddr := flag.String("health-addr", ":8080", "HTTP listen address for /healthz (empty disables)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cm := schemas.ContextMode(*contextMode)
	switch cm {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		fail(log, fmt.Sprintf("invalid --context-mode %q (want global/thread/stateless)", *contextMode), nil)
	}

	kf, err := keyfile.Load(*keyfilePath)
	if err != nil {
		fail(log, "load keyfile", err)
	}
	log.Info("dispatch-controller: keyfile loaded",
		"path", *keyfilePath,
		"nexus_url", kf.Envelope.NexusURL,
		"nexus_id", kf.Envelope.NexusID)

	brokerTLS, err := kf.BrokerTLSConfig()
	if err != nil {
		fail(log, "build broker TLS config from keyfile", err)
	}
	if brokerTLS != nil {
		log.Info("dispatch-controller: trusting pinned broker cert from keyfile")
	}

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	client := &keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(brokerTLS)}
	res, err := client.Validate(bootCtx, kf)
	bootCancel()
	if err != nil {
		fail(log, "validate", err)
	}
	log.Info("dispatch-controller: validated",
		"aspect", res.AspectName,
		"jwt_expires", res.SessionExpiresAt.Format(time.RFC3339))

	rc, err := rest.InClusterConfig()
	if err != nil {
		fail(log, "in-cluster config", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		fail(log, "k8s client", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl := &dispatch.Controller{
		K8s: &dispatch.K8s{Client: cs, Namespace: *namespace},
		Cfg: dispatch.JobConfig{
			Image:         *image,
			Namespace:     *namespace,
			NodeIP:        *nodeIP,
			BrokerHost:    *brokerHost,
			BriefTimeout:  *briefTimeout,
			GitCredName:   *gitCredName,
			LynxAIBaseURL: os.Getenv("LYNXAI_BASE_URL"),
			LynxAIKey:     os.Getenv("LYNXAI_KEY"),
		},
		MaxConc: *maxConc,
	}
	if err := ctrl.Init(ctx); err != nil {
		fail(log, "dispatch controller init", err)
	}

	wsURL := res.NexusURL
	if !strings.HasSuffix(wsURL, "/connect") && !strings.HasSuffix(wsURL, "/connect/") {
		wsURL = strings.TrimRight(wsURL, "/") + "/connect"
	}

	state := newSessionState(sessionSnapshot{JWT: res.SessionJWT, Expires: res.SessionExpiresAt})
	tokenProvider := func(ctx context.Context) (string, error) {
		snap := state.Snapshot()
		if snap.JWT != "" && time.Until(snap.Expires) > time.Minute {
			return snap.JWT, nil
		}
		fresh, ferr := (&keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(brokerTLS)}).Validate(ctx, kf)
		if ferr != nil {
			log.Warn("dispatch-controller: TokenProvider re-validate failed", "err", ferr)
			return "", ferr
		}
		state.Set(sessionSnapshot{JWT: fresh.SessionJWT, Expires: fresh.SessionExpiresAt})
		log.Info("dispatch-controller: TokenProvider re-validated via keyfile",
			"expires", fresh.SessionExpiresAt.Format(time.RFC3339))
		return fresh.SessionJWT, nil
	}

	sessionID := uuid.NewString()
	registerProvider := dispatchControllerRegisterProvider(res.Provider)
	if registerProvider != res.Provider {
		log.Warn("dispatch-controller: validation response has empty provider; using controller registration provider",
			"provider", registerProvider)
	}
	wsCfg := wsasp.Config{
		URL:           wsURL,
		AuthToken:     res.SessionJWT,
		TokenProvider: tokenProvider,
		TLSConfig:     brokerTLS,
		PingInterval:  10 * time.Second,
		PingTimeout:   5 * time.Second,
		AspectName:    res.AspectName,
		CursorFile:    wsasp.CursorFileForAspect(resolveCursorDir(*cursorDir)),
		OnDeliver: func(msg wsasp.DeliveredMessage) {
			ctrl.HandleMessage(ctx, []byte(msg.Content))
		},
		Register: schemas.RegisterRequest{
			Name:           res.AspectName,
			ContextMode:    cm,
			Provider:       registerProvider,
			PID:            os.Getpid(),
			StartedAt:      time.Now().UTC(),
			Model:          res.Model,
			SessionID:      sessionID,
			PrimarySurface: schemas.SurfaceFunnel,
		},
		Logger: log,
	}
	wsClient, err := wsasp.NewClient(wsCfg)
	if err != nil {
		fail(log, "wsasp.NewClient", err)
	}
	ctrl.Poster = dispatch.NewWsPoster(ctx, wsClient)
	if *healthAddr != "" {
		startHealthServer(ctx, *healthAddr, wsClient, log)
	}

	go jwtExpiryMonitor(ctx, func() time.Time { return state.Snapshot().Expires }, time.Minute, stop, log)
	go func() {
		if err := ctrl.WatchLoop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("dispatch-controller: watch jobs", "err", err)
			stop()
		}
	}()

	log.Info("dispatch-controller: up", "aspect", res.AspectName, "ns", *namespace, "max", *maxConc)
	if err := wsClient.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("dispatch-controller: wsClient.Run", "err", err)
		os.Exit(1)
	}
	log.Info("dispatch-controller: stopped")
}

func dispatchControllerRegisterProvider(provider string) string {
	if strings.TrimSpace(provider) == "" {
		return "dispatch-controller"
	}
	return provider
}

type readinessChecker interface {
	Connected() bool
	Ready() bool
}

func dispatchHealthHandler(checker readinessChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch {
		case !checker.Connected():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("ws disconnected\n"))
		case !checker.Ready():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("ws registering\n"))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		}
	})
}

func startHealthServer(ctx context.Context, addr string, checker readinessChecker, log *slog.Logger) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           dispatchHealthHandler(checker),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("dispatch-controller: health server shutdown failed", "err", err)
		}
	}()
	go func() {
		log.Info("dispatch-controller: health server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("dispatch-controller: health server failed", "err", err)
		}
	}()
}

func fail(log *slog.Logger, what string, err error) {
	if err == nil {
		log.Error("dispatch-controller: " + what)
	} else {
		log.Error("dispatch-controller: "+what, "err", err)
	}
	os.Exit(1)
}

type sessionSnapshot struct {
	JWT     string
	Expires time.Time
}

type sessionState struct {
	ch chan func(*sessionSnapshot)
}

func newSessionState(initial sessionSnapshot) *sessionState {
	s := &sessionState{ch: make(chan func(*sessionSnapshot))}
	go func() {
		current := initial
		for fn := range s.ch {
			fn(&current)
		}
	}()
	return s
}

func (s *sessionState) Snapshot() sessionSnapshot {
	resp := make(chan sessionSnapshot, 1)
	s.ch <- func(current *sessionSnapshot) { resp <- *current }
	return <-resp
}

func (s *sessionState) Set(next sessionSnapshot) {
	s.ch <- func(current *sessionSnapshot) { *current = next }
}

func jwtExpiryMonitor(ctx context.Context, expiryFn func() time.Time, lead time.Duration, stop context.CancelFunc, log *slog.Logger) {
	for {
		wakeAt := expiryFn().Add(-lead)
		d := time.Until(wakeAt)
		if d <= 0 {
			break
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
		if time.Until(expiryFn().Add(-lead)) > 0 {
			continue
		}
		break
	}
	log.Info("dispatch-controller: JWT nearing expiry - exiting for supervisor restart",
		"jwt_expires", expiryFn().Format(time.RFC3339),
		"lead", lead.String())
	stop()
}

func resolveCursorDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "cursor"
	}
	return filepath.Join(cwd, "cursor")
}
