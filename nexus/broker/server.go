// Package broker serves the Nexus WS and HTTP surface. Per transport
// spec v0.1 §10, the bulk of inter-component traffic runs over the
// WS endpoint at /connect (see ws.go). This file keeps the HTTP bits
// that remain legitimately HTTP: /health (external monitoring) and
// /api/aspects (dashboard convenience — authoritative roster state
// is the WS-driven in-memory map).
//
// Business logic lives in nexus/roster; this package is transport.
package broker

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/handqueue"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/sessions"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/handqueue"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/sessions"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// chatHTML is the operator-aspect smoke-test page (chat.html). Served
// at GET /chat.html so a browser on any tailnet peer can drive the
// nexus over WS without a separate static-file server. Single-file,
// no build step. Source lives at nexus/broker/static/chat.html.
//
//go:embed static/chat.html
var chatHTML []byte

// Config configures a Broker.
type Config struct {
	Addr string // host:port, e.g. ":7888"

	// AuthToken is the LEGACY shared bearer token. Pre-drift-C, every
	// caller used this single token and the broker trusted whoever
	// presented it. Post-drift-C the broker resolves per-aspect tokens
	// via the TokenStore (hand-dispatch v0.1 §5.3, §5.4); AuthToken is
	// retained as a back-compat shim — when set, presenting it
	// resolves to the Frame identity (admin=true). Leave empty in new
	// deployments; populate per-aspect tokens via TokenStore instead.
	AuthToken string

	// Tokens carries the per-aspect bearer tokens and admin flags
	// resolved from agent_tokens. Required for per-aspect identity
	// resolution; if nil, the broker constructs an empty store and
	// only the legacy AuthToken (if set) will authenticate. The
	// caller (cmd/nexus) is responsible for calling
	// ReconcileAgentTokens / ReconcileFrameToken before
	// ListenAndServe.
	Tokens *TokenStore

	HeartbeatIntervalS int           // value returned to aspects on register
	StaleAfter         time.Duration // aspect becomes "stale" after this gap
	Logger             *slog.Logger

	// Projection receives session.entry.appended frames from aspects.
	// Optional — if nil, the broker logs and drops session-projection
	// frames instead of persisting (useful for tests that don't need
	// a DB).
	Projection *sessions.Projection

	// HandQueue dispatches `dispatch` frames. Optional — if nil,
	// the broker responds with a dispatch.error indicating no
	// dispatcher configured. (Field name is legacy; the queue
	// implements the generic dispatch protocol.)
	HandQueue *handqueue.Queue

	// Admin wires the embedded Frame's admin-action callbacks (#79
	// lock — REST-only admin surface). When nil, the /api/admin/*
	// endpoints are not registered. P5 supplies these from the
	// EmbeddedFrame; pre-§6.5 deployments without a Frame leave Admin
	// nil and lose the admin surface (correct — no Frame = no admin).
	Admin *AdminCallbacks

	// ChatRouter routes chat.send frames to the embedded Frame's
	// deliberation funnel (§6.5 P6). When nil, chat.send frames are
	// logged as "not yet handled" — same behaviour as before P6.
	// Only chat.send is routed here; chat.deliver and other comms
	// frames are handled by the aspect WS path.
	ChatRouter *ChatRouterCallbacks

	// Replayer drives Lock 6 reconnect/replay. When an aspect registers
	// with since_msg_id > 0, the broker queries chat history for
	// messages addressed to that aspect since the cursor and emits each
	// as a chat.deliver frame with Replay=true before resuming live
	// delivery. Optional: when nil, since_msg_id is ignored and aspects
	// only receive live frames going forward (Lock 6's "graceful
	// degradation" path — no replay, but no crash).
	Replayer *Replayer

	// ChatStore powers chat.read frames (Lock 2 pull path). Aspects
	// invoke chat.read to fetch thread context they weren't pushed,
	// without triggering a fresh deliberation cycle. When nil, chat.read
	// frames return an empty result with an error string.
	ChatStore chat.Store

	// RecipientPolicy decides which aspects receive chat.deliver for
	// each chat.send. When non-nil, HandleChatSend uses it to fan out
	// after persistence. When nil, only persistence + the legacy
	// ChatRouter callback fire (live aspects don't see cross-aspect
	// chats; Lock 6 replay still works on register).
	RecipientPolicy *RecipientPolicy

	// AspectHomes maps registered aspect-name → canonical filesystem
	// home, populated at startup from the autospawn discovery scan
	// over one or more aspect-dir roots. The broker uses this as the
	// source of truth for "where does aspect X live" — payload.Home
	// from the register frame is IGNORED in favour of this map (#21).
	// Closes the cmd.Dir control vector: an attacker who steals an
	// aspect token can't repoint the worker's working directory by
	// register payload.
	//
	// When nil OR when an aspect's name isn't in the map, the broker
	// falls back to payload.Home (legacy behaviour) but logs a
	// warning. New deployments should configure --aspect-dir so the
	// scan populates this map.
	AspectHomes map[string]string

	// MaxConnections caps the total number of concurrently-accepted
	// /connect upgrades. Pre-#25 the broker accepted unbounded
	// connections; an attacker (even unauthenticated, per the
	// pre-401-delay path) could exhaust file descriptors / goroutines
	// by opening connections faster than they were closed. Default
	// from defaultMaxConnections when zero.
	MaxConnections int

	// MaxConnectionsPerIP caps per source-IP concurrent connections.
	// Without this, one misbehaving (or attacker-controlled) host
	// can consume the global cap and lock out legitimate aspects.
	// Default from defaultMaxConnectionsPerIP when zero.
	MaxConnectionsPerIP int

	// MaxConsecutiveBadFrames is the threshold for the per-connection
	// bad-frame counter (#34). After this many consecutive decode
	// failures the connection is closed. The counter resets on every
	// successful decode. Default from defaultMaxConsecutiveBadFrames
	// when zero.
	MaxConsecutiveBadFrames int

	// AllowLegacyMaster opts in to the back-compat fallback that
	// promotes AuthToken to a Frame-identity master token. Default
	// false: legacy auth is disabled and aspects must present their
	// per-aspect bearer (or the broker rejects). Operators set this
	// during the per-aspect-token migration; once all aspects have
	// rotated, leave it false. Cmd wrapper reads NEXUS_ALLOW_LEGACY_MASTER
	// env into this field.
	AllowLegacyMaster bool

	// TLSCertFile / TLSKeyFile point at the PEM-encoded server cert
	// and key used by ListenAndServe. Required — the broker has no
	// plain-HTTP path. Operator runs `nexus cert init` once per host
	// to provision these (see PR-A2.1). Operator decision (#9667):
	// always enforce certificate and TLS use; no exceptions.
	TLSCertFile string
	TLSKeyFile  string

	// OriginPatterns is the WebSocket Origin allowlist for /connect
	// upgrades. Browser-based callers (dashboard SPA, future UI agents)
	// send an Origin header; non-browser aspects (Go ws clients) do
	// not. The broker treats UI surfaces the same as any other aspect:
	// they authenticate via per-aspect bearer token, and their Origin
	// must match this list.
	//
	// Empty list = no browser origins accepted. Non-browser aspects
	// (no Origin header) connect regardless. This is the v1 default
	// since the dashboard reaches the broker via REST today; once a
	// browser-side WS client lands, its origin is added here.
	//
	// Patterns follow nhooyr.io/websocket's matching: literal host
	// matches (e.g. "https://localhost:7888") or wildcard subdomain
	// patterns. See websocket.AcceptOptions.OriginPatterns.
	OriginPatterns []string

	// KeyfileValidator wires the spec §5 keyfile-auth endpoints
	// (GET /api/nexus_id + POST /api/aspect/validate). cmd/nexus
	// builds this from the loaded identity + an aspects.SQLStore. When
	// nil, the endpoints are not registered (legacy boot mode without
	// keyfile auth — Part 5+ will tighten this).
	KeyfileValidator *KeyfileValidator

	// OnPersonalityChange is invoked after a successful personality
	// edit (CLI Part 7a or REST Part 7b). cmd/nexus wires this to
	// EmbeddedFrame.RefreshPersonality so the in-process Frame picks
	// up the change on its next deliberation turn (per spec §11
	// in-process refresh callback).
	//
	// Per spec §6: a separate WS frame `personality.refresh` should
	// also broadcast to remote agentfunnels. Deferred — for v0.1,
	// remote aspects pick up at next JWT re-validation (1h TTL).
	// When a future broker grows the broadcast, it lands here too.
	//
	// nil callback is a no-op (legacy boot path, or Frame not yet
	// embedded).
	OnPersonalityChange func(aspectName string, newVersion int64)

	// OperatorLogin wires the dashboard-ws-port login + register
	// endpoints (POST /api/operator/{register,login}/{begin,finish}).
	// nil → endpoints not registered (legacy boot, no dashboard SPA).
	OperatorLogin *OperatorLogin

	// KnowledgeStore powers operator-facing knowledge frames
	// (knowledge.list / knowledge.search / knowledge.store) on the WS
	// surface. nil → those frames return an "<kind>.error"
	// "knowledge store not configured" so the SPA renders a clean
	// "feature not available" instead of a hung Promise.
	KnowledgeStore *knowledge.Store

	// OnNexusMDChange is invoked after a successful central nexus_md
	// edit (REST Part 9c via PUT /api/admin/nexus-md). cmd/nexus wires
	// this to EmbeddedFrame.RefreshCentral so the in-process Frame
	// picks up the change on the next turn (Part 9b's SystemPromptFn
	// callback path).
	//
	// Network-wide change: every live aspect's composed prompt
	// includes central content, so the future WS broadcast will land
	// here too (Part 9d). nil callback is a no-op.
	OnNexusMDChange func(newVersion int64)
}

