// Command relay-cli is the operator-facing CLI for the Frame-to-Frame
// relay (interchange). Subcommands:
//
//	init <nexus-id> [-store DIR] [-dh P-256|X25519]
//	    Generate a casket Channel with persistent keys.
//
//	token -endpoint URL [-store DIR]
//	    Print our PairingToken (JSON) for OOB exchange with a peer.
//
//	pair -target ID -relay URL [-endpoint URL] [-store DIR]
//	    Requester side: submit a signed pair half, poll until decided.
//
//	pending -tailnet URL
//	    Owner side: list pending pair requests on our relay.
//
//	approve <request-id> -tailnet URL [-as ID] [-store DIR]
//	    Owner side: build + sign owner half, POST approve.
//
//	deny <request-id> -tailnet URL
//	    Owner side: deny a pending request (no signed half required).
//
// Identity persistence: a tiny file-backed ChannelStorage rooted at
// -store (default ./relay-state). One subdir per nexus-id.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	casket "github.com/nexus-cw/casket-go"
	"github.com/nexus-cw/nexus/relay"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	ctx := context.Background()

	var err error
	switch sub {
	case "init":
		err = runInit(ctx, args)
	case "token":
		err = runToken(ctx, args)
	case "pair":
		err = runPair(ctx, args)
	case "pair-with-token":
		err = runPairWithToken(ctx, args)
	case "pending":
		err = runPending(ctx, args)
	case "approve":
		err = runApprove(ctx, args)
	case "deny":
		err = runDeny(ctx, args)
	case "discover":
		err = runDiscover(ctx, args)
	case "health":
		err = runHealth(ctx, args)
	case "status":
		err = runStatus(ctx, args)
	case "send":
		err = runSend(ctx, args)
	case "recv":
		err = runRecv(ctx, args)
	case "ack":
		err = runAck(ctx, args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "relay-cli: unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay-cli: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `relay-cli — operator CLI for the Nexus interchange

Bootstrap & discovery:
  discover -relay URL                          fetch /.well-known doc
  health   -relay URL                          GET /health

Identity:
  init <nexus-id> [-store DIR] [-dh P-256|X25519]
  token -endpoint URL [-store DIR] [-as ID]    print our PairingToken JSON

Pair flow (requester side):
  pair    -target ID -relay URL [-endpoint URL] [-store DIR] [-as ID]
  status  <request-id> -relay URL              one-shot status check

Pair flow (owner side, tailnet):
  pending -tailnet URL
  approve <request-id> -tailnet URL [-store DIR] [-as ID]
  deny    <request-id> -tailnet URL

Activate the pair locally (consume peer's PairingToken OOB):
  pair-with-token <token-json> [-store DIR] [-as ID] [-max-age SECS]
  pair-with-token -file PATH    [-store DIR] [-as ID]   (use - for stdin)

Mailbox:
  send <peer-id> <body...>      -relay URL [-kind K] [-content-type MIME] [-in-reply-to MSGID] [-store DIR] [-as ID]
  send <peer-id> -file PATH     -relay URL ...                              (use - for stdin)
  recv <peer-id>                -relay URL [-since MSGID] [-no-ack] [-json] [-store DIR] [-as ID]
  ack  <peer-id> <msg-id> [...] -relay URL [-store DIR] [-as ID]

Default store dir: ./relay-state
`)
}

// ---------- subcommands ----------

func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	dhAlg := fs.String("dh", "P-256", "ECDH curve: P-256 or X25519")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("init: exactly one positional <nexus-id> required")
	}
	nexusID := fs.Arg(0)

	st, err := openStore(*store, nexusID)
	if err != nil {
		return err
	}
	ch, err := casket.Load(ctx, nexusID, st, casket.DhAlgorithm(*dhAlg))
	if err != nil {
		return fmt.Errorf("casket.Load: %w", err)
	}
	fmt.Printf("initialized %s\n  store    : %s\n  pubkey   : %s\n  dh_pubkey: %s\n  dh_alg   : %s\n",
		nexusID, filepath.Join(*store, nexusID),
		ch.PublicKeyB64u(), ch.DHPublicKeyB64u(), ch.DHAlg())
	return nil
}

func runToken(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	endpoint := fs.String("endpoint", "", "our public relay endpoint URL (required)")
	asID := fs.String("as", "", "nexus-id (defaults to single id under -store)")
	_ = fs.Parse(args)
	if *endpoint == "" {
		return errors.New("token: -endpoint required")
	}
	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	tok := ch.MakePairingToken(*endpoint)
	out, _ := json.MarshalIndent(tok, "", "  ")
	fmt.Println(string(out))
	return nil
}

func runPair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	target := fs.String("target", "", "target nexus-id (required)")
	relayURL := fs.String("relay", "", "interchange base URL (required)")
	endpoint := fs.String("endpoint", "", "our endpoint URL (advertised to peer)")
	asID := fs.String("as", "", "nexus-id (defaults to single id under -store)")
	_ = fs.Parse(args)
	if *target == "" || *relayURL == "" {
		return errors.New("pair: -target and -relay required")
	}
	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	c := &relay.Client{
		BaseURL:      strings.TrimRight(*relayURL, "/"),
		HTTP:         insecureHTTP(),
		PollInterval: 3 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "submitting pair request as %s → target %s on %s ...\n", id, *target, *relayURL)
	res, err := c.Pair(ctx, ch, id, *target, *endpoint)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))

	// v2: if approved and the relay returned the owner's half, auto-
	// instantiate the local PairedChannel. Eliminates the manual
	// pair-with-token step that the v1 flow required.
	if res.Status == "approved" && res.OwnerHalf != nil {
		paired, err := autoPairFromHalf(ctx, ch, *res.OwnerHalf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: auto-pair from owner_half failed (%v); peer's PairingToken can still be exchanged out-of-band as a fallback\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "auto-paired with %s (path_id=%s)\n", paired.PeerID(), paired.PathID())
		}
	}
	return nil
}

func runPending(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	tailnet := fs.String("tailnet", "", "owner tailnet base URL (required, e.g. http://100.x.x.x:10001)")
	_ = fs.Parse(args)
	if *tailnet == "" {
		return errors.New("pending: -tailnet required")
	}
	url := strings.TrimRight(*tailnet, "/") + "/pair/requests?status=pending"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := insecureHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("pending: HTTP %d: %s", resp.StatusCode, body)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(body))
	}
	return nil
}

func runApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	store := fs.String("store", "./relay-state", "state dir")
	tailnet := fs.String("tailnet", "", "owner tailnet base URL (required)")
	asID := fs.String("as", "", "owner nexus-id (defaults to single id under -store)")
	endpoint := fs.String("endpoint", "", "our endpoint URL (optional)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("approve: exactly one positional <request-id> required")
	}
	if *tailnet == "" {
		return errors.New("approve: -tailnet required")
	}
	requestID := fs.Arg(0)
	id, err := resolveID(*store, *asID)
	if err != nil {
		return err
	}
	ch, err := loadChannel(ctx, *store, id)
	if err != nil {
		return err
	}
	half, err := relay.BuildSignedPairHalf(ch, id, *endpoint)
	if err != nil {
		return err
	}
	body := struct {
		Owner relay.PairHalfPayload `json:"owner"`
	}{Owner: half}
	raw, _ := json.Marshal(body)
	url := strings.TrimRight(*tailnet, "/") + "/pair/requests/" + requestID + "/approve"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := insecureHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("approve: HTTP %d: %s", resp.StatusCode, respBody)
	}
	fmt.Println(string(respBody))

	// v2: parse the requester_half from the response and auto-instantiate
	// the local PairedChannel. Eliminates the manual pair-with-token
	// step that the v1 flow required.
	var parsed struct {
		Status        string                 `json:"status"`
		RequesterHalf *relay.PairHalfPayload `json:"requester_half"`
	}
	if err := json.Unmarshal(respBody, &parsed); err == nil && parsed.Status == "approved" && parsed.RequesterHalf != nil {
		paired, err := autoPairFromHalf(ctx, ch, *parsed.RequesterHalf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: auto-pair from requester_half failed (%v); peer's PairingToken can still be exchanged out-of-band as a fallback\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "auto-paired with %s (path_id=%s)\n", paired.PeerID(), paired.PathID())
		}
	}
	return nil
}

func runDeny(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deny", flag.ExitOnError)
	tailnet := fs.String("tailnet", "", "owner tailnet base URL (required)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("deny: exactly one positional <request-id> required")
	}
	if *tailnet == "" {
		return errors.New("deny: -tailnet required")
	}
	requestID := fs.Arg(0)
	url := strings.TrimRight(*tailnet, "/") + "/pair/requests/" + requestID + "/deny"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := insecureHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("deny: HTTP %d: %s", resp.StatusCode, respBody)
	}
	fmt.Println(string(respBody))
	return nil
}

// ---------- helpers ----------

// resolveID returns asID if set, else the single nexus-id present in store.
// Errors if zero or multiple ids exist and -as is unset.
func resolveID(store, asID string) (string, error) {
	if asID != "" {
		return asID, nil
	}
	entries, err := os.ReadDir(store)
	if err != nil {
		return "", fmt.Errorf("read store %s: %w (run `init` first)", store, err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no identities in %s — run `init` first", store)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("multiple identities in %s (%v) — pass -as", store, ids)
	}
}

func loadChannel(ctx context.Context, store, id string) (*casket.Channel, error) {
	st, err := openStore(store, id)
	if err != nil {
		return nil, err
	}
	// Pass empty DhAlg to honor whatever was persisted at init.
	return casket.Load(ctx, id, st, "")
}

// autoPairFromHalf delegates to relay.PairFromHalf with a 1-hour skew
// window. CLI-side wrapper so the call sites in runPair / runApprove
// stay short and the warn-on-failure UX is consistent.
//
// 3600s is generous: the half was signed at submit time and is
// signature-bound to that ts; the relay already validated freshness
// at submit. SDK consumers wanting tighter bounds call relay.PairFromHalf
// directly.
func autoPairFromHalf(ctx context.Context, ch *casket.Channel, h relay.PairHalfPayload) (*casket.PairedChannel, error) {
	return relay.PairFromHalf(ctx, ch, h, 3600)
}
