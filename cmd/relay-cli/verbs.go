package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	casket "github.com/nexus-cw/casket-go"
	"github.com/nexus-cw/nexus/relay"
)

// runDiscover fetches /.well-known/nexus-interchange and prints it.
func runDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	_ = fs.Parse(args)
	if *relayURL == "" {
		return errors.New("discover: -relay required")
	}
	c := &relay.Client{
		BaseURL: strings.TrimRight(*relayURL, "/"),
		HTTP:    insecureHTTP(),
	}
	doc, err := c.Discover(ctx)
	if err != nil {
		return err
	}
	// Pretty-print: doc is already JSON, re-indent for readability.
	var v any
	if err := json.Unmarshal(doc, &v); err == nil {
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	fmt.Println(string(doc))
	return nil
}

// runHealth hits GET /health.
func runHealth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	_ = fs.Parse(args)
	if *relayURL == "" {
		return errors.New("health: -relay required")
	}
	url := strings.TrimRight(*relayURL, "/") + "/health"
	resp, err := insecureHTTP().Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("health: HTTP %d: %s", resp.StatusCode, body)
	}
	fmt.Println(string(body))
	return nil
}

// runStatus polls a single pair-request status. Doesn't loop — one shot.
func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("status: exactly one positional <request-id> required")
	}
	if *relayURL == "" {
		return errors.New("status: -relay required")
	}
	requestID := fs.Arg(0)
	c := &relay.Client{
		BaseURL: strings.TrimRight(*relayURL, "/"),
		HTTP:    insecureHTTP(),
	}
	// PollRequest loops until non-pending; for one-shot we need the
	// underlying status fetch. The relay package only exposes
	// PollRequest publicly, so use a tight context to bound the wait.
	one, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	res, err := c.PollRequest(one, requestID)
	if errors.Is(err, context.DeadlineExceeded) {
		// Pending: spit out a minimal record so callers see the state.
		fmt.Println(`{"request_id":"` + requestID + `","status":"pending"}`)
		return nil
	}
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return nil
}

// runPairWithToken consumes a peer's PairingToken (JSON) and instantiates
// a local PairedChannel, persisting to the casket storage backend.
//
// The peer's token is exchanged out-of-band (operator-to-operator) AFTER
// the pair-request has been approved on the relay. Without this step,
// send/recv have nothing to encrypt against.
func runPairWithToken(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair-with-token", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	asID := fs.String("as", "", "our nexus-id (defaults to single id under -store)")
	tokenFile := fs.String("file", "", "read token JSON from file (use - for stdin); else pass as positional arg")
	maxAgeSec := fs.Int64("max-age", 600, "reject tokens older than this many seconds")
	_ = fs.Parse(args)

	var raw []byte
	if *tokenFile != "" {
		var err error
		if *tokenFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(*tokenFile)
		}
		if err != nil {
			return fmt.Errorf("pair-with-token: read token: %w", err)
		}
	} else {
		if fs.NArg() != 1 {
			return errors.New("pair-with-token: provide token JSON as positional arg or use -file")
		}
		raw = []byte(fs.Arg(0))
	}

	var tok casket.PairingToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("pair-with-token: parse token: %w", err)
	}

	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	paired, err := ch.Pair(ctx, tok, *maxAgeSec)
	if err != nil {
		return fmt.Errorf("pair-with-token: %w", err)
	}
	fmt.Printf("paired with %s\n  path_id : %s\n  endpoint: %s\n",
		paired.PeerID(), paired.PathID(), paired.PeerEndpoint())
	return nil
}

