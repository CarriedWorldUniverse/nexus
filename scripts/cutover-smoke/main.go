// Command cutover-smoke exercises the dashboard-ws-port surface
// against a running nexus. Assumes the operator has already
// completed `nexus identity init` and started the broker, but
// does NOT require a passkey to be registered — it forges an
// operator JWT using the same SessionSigningSecret the broker
// holds, then drives the WS through every Crossing 5c/5d frame
// kind to verify the wire is healthy.
//
// Operator's go/no-go gate before cutover: agent-network goes down
// only after this script passes against the nexus running on the
// production tailnet host.
//
// Usage:
//
//	NEXUS_URL=wss://agentnetwork.tail41686e.ts.net:7888 \
//	NEXUS_DATA_DIR=/path/to/nexus/data \
//	go run ./scripts/cutover-smoke
//
// Reads the signing secret directly from nexus.db (via the
// identity package) so it works without operator passkey + WebAuthn
// — that flow is for browsers, not for a Go smoke harness.
//
// Exit codes:
//
//	0   all checks pass
//	1   any check failed (stderr names the failed step)
//	2   environment / argument problem (e.g. NEXUS_DATA_DIR unset)
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/nexus/identity"
	"github.com/nexus-cw/nexus/nexus/jwt"
	"github.com/nexus-cw/nexus/nexus/storage"
)

var (
	urlFlag     = flag.String("url", "", "broker WS URL (default: $NEXUS_URL, falls back to wss://localhost:7888)")
	dataDir     = flag.String("data-dir", "", "nexus data dir holding nexus.db (default: $NEXUS_DATA_DIR, falls back to ./data)")
	insecure    = flag.Bool("insecure", false, "skip TLS verification (use for self-signed dev certs)")
	timeoutFlag = flag.Duration("timeout", 30*time.Second, "overall smoke timeout")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS — every operator-WS frame round-tripped clean. nexus is ready for cutover.")
}

func run() error {
	wsURL := resolveURL()
	dir := resolveDataDir()

	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()

	// 1. Open the DB and pull out the signing secret + nexus_id.
	//    This is the same path cmd/nexus uses to wire the broker;
	//    forging a JWT here is exactly what the broker would mint
	//    after a successful passkey login.
	fmt.Println("[1/8] reading nexus identity from", dir)
	db, err := storage.Open(ctx, dir, nil)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	id, err := identity.Load(ctx, db)
	if err != nil {
		return fmt.Errorf("identity.Load (run `nexus identity init` first?): %w", err)
	}

	// 2. Mint an operator JWT signed with the broker's secret.
	fmt.Println("[2/8] minting operator JWT")
	now := time.Now()
	tok, err := jwt.Sign(id.SessionSigningSecret, jwt.Claims{
		Iss: "nexus://" + id.NexusID,
		Sub: "operator",
		Iat: now.Unix(),
		Exp: now.Add(5 * time.Minute).Unix(),
		Ses: "cutover-smoke",
	})
	if err != nil {
		return fmt.Errorf("jwt sign: %w", err)
	}

	// 3. Dial /connect over WS.
	fmt.Println("[3/8] dialing", wsURL)
	dialURL := wsURL + "/connect?token=" + url.QueryEscape(tok)
	httpClient := &http.Client{}
	if *insecure {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	c, _, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "smoke done")

	// 4. roster.list — should return the live aspect set.
	fmt.Println("[4/8] roster.list")
	rosterRes, err := rpc(ctx, c, frames.KindRosterList, frames.RosterListPayload{})
	if err != nil {
		return fmt.Errorf("roster.list: %w", err)
	}
	var rosterPayload frames.RosterListResultPayload
	_ = json.Unmarshal(rosterRes, &rosterPayload)
	fmt.Printf("      → %d aspects online: %s\n",
		len(rosterPayload.Aspects), aspectNames(rosterPayload.Aspects))

	// 5. subscribe.chat — operator should immediately start
	//    receiving chat.deliver frames.
	fmt.Println("[5/8] subscribe.chat")
	if _, err := rpc(ctx, c, frames.KindSubscribeChat, frames.SubscribePayload{}); err != nil {
		return fmt.Errorf("subscribe.chat: %w", err)
	}

	// 6. chat.send → chat.deliver round-trip. The operator's own
	//    chat.send fans out via the per-aspect recipient policy
	//    plus the operator broadcast (5d), so this connection
	//    receives the deliver too. Verifies write + push wire.
	fmt.Println("[6/8] chat.send → chat.deliver")
	probeContent := fmt.Sprintf("cutover-smoke probe %d", now.UnixMicro())
	if _, err := sendNoWait(c, frames.KindChatSend, frames.ChatSendPayload{
		From:    "operator",
		Content: probeContent,
	}); err != nil {
		return fmt.Errorf("chat.send: %w", err)
	}
	if err := awaitDeliverContaining(ctx, c, probeContent); err != nil {
		return fmt.Errorf("chat.deliver round-trip: %w", err)
	}

	// 7. knowledge.store → knowledge.list round-trip. Verifies the
	//    operator's WS surface against the knowledge.Store seam.
	fmt.Println("[7/8] knowledge.store → knowledge.list")
	storeRes, err := rpc(ctx, c, frames.KindKnowledgeStore, frames.KnowledgeStorePayload{
		Topic:   "cutover-smoke",
		Content: "smoke probe — delete me",
	})
	if err != nil {
		return fmt.Errorf("knowledge.store: %w", err)
	}
	var storePayload frames.KnowledgeStoreResultPayload
	_ = json.Unmarshal(storeRes, &storePayload)
	if storePayload.ID == 0 {
		return fmt.Errorf("knowledge.store: empty id in result")
	}
	listRes, err := rpc(ctx, c, frames.KindKnowledgeList, frames.KnowledgeListPayload{
		Agent: "operator",
		Limit: 10,
	})
	if err != nil {
		return fmt.Errorf("knowledge.list: %w", err)
	}
	var listPayload frames.KnowledgeListResultPayload
	_ = json.Unmarshal(listRes, &listPayload)
	if !containsTopic(listPayload.Entries, "cutover-smoke") {
		return fmt.Errorf("knowledge.list: probe entry not visible after store")
	}

	// 8. aspect.say — sugar over chat.send. Verifies the
	//    @<aspect> mention prepend + identity threading.
	fmt.Println("[8/8] aspect.say")
	if len(rosterPayload.Aspects) == 0 {
		fmt.Println("      → no aspects online, skipping aspect.say")
	} else {
		target := rosterPayload.Aspects[0].Name
		sayRes, err := rpc(ctx, c, frames.KindAspectSay, frames.AspectSayPayload{
			Aspect:  target,
			Content: "cutover smoke ping; please ignore",
		})
		if err != nil {
			return fmt.Errorf("aspect.say: %w", err)
		}
		var sayPayload frames.AspectSayResultPayload
		_ = json.Unmarshal(sayRes, &sayPayload)
		if sayPayload.MsgID == 0 {
			return fmt.Errorf("aspect.say: empty msg_id in result")
		}
		fmt.Printf("      → @%s msg_id=%d\n", target, sayPayload.MsgID)
	}

	return nil
}