// ChatRouterCallbacks wires the broker's chat.send handling to the
// Frame funnel. A nil RouteChat is treated as "no router" — the
// broker logs and drops chat.send frames.
type ChatRouterCallbacks struct {
	// RouteChat is called for every chat.send frame the broker receives.
	// It runs in a goroutine; the broker does not block on it. Errors
	// are logged; the caller can't surface them to the sender (WS chat
	// send is fire-and-forget per the transport spec).
	RouteChat func(ctx context.Context, msgID int64, from, content string, replyTo int64, topic string)
}

// Broker owns the HTTP server and its roster.
// Default DoS-resistance knobs. Generous defaults for the v1
// deployment shape (small aspect roster, single Nexus host); operators
// in larger or hostile-adjacent deployments tune via Config.
const (
	defaultMaxConnections          = 256
	defaultMaxConnectionsPerIP     = 32
	defaultMaxConsecutiveBadFrames = 16
)

type Broker struct {
	cfg    Config
	roster *roster.Roster
	srv    *http.Server
	log    *slog.Logger

	// Connection accounting for #25. connMu guards both fields.
	// connTotal is the current count of accepted /connect upgrades;
	// connPerIP[host] is the per-source-IP count (host:port stripped
	// to host before lookup).
	connMu    sync.Mutex
	connTotal int
	connPerIP map[string]int

	// ctx drives the lifetime of WS goroutines. Set in ListenAndServe
	// from the caller's context; cancelled when ListenAndServe returns
	// so detached WS serve-goroutines tear down during graceful
	// shutdown (not just when the OS drops the TCP connection).
	ctx       context.Context
	ctxCancel context.CancelFunc

	// operators tracks live operator WS connections (dashboard SPA
	// sessions). Distinct from `dispatcher` (per-aspect-name → conn);
	// operator conns aren't named in the roster — they're registered
	// here at handleConnect time when c.auth.Operator is true and
	// removed in cleanup. Used by 5d's subscription fan-out to push
	// chat.deliver / roster.update / aspect.status_pulse frames to
	// every subscribing operator without naming them individually.
	//
	// opMu guards operators. Range under read-lock during fan-out
	// (write paths run in WS-handler goroutines and bind/unbind on
	// connect/cleanup; both rare relative to fan-out).
	opMu      sync.RWMutex
	operators map[*wsConn]struct{}

	// dispatcher is the server-side request/response API: tracks
	// which wsConn holds each aspect name, and delivers correlated
	// response frames. Used by SendTurn (and later SendHand etc).
	dispatcher *Dispatcher

	// adminOps tracks in-flight long-running admin operations
	// (shutdown/compact/rewind). Lazily allocated by registerAdmin.
	adminOps *adminOpStore
}