// runSend encrypts a message and PUTs it on the pair's mailbox.
func runSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	asID := fs.String("as", "", "our nexus-id (defaults to single id under -store)")
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	kind := fs.String("kind", "announce", "message kind: proposal/question/reply/accept/reject/announce")
	contentType := fs.String("content-type", "text/markdown", "MIME type of body")
	inReplyTo := fs.String("in-reply-to", "", "msg_id of message this replies to (optional)")
	bodyFile := fs.String("file", "", "read body from file (use - for stdin); else pass as positional arg after <peer-id>")
	_ = fs.Parse(args)

	if *relayURL == "" {
		return errors.New("send: -relay required")
	}
	if fs.NArg() < 1 {
		return errors.New("send: <peer-id> positional arg required (and either -file or text body)")
	}
	peerID := fs.Arg(0)

	var bodyText string
	if *bodyFile != "" {
		var raw []byte
		var err error
		if *bodyFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(*bodyFile)
		}
		if err != nil {
			return fmt.Errorf("send: read body: %w", err)
		}
		bodyText = string(raw)
	} else {
		if fs.NArg() < 2 {
			return errors.New("send: provide body as positional arg after <peer-id> or use -file")
		}
		bodyText = strings.Join(fs.Args()[1:], " ")
	}

	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	paired, err := ch.GetPaired(ctx, peerID)
	if err != nil {
		return fmt.Errorf("send: load pair %s: %w", peerID, err)
	}
	if paired == nil {
		return fmt.Errorf("send: not paired with %s — run pair-with-token first", peerID)
	}

	inner := relay.InnerEnvelope{
		OriginNexus: id,
		DestNexus:   peerID,
		Kind:        *kind,
		InReplyTo:   *inReplyTo,
		ContentType: *contentType,
		Body:        bodyText,
	}
	if err := inner.Validate(0); err != nil {
		return fmt.Errorf("send: invalid inner: %w", err)
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return err
	}

	// Generate msg_id BEFORE encrypting — AAD = path_id || msg_id, so the
	// msg_id must be fixed at AEAD time. Reordering this is the only
	// sensitive part of the v1→AAD-binding migration.
	msgID, err := uuidv7()
	if err != nil {
		return fmt.Errorf("send: msg_id: %w", err)
	}
	aad := relay.MakeAAD(paired.PathID(), msgID)
	ct, err := paired.EncryptBody(innerJSON, aad)
	if err != nil {
		return fmt.Errorf("send: encrypt: %w", err)
	}
	env := relay.BuildOuter(paired.PathID(), msgID, relay.IsoTs(time.Now().UTC()), ct)

	c := &relay.Client{BaseURL: strings.TrimRight(*relayURL, "/"), HTTP: insecureHTTP()}
	if err := c.Put(ctx, paired, env); err != nil {
		return fmt.Errorf("send: put: %w", err)
	}
	fmt.Printf("sent\n  msg_id  : %s\n  path_id : %s\n  bytes   : %d\n",
		msgID, paired.PathID(), len(ct))
	return nil
}

