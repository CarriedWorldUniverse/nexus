# Unify Frame + Aspect Chat Path — Spec

**Date:** 2026-05-04
**Status:** Draft
**Owner:** keel
**Trigger:** F2.6 smoke surfaced two real bugs (operator #9821):
1. Operator chat.send → broker → no chat.Store row → funnel deliberation works in memory but reply has no parent + usage.Record FK fails
2. `framecomms.Gateway` (Frame's ChatGateway) writes chat.Store directly; broker's WS chat.send handler doesn't write at all. Two parallel paths for the same protocol, already diverged.

**Companion to:** `2026-05-02-aspect-funnel-architecture.md` (six locks), `2026-05-04-operator-as-aspect-ws-extension.md` (WS protocol)

---

## 1. Problem

Today's nexus has two implementations of "post a chat message to the bus":

| Origin | Path | Persistence |
|---|---|---|
| Frame (in-process) | `framecomms.Gateway.SendChat` → `chat.Store.Insert` directly | ✅ writes |
| Aspect (out-of-process WS) | `wsasp.Gateway.SendChat` → frame on WS → broker `handleChatSendFrame` → `ChatRouter.RouteChat` callback | ❌ does NOT write |
| Operator (browser WS) | same as aspect | ❌ does NOT write |

The route callback fans out to the funnel for deliberation, but never persists. Result: operator/aspect input is transient, never gets a `chat_messages.id`, downstream FK constraints (usage.Record references chat_messages) fail.

Operator framing (#9821): "the aspect doesn't have to be a separate process — it can ship with the Frame deploy. But it must use funnel/bridle the same way." Translation: deploy locality is independent from protocol locality. The Frame can stay in-process; what must unify is the **chat.send code path**.

## 2. Solution

Single chat.send handler. Both gateways converge on it.

```
                     ┌──────────────────────────┐
                     │ broker.HandleChatSend()  │
                     │  1. INSERT chat.Store    │
                     │  2. emit msg_id          │
                     │  3. RouteRecipients      │
                     │  4. fan out deliver      │
                     └────────────┬─────────────┘
                                  ▲
                ┌─────────────────┴──────────────────┐
                │                                    │
   inproc gateway (Frame)                wsasp gateway (forge, operator)
   (same goroutine,                      (WS frame → broker
    just calls broker.HandleChatSend)     handleChatSendFrame
                                           → broker.HandleChatSend)
```

The Frame's `framecomms.Gateway` becomes a thin shim that calls `broker.HandleChatSend` directly. The broker's WS handler becomes a thin shim that decodes the frame and calls `broker.HandleChatSend`. Both end up at the same function with the same arguments.

## 3. Concrete changes

### 3.1 Add `Broker.HandleChatSend` (the single canonical path)

```go
// broker/chat_send.go (new file)

// HandleChatSend persists an inbound chat message and fans it out to
// recipients via Lock 2 routing. The single canonical chat-send path —
// every gateway (in-process Frame, out-of-process aspect WS, operator
// WS) lands here.
//
// Returns the assigned chat msg_id so callers can use it for reply
// chains, usage attribution, etc.
func (b *Broker) HandleChatSend(ctx context.Context, from, content string, replyTo int64, topic string) (int64, error) {
    if b.cfg.ChatStore == nil {
        return 0, errors.New("broker: chat store not configured")
    }

    // 1. Persist. ChatStore.Insert mints the id + timestamp.
    msg, err := b.cfg.ChatStore.Insert(ctx, from, content, replyTo, topic)
    if err != nil {
        return 0, fmt.Errorf("broker: chat insert: %w", err)
    }

    // 2. Compute recipients per Lock 2 RecipientPolicy.
    recipients := b.cfg.RecipientPolicy.Compute(from, content, replyTo)

    // 3. Fan out chat.deliver to live WS connections + the Frame's
    //    in-process funnel. Both go through the dispatcher's
    //    "find binding for aspect-id" lookup.
    for _, rec := range recipients {
        b.deliverTo(ctx, rec, msg, "" /* reason: derive from policy */)
    }

    // 4. ChatRouter callback (legacy — Frame's Deliberate trigger). Eventually
    //    folds into deliverTo path so the Frame is just another recipient.
    if b.cfg.ChatRouter != nil && b.cfg.ChatRouter.RouteChat != nil {
        go b.cfg.ChatRouter.RouteChat(ctx, msg.ID, from, content, replyTo, topic)
    }

    return msg.ID, nil
}
```

### 3.2 Rewrite `broker.handleChatSendFrame` as thin shim

```go
func (c *wsConn) handleChatSendFrame(env frames.Envelope) {
    var p frames.ChatSendPayload
    if err := frames.PayloadAs(env, &p); err != nil {
        c.log.Warn("chat.send payload malformed", "err", err)
        return
    }
    from := p.From
    if from == "" { from = c.registeredAs }
    
    ctx := c.broker.ctx
    if ctx == nil { ctx = context.Background() }
    
    if _, err := c.broker.HandleChatSend(ctx, from, p.Content, int64(p.ReplyTo), p.Topic); err != nil {
        c.log.Warn("chat.send: handler error", "err", err)
    }
}
```

### 3.3 Replace `framecomms.Gateway.SendChat` with broker call

```go
// framecomms/gateway.go SendChat:
func (g *Gateway) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
    return g.broker.HandleChatSend(ctx, g.aspectID, content, replyTo, topic)
}
```

`framecomms.Gateway` keeps its other methods (ReactTo, ReadThread, AnnounceFile, ShareFile) but they all go through analogous broker handlers — `HandleReactTo`, `HandleChatRead`, etc. All persistence happens in one place.

### 3.4 Delete the FK error path

Once chat.send always inserts before routing, `usage.Record` finds the `chat_messages.id` it expects. The FK constraint failure I saw at 10:18:55 disappears.

## 4. Migration

This is a refactor of one code path. No data migration. No protocol change. Aspects (forge) and operator browsers don't change at all — they keep sending the same WS frames. Only the broker's internal handling reshapes.

Backwards compat: `ChatRouter.RouteChat` callback stays for now (so the Frame's deliberation triggers continue working). Eventually the Frame becomes a recipient like any other and the callback retires.

## 5. Test plan

- Unit: `HandleChatSend` writes to chat.Store, returns the assigned id, fans out via RecipientPolicy. Mock store + recorder.
- Integration: end-to-end smoke — operator sends "hello" via WS, expect chat_messages row exists, Frame receives chat.deliver, Frame's reply produces a second chat_messages row with reply_to set.
- Regression: existing chat tests continue to pass (the Frame's `framecomms.SendChat` should still result in a chat row, just via the new path).

## 6. Out of scope

- Lock 2 recipient computation refinements (already exists; reused as-is)
- Lock 6 replay (already wired; this change doesn't touch it)
- chat.read, react_to, announce_file, share_file persistence (analogous fix, separate spec)
- Frame-as-WS-client refactor (keel's earlier proposal; **not** what operator asked for)

## 7. Acceptance criteria

- [ ] `Broker.HandleChatSend` exists, persists + routes
- [ ] `handleChatSendFrame` is a thin shim
- [ ] `framecomms.Gateway.SendChat` calls `broker.HandleChatSend`
- [ ] Operator-via-WS message produces a chat_messages row
- [ ] Frame's reply produces a chat_messages row with reply_to set to the operator's row id
- [ ] usage.Record succeeds (FK satisfied)
- [ ] forge sees `@forge` mention via chat.deliver, deliberates, replies, reply persists
- [ ] All existing tests pass

## 8. Estimated work

~80–120 LOC change. Half a day with reviewer cycle. Does NOT need the Frame-as-WS-client refactor — that's a separate (larger) consideration for post-cutover.