func New(cfg Config, r *roster.Roster) *Broker {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HeartbeatIntervalS == 0 {
		cfg.HeartbeatIntervalS = 10
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = 30 * time.Second
	}
	// Always have a usable TokenStore. If the caller didn't provide
	// one (older test paths), construct an empty store. Legacy
	// AuthToken — when set AND opt-in flag is on — is registered as
	// the master-fallback so pre-drift-C tests and callers continue
	// to authenticate as the Frame identity (admin=true) without
	// per-aspect minting. Per #31 / operator A/A/A, the auto-promote
	// is off by default; operators opt in via AllowLegacyMaster (or
	// NEXUS_ALLOW_LEGACY_MASTER=1 in the cmd wrapper).
	if cfg.Tokens == nil {
		cfg.Tokens = NewTokenStore()
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = defaultMaxConnections
	}
	if cfg.MaxConnectionsPerIP == 0 {
		cfg.MaxConnectionsPerIP = defaultMaxConnectionsPerIP
	}
	if cfg.MaxConsecutiveBadFrames == 0 {
		cfg.MaxConsecutiveBadFrames = defaultMaxConsecutiveBadFrames
	}
	if cfg.AuthToken != "" && cfg.AllowLegacyMaster {
		cfg.Tokens.SetLegacyMaster(cfg.AuthToken)
		cfg.Logger.Warn("legacy master token enabled — every /connect via this token will WARN. " +
			"Migrate aspects to per-aspect tokens; clear NEXUS_ALLOW_LEGACY_MASTER once done.")
	}
	return &Broker{
		cfg:        cfg,
		roster:     r,
		log:        cfg.Logger,
		dispatcher: newDispatcher(),
		connPerIP:  make(map[string]int),
		operators:  make(map[*wsConn]struct{}),
	}
}