// runRecv pulls envelopes for a peer, decrypts each, and prints them.
// Auto-acks unless -no-ack.
func runRecv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recv", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	asID := fs.String("as", "", "our nexus-id (defaults to single id under -store)")
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	since := fs.String("since", "", "msg_id cursor — pull envelopes newer than this")
	noAck := fs.Bool("no-ack", false, "do not ack consumed envelopes (useful for re-reading)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of human format")
	_ = fs.Parse(args)
	if *relayURL == "" {
		return errors.New("recv: -relay required")
	}
	if fs.NArg() != 1 {
		return errors.New("recv: <peer-id> positional arg required")
	}
	peerID := fs.Arg(0)

	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	paired, err := ch.GetPaired(ctx, peerID)
	if err != nil {
		return fmt.Errorf("recv: load pair %s: %w", peerID, err)
	}
	if paired == nil {
		return fmt.Errorf("recv: not paired with %s — run pair-with-token first", peerID)
	}

	c := &relay.Client{BaseURL: strings.TrimRight(*relayURL, "/"), HTTP: insecureHTTP()}
	resp, err := c.Get(ctx, paired, paired.PathID(), *since)
	if err != nil {
		return fmt.Errorf("recv: get: %w", err)
	}
	if len(resp.Envelopes) == 0 {
		if *jsonOut {
			fmt.Println(`{"envelopes":[]}`)
		} else {
			fmt.Println("(no new messages)")
		}
		return nil
	}

	type out struct {
		MsgID string                 `json:"msg_id"`
		Inner relay.InnerEnvelope    `json:"inner"`
	}
	var decoded []out
	var ackIDs []string

	for _, raw := range resp.Envelopes {
		var env relay.OuterEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			fmt.Fprintf(os.Stderr, "recv: skip malformed envelope: %v\n", err)
			continue
		}
		ctBytes, err := decodeCiphertext(env.Ciphertext)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recv: skip %s: ciphertext decode: %v\n", env.MsgID, err)
			continue
		}
		// AAD = path_id || msg_id (UTF-8 bytes, no separator) per spec.
		// The path_id is fixed per pair; msg_id is per-envelope. Both are
		// in the outer envelope so we can compute AAD independently of
		// whether dedupe/seen-cache says we've processed before.
		aad := relay.MakeAAD(env.PathID, env.MsgID)
		plaintext, err := paired.DecryptBody(ctBytes, aad)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recv: skip %s: decrypt: %v\n", env.MsgID, err)
			continue
		}
		var inner relay.InnerEnvelope
		if err := json.Unmarshal(plaintext, &inner); err != nil {
			fmt.Fprintf(os.Stderr, "recv: skip %s: inner parse: %v\n", env.MsgID, err)
			continue
		}
		if err := inner.Validate(0); err != nil {
			fmt.Fprintf(os.Stderr, "recv: skip %s: invalid inner: %v\n", env.MsgID, err)
			continue
		}
		decoded = append(decoded, out{MsgID: env.MsgID, Inner: inner})
		ackIDs = append(ackIDs, env.MsgID)
	}

	if *jsonOut {
		buf, _ := json.MarshalIndent(decoded, "", "  ")
		fmt.Println(string(buf))
	} else {
		for _, m := range decoded {
			fmt.Printf("--- %s ---\n", m.MsgID)
			fmt.Printf("  from   : %s\n  kind   : %s\n  type   : %s\n",
				m.Inner.OriginNexus, m.Inner.Kind, m.Inner.ContentType)
			if m.Inner.InReplyTo != "" {
				fmt.Printf("  reply  : %s\n", m.Inner.InReplyTo)
			}
			fmt.Printf("  body   :\n%s\n\n", m.Inner.Body)
		}
	}

	if !*noAck && len(ackIDs) > 0 {
		evicted, err := c.Ack(ctx, paired, paired.PathID(), ackIDs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recv: ack failed (envelopes will redeliver): %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "(acked %d, evicted %d)\n", len(ackIDs), evicted)
		}
	}
	return nil
}

// runAck explicitly acknowledges one or more msg_ids on the peer's mailbox.
func runAck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ack", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	asID := fs.String("as", "", "our nexus-id (defaults to single id under -store)")
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	_ = fs.Parse(args)
	if *relayURL == "" {
		return errors.New("ack: -relay required")
	}
	if fs.NArg() < 2 {
		return errors.New("ack: <peer-id> <msg-id> [<msg-id>...] required")
	}
	peerID := fs.Arg(0)
	msgIDs := fs.Args()[1:]

	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	paired, err := ch.GetPaired(ctx, peerID)
	if err != nil {
		return fmt.Errorf("ack: load pair %s: %w", peerID, err)
	}
	if paired == nil {
		return fmt.Errorf("ack: not paired with %s — run pair-with-token first", peerID)
	}

	c := &relay.Client{BaseURL: strings.TrimRight(*relayURL, "/"), HTTP: insecureHTTP()}
	evicted, err := c.Ack(ctx, paired, paired.PathID(), msgIDs)
	if err != nil {
		return err
	}
	fmt.Printf("acked %d, evicted %d\n", len(msgIDs), evicted)
	return nil
}

// ---------- helpers ----------

// uuidv7 produces a UUIDv7 (timestamp + random) per RFC 9562.
// Format: 48 bits unix-ms timestamp, 4-bit version (7), 12 bits random,
// 2-bit variant (10), 62 bits random.
func uuidv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint64(b[0:8], ms<<16) // shift left so ms occupies top 48 bits
	// Bytes 6-7: 4-bit version (0x7) + 12 bits random; 12 bits already random, set version nibble
	b[6] = (b[6] & 0x0f) | 0x70
	// Byte 8: 2-bit variant (10) + 6 bits random
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// decodeCiphertext accepts either base64-std or base64-url-no-pad
// encoding for the outer envelope's ciphertext field. Senders we
// control (relay.BuildOuter) use raw-url; we accept either to interop
// with peers whose JSON libraries default to std.
func decodeCiphertext(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}
