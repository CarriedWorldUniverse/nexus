// Command outpost is the Outpost relay binary. Runs on a host that
// has aspects; accepts local aspect WS connections and multiplexes
// them upstream to Nexus (or another Outpost).
//
// Usage:
//
//	outpost
//
// Env:
//
//	NEXUS_UPSTREAM      Required. Nexus /connect URL.
//	NEXUS_TOKEN         Required. Shared bearer token.
//	OUTPOST_LISTEN      Optional, default :7950. Local WS listen addr.
//	OUTPOST_ID          Optional, defaults to hostname.
//	NEXUS_TLS_CERT      Required. Path to TLS server cert PEM.
//	NEXUS_TLS_KEY       Required. Path to TLS server key PEM.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexus-cw/nexus/nexus/outpost"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	upstream := os.Getenv("NEXUS_UPSTREAM")
	if upstream == "" {
		log.Error("NEXUS_UPSTREAM required")
		os.Exit(2)
	}
	token := os.Getenv("NEXUS_TOKEN")
	if token == "" {
		log.Error("NEXUS_TOKEN required")
		os.Exit(2)
	}
	listenAddr := os.Getenv("OUTPOST_LISTEN")
	if listenAddr == "" {
		listenAddr = ":7950"
	}
	outpostID := os.Getenv("OUTPOST_ID")
	if outpostID == "" {
		host, err := os.Hostname()
		if err != nil {
			log.Error("cannot resolve hostname", "err", err)
			os.Exit(2)
		}
		outpostID = host
	}

	tlsCert := os.Getenv("NEXUS_TLS_CERT")
	tlsKey := os.Getenv("NEXUS_TLS_KEY")
	if tlsCert == "" || tlsKey == "" {
		log.Error("NEXUS_TLS_CERT and NEXUS_TLS_KEY required (run `nexus cert init` to provision)")
		os.Exit(2)
	}

	o, err := outpost.New(outpost.Config{
		ListenAddr:  listenAddr,
		UpstreamURL: upstream,
		AuthToken:   token,
		OutpostID:   outpostID,
		TLSCertFile: tlsCert,
		TLSKeyFile:  tlsKey,
		Logger:      log,
	})
	if err != nil {
		log.Error("outpost.New", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := o.Run(ctx); err != nil {
		log.Error("outpost.Run", "err", err)
		os.Exit(1)
	}
	log.Info("outpost stopped")
}