// reserveConn accounts a new /connect against the global + per-IP
// caps. Returns (true, host) on success — caller must call releaseConn
// with the returned host on disconnect. Returns (false, "") if either
// cap is reached; caller should reject with 503.
func (b *Broker) reserveConn(remoteAddr string) (bool, string) {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	// SplitHostPort failure (or empty remoteAddr in test wiring)
	// leaves host equal to the raw input. The global cap still
	// protects under malformed input; the per-IP map may accumulate
	// a phantom key for that exact malformed string but it's bounded
	// by the global cap and cleaned up via releaseConn on disconnect.
	b.connMu.Lock()
	defer b.connMu.Unlock()
	if b.connTotal >= b.cfg.MaxConnections {
		return false, ""
	}
	if b.connPerIP[host] >= b.cfg.MaxConnectionsPerIP {
		return false, ""
	}
	b.connTotal++
	b.connPerIP[host]++
	return true, host
}

// releaseConn decrements the connection accounting after a /connect
// disconnects. host is the value returned by reserveConn; if empty,
// this is a no-op (paired with a failed reserve).
func (b *Broker) releaseConn(host string) {
	if host == "" {
		return
	}
	b.connMu.Lock()
	defer b.connMu.Unlock()
	if b.connTotal > 0 {
		b.connTotal--
	}
	if b.connPerIP[host] > 0 {
		b.connPerIP[host]--
		if b.connPerIP[host] == 0 {
			delete(b.connPerIP, host)
		}
	}
}

