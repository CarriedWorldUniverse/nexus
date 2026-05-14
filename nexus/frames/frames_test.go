package frames

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewStampsTimeAndMarshalsPayload(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	env, err := New(KindTurn, TurnPayload{Prompt: "hi"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	if env.Kind != KindTurn {
		t.Errorf("Kind = %q", env.Kind)
	}
	if env.TS.Before(before) || env.TS.After(after) {
		t.Errorf("TS %v outside expected window [%v, %v]", env.TS, before, after)
	}
	if len(env.Payload) == 0 {
		t.Error("Payload not marshalled")
	}

	var back TurnPayload
	if err := PayloadAs(env, &back); err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if back.Prompt != "hi" {
		t.Errorf("Prompt = %q", back.Prompt)
	}
}

func TestNewRequestGeneratesID(t *testing.T) {
	req, err := NewRequest(KindKnowledgeSearch, KnowledgeSearchPayload{Text: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if req.ID == "" {
		t.Error("NewRequest should generate an ID; got empty")
	}
	if len(req.ID) != 26 {
		t.Errorf("ID length = %d, want 26 (ULID canonical)", len(req.ID))
	}
	if req.InReplyTo != "" {
		t.Errorf("request InReplyTo = %q, want empty", req.InReplyTo)
	}

	resp, err := NewResponse(KindKnowledgeSearchResult, req.ID, KnowledgeSearchResultPayload{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InReplyTo != req.ID {
		t.Errorf("response InReplyTo = %q, want %q", resp.InReplyTo, req.ID)
	}
	if resp.ID != "" {
		t.Errorf("response ID = %q, want empty (server-side responses don't initiate)", resp.ID)
	}
}

func TestNewRequestIDsAreUniqueUnderConcurrency(t *testing.T) {
	const goroutines = 50
	const per = 20
	ids := make(chan string, goroutines*per)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				req, err := NewRequest(KindTurn, TurnPayload{Prompt: "x"})
				if err != nil {
					t.Errorf("NewRequest: %v", err)
					return
				}
				ids <- req.ID
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, goroutines*per)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID under concurrency: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != goroutines*per {
		t.Errorf("seen %d unique ids, want %d", len(seen), goroutines*per)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	orig, err := New(KindTurnResult, TurnResultPayload{
		Output:     "hello",
		StopReason: "end_turn",
		Tokens:     TokenUsage{Input: 10, Output: 5, Total: 15},
		EntryIDs:   []string{"a", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	wire, err := Encode(orig)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Kind != orig.Kind {
		t.Errorf("Kind drift: %q != %q", got.Kind, orig.Kind)
	}

	var back TurnResultPayload
	if err := PayloadAs(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.Output != "hello" || back.Tokens.Total != 15 || len(back.EntryIDs) != 2 {
		t.Errorf("payload round-trip drift: %+v", back)
	}
}

func TestEncodeRejectsMissingKind(t *testing.T) {
	_, err := Encode(Envelope{TS: time.Now()})
	if err == nil {
		t.Error("Encode should reject frame with no kind")
	}
}

func TestDecodeRejectsMissingKind(t *testing.T) {
	raw := []byte(`{"ts":"2026-04-25T00:00:00Z"}`)
	_, err := Decode(raw)
	if err == nil {
		t.Error("Decode should reject frame with no kind")
	}
}

func TestDecodeToleratesUnknownKind(t *testing.T) {
	// Forward-compat: an unknown kind should still decode cleanly.
	// Rejection is the caller's job (log + drop).
	raw := []byte(`{"kind":"something.new","ts":"2026-04-25T00:00:00Z","payload":{"foo":1}}`)
	env, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if env.Kind != "something.new" {
		t.Errorf("Kind = %q", env.Kind)
	}
	if IsKnown(env.Kind) {
		t.Error("IsKnown(something.new) should be false")
	}
}

func TestIsKnownCoversAllDeclaredKinds(t *testing.T) {
	declared := []Kind{
		KindRegister, KindRegisterAck, KindDeregister,
		KindOutpostRegister, KindOutpostRegisterAck, KindOutpostDeregister,
		KindTurn, KindTurnResult,
		KindDispatch, KindDispatchResult, KindDispatchError,
		KindChatSend, KindChatDeliver, KindChatReaction, KindChatRead,
		KindChatReadResult, KindAnnounceFile, KindShareFile, KindFileResult,
		KindAspectActivity,
		KindKnowledgeStore, KindKnowledgeSearch, KindKnowledgeSearchResult,
		KindSessionEntryAppended, KindSessionRewind, KindSessionFork,
		KindShutdown,
	}
	for _, k := range declared {
		if !IsKnown(k) {
			t.Errorf("IsKnown(%q) returned false — update the switch in IsKnown", k)
		}
	}
}

func TestPayloadAsReturnsErrOnEmptyPayload(t *testing.T) {
	// Empty payloads return ErrNoPayload so callers must explicitly
	// handle the case — silent no-op would hide routing bugs.
	env, err := New(KindShutdown, nil)
	if err != nil {
		t.Fatal(err)
	}
	var dst ShutdownPayload
	err = PayloadAs(env, &dst)
	if !errors.Is(err, ErrNoPayload) {
		t.Errorf("PayloadAs on empty payload err = %v, want ErrNoPayload", err)
	}
}

func TestPayloadAsNonEmptyPayloadDecodes(t *testing.T) {
	env, err := New(KindShutdown, ShutdownPayload{Reason: "test", GracePeriodS: 5})
	if err != nil {
		t.Fatal(err)
	}
	var dst ShutdownPayload
	if err := PayloadAs(env, &dst); err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if dst.Reason != "test" || dst.GracePeriodS != 5 {
		t.Errorf("decoded = %+v", dst)
	}
}

// A handful of spot-checks that the payload structs themselves
// serialise cleanly with the expected JSON field names. Guards
// against accidental tag breakage during future edits.
func TestPayloadJSONTags(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		needs   []string // substrings that must appear in the JSON output
	}{
		{
			"RegisterAck",
			RegisterAckPayload{HeartbeatIntervalS: 15, StaleAfterS: 60},
			[]string{`"heartbeat_interval_s":15`, `"stale_after_s":60`},
		},
		{
			"Dispatch",
			DispatchPayload{
				Aspect: "wren", Thread: "t-1", DispatchID: "d-1",
				Payload: map[string]any{"text": "x"},
			},
			[]string{`"aspect":"wren"`, `"thread":"t-1"`, `"dispatch_id":"d-1"`},
		},
		{
			"SessionEntryAppended",
			SessionEntryAppendedPayload{
				Aspect: "harrow", SessionID: "sess-1", EntryID: "e-1",
				EntryKind: "turn.user",
			},
			[]string{`"aspect":"harrow"`, `"entry_id":"e-1"`, `"entry_kind":"turn.user"`},
		},
		{
			"ChatDeliver_WithReceivedAt_Lock6",
			ChatDeliverPayload{
				ID: 42, From: "operator", Content: "hi",
				ReceivedAt: "2026-05-02T05:30:00Z", Reason: "mention",
			},
			// Lock 6: received_at must be in the wire shape; no
			// silent fallback to the old `at` field.
			[]string{`"id":42`, `"received_at":"2026-05-02T05:30:00Z"`, `"reason":"mention"`},
		},
		{
			"ChatDeliver_ReplayFlag_Lock6",
			ChatDeliverPayload{
				ID: 100, From: "operator", Content: "old msg",
				ReceivedAt: "2026-05-01T00:00:00Z", Reason: "mention", Replay: true,
			},
			[]string{`"replay":true`},
		},
		{
			"AnnounceFile",
			AnnounceFilePayload{From: "frame", Path: "/tmp/x.md", Description: "spec"},
			[]string{`"from":"frame"`, `"path":"/tmp/x.md"`, `"description":"spec"`},
		},
		{
			"ShareFile",
			ShareFilePayload{From: "frame", Path: "/x", Recipients: []string{"anvil", "wren"}},
			[]string{`"recipients":["anvil","wren"]`},
		},
		{
			"FileResult_Announce",
			FileResultPayload{MsgID: 9001},
			[]string{`"msg_id":9001`},
		},
		{
			"AspectActivity",
			AspectActivityPayload{
				Type: "turn.start", AspectID: "forge",
				EmittedAt: "2026-05-02T05:30:00Z",
				Payload:   []byte(`{"turn_id":"t-1"}`),
			},
			[]string{`"type":"turn.start"`, `"aspect_id":"forge"`, `"turn_id":"t-1"`},
		},
		{
			"ChatReadResult",
			ChatReadResultPayload{Messages: []ChatDeliverPayload{
				{ID: 1, From: "operator", Content: "root", ReceivedAt: "2026-05-02T05:30:00Z", Reason: "thread"},
			}},
			[]string{`"messages":[`, `"id":1`},
		},
		{
			"CredentialFetch_ByName",
			CredentialFetchPayload{Kind: "jira", Name: "jira-prod"},
			[]string{`"kind":"jira"`, `"name":"jira-prod"`},
		},
		{
			"CredentialFetch_ByDefault",
			CredentialFetchPayload{Kind: "imap"},
			// Name should be omitted when unset (omitempty).
			[]string{`"kind":"imap"`},
		},
		{
			"CredentialFetchResult",
			CredentialFetchResultPayload{
				Name: "jira-prod", Kind: "jira",
				Bundle: map[string]any{
					"atlassian_email":     "ops@example.com",
					"atlassian_token":     "tok-abc",
					"atlassian_subdomain": "myorg",
				},
			},
			[]string{`"name":"jira-prod"`, `"kind":"jira"`, `"atlassian_subdomain":"myorg"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.payload)
			if err != nil {
				t.Fatal(err)
			}
			s := string(raw)
			for _, need := range c.needs {
				if !strings.Contains(s, need) {
					t.Errorf("JSON %q missing substring %q", s, need)
				}
			}
		})
	}
}