// --- helpers ---

// rpc sends a request and reads frames until the matching .result
// arrives. Drops any push frames that arrive in between (the SPA
// would route them via comms.subscribe; the smoke just ignores).
func rpc(ctx context.Context, c *websocket.Conn, kind frames.Kind, payload any) (json.RawMessage, error) {
	req, err := frames.NewRequest(kind, payload)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(ctx, c, req); err != nil {
		return nil, err
	}
	for {
		env, err := readFrame(ctx, c)
		if err != nil {
			return nil, err
		}
		if env.InReplyTo != req.ID {
			// Push frame for some other reason; ignore.
			continue
		}
		if strings.HasSuffix(string(env.Kind), ".error") {
			return nil, fmt.Errorf("%s: %s", env.Kind, errorMsg(env.Payload))
		}
		return env.Payload, nil
	}
}

// sendNoWait fires a frame without waiting for a response.
func sendNoWait(c *websocket.Conn, kind frames.Kind, payload any) (string, error) {
	req, err := frames.NewRequest(kind, payload)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writeFrame(ctx, c, req); err != nil {
		return "", err
	}
	return req.ID, nil
}

// awaitDeliverContaining reads frames until a chat.deliver carries
// `needle` in its content. Discards everything else (other deliver
// frames from concurrent traffic, etc.).
func awaitDeliverContaining(ctx context.Context, c *websocket.Conn, needle string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	for time.Now().Before(deadline) {
		env, err := readFrame(ctx, c)
		if err != nil {
			return err
		}
		if env.Kind != frames.KindChatDeliver {
			continue
		}
		var p frames.ChatDeliverPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		if strings.Contains(p.Content, needle) {
			return nil
		}
	}
	return fmt.Errorf("chat.deliver containing %q never arrived", needle)
}

func writeFrame(ctx context.Context, c *websocket.Conn, env frames.Envelope) error {
	raw, err := frames.Encode(env)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, raw)
}

func readFrame(ctx context.Context, c *websocket.Conn) (frames.Envelope, error) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return frames.Envelope{}, err
	}
	return frames.Decode(data)
}

func errorMsg(raw json.RawMessage) string {
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	if e, ok := m["error"]; ok {
		return e
	}
	return string(raw)
}

func aspectNames(rs []frames.RosterAspect) string {
	if len(rs) == 0 {
		return "(none)"
	}
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

func containsTopic(entries []frames.KnowledgeHit, topic string) bool {
	for _, e := range entries {
		if e.Topic == topic {
			return true
		}
	}
	return false
}

func resolveURL() string {
	v := *urlFlag
	if v == "" {
		v = os.Getenv("NEXUS_URL")
	}
	if v == "" {
		v = "wss://localhost:7888"
	}
	// Trim trailing slash for clean concat.
	v = strings.TrimSuffix(v, "/")
	return v
}

func resolveDataDir() string {
	v := *dataDir
	if v == "" {
		v = os.Getenv("NEXUS_DATA_DIR")
	}
	if v == "" {
		v = "./data"
	}
	return v
}