// ListenAndServe blocks serving the broker until the context is cancelled.
// TLS-always: requires cfg.TLSCertFile + cfg.TLSKeyFile. There is no
// plain-HTTP listener. Operator decision (#9667).
func (b *Broker) ListenAndServe(ctx context.Context) error {
	if b.cfg.TLSCertFile == "" || b.cfg.TLSKeyFile == "" {
		return errors.New("broker: TLSCertFile and TLSKeyFile required " +
			"(run `nexus cert init` to provision, then pass --tls-cert / --tls-key)")
	}
	b.ctx, b.ctxCancel = context.WithCancel(ctx)
	defer b.ctxCancel()

	mux := http.NewServeMux()
	// WS surface per transport spec v0.1 — see ws.go. Auth is checked
	// inside handleConnect before upgrade so bad tokens get clean 401s.
	mux.HandleFunc("GET /connect", b.handleConnect)
	// HTTP surface that stays per spec §10: dashboard convenience +
	// external monitoring.
	mux.Handle("GET /api/aspects", b.auth(http.HandlerFunc(b.handleList)))
	mux.HandleFunc("GET /health", b.handleHealth)
	// Operator-aspect chat UI — single-page smoke-test client. Served
	// at /chat.html for direct browser access. Token + URL fields are
	// inputs in the page itself; no server-side state needed.
	mux.HandleFunc("GET /chat.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(chatHTML)
	})

	// Keyfile auth endpoints (spec §5 — Part 4b). Registered only
	// when KeyfileValidator is configured. Both routes deliberately
	// bypass auth(): the keyfile is its own credential and the
	// nexus_id endpoint is meant to be queried before any.
	b.registerKeyfileEndpoints(mux)

	// Operator login (dashboard-ws-port spec §2.2). Bypasses auth()
	// the same way the keyfile endpoints do — the passkey ceremony
	// is the credential. Registered only when the embedding caller
	// (cmd/nexus) supplies an OperatorLogin.
	if b.cfg.OperatorLogin != nil {
		b.cfg.OperatorLogin.register(mux)
	}

	// Admin REST surface (#79 lock). Registered only when a Frame is
	// embedded and supplies AdminCallbacks. Per spec §3.3, admin ops
	// belong to the Frame because the Frame IS the Nexus.
	b.registerAdmin(mux)

	b.srv = &http.Server{
		Addr:              b.cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		b.log.Info("broker listening", "addr", b.cfg.Addr)
		if err := b.srv.ListenAndServeTLS(b.cfg.TLSCertFile, b.cfg.TLSKeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Stop the HTTP listener AND drain the dispatch queue (#40).
		// Note: srv.Shutdown does NOT track hijacked WS connections,
		// so it returns fast for the WS handlers — they're invisible
		// to the http.Server once the upgrade succeeds. The actual
		// drain work happens in HandQueue.Shutdown, which signals
		// in-flight workers to exit; their Submit callers (still
		// running inside hijacked WS handler goroutines) get
		// ErrQueueShutdown and propagate to clients. Pre-#40 fix the
		// queue Shutdown was never called, so workers ran to
		// completion regardless of broker shutdown.
		shutdownErr := b.srv.Shutdown(shutdownCtx)
		if b.cfg.HandQueue != nil {
			if qerr := b.cfg.HandQueue.Shutdown(shutdownCtx); qerr != nil && shutdownErr == nil {
				shutdownErr = qerr
			}
		}
		return shutdownErr
	case err := <-errCh:
		return err
	}
}

// auth rejects any request whose bearer token does not resolve to a
// known identity. The resolved TokenInfo is stashed on the request
// context so handlers downstream can read it via authUserFromContext.
// Health is left unauthenticated so process supervisors can poll it.
func (b *Broker) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ExtractBearer(r.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		info, ok := b.cfg.Tokens.ResolveToken(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r.WithContext(withAuthUser(r.Context(), info)))
	})
}

// authUserCtxKey is the unexported context key under which the
// resolved TokenInfo is stored. Exported only via withAuthUser /
// AuthUserFromContext helpers below.
type authUserCtxKey struct{}

// withAuthUser returns a copy of ctx carrying the TokenInfo.
func withAuthUser(ctx context.Context, info TokenInfo) context.Context {
	return context.WithValue(ctx, authUserCtxKey{}, info)
}

// AuthUserFromContext extracts the TokenInfo a previous auth pass
// installed on the request context. Returns (zero, false) if absent.
// Drift D's override handlers will use this to gate admin-only ops.
func AuthUserFromContext(ctx context.Context) (TokenInfo, bool) {
	v, ok := ctx.Value(authUserCtxKey{}).(TokenInfo)
	return v, ok
}


func (b *Broker) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"aspects": b.roster.List(),
	})
}

func (b *Broker) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func validateRegister(req *schemas.RegisterRequest) error {
	if req.Name == "" {
		return errors.New("name required")
	}
	if req.SessionID == "" {
		return errors.New("session_id required")
	}
	switch req.ContextMode {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		return errors.New("context_mode must be one of: global, thread, stateless")
	}
	if req.Provider == "" {
		return errors.New("provider required")
	}
	// Port used to be required (HTTP-era: broker needed it to route
	// back to the aspect). Under the WS transport, aspects dial out
	// and have no inbound listener, so port is advisory metadata
	// only. Validated for range if provided.
	if req.Port < 0 || req.Port > 65535 {
		return errors.New("port must be 0–65535 (0 means no inbound listener)")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
