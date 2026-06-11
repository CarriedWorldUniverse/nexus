// gateway.go bridges wsasp.Client into the in-process funnel.
//
// Two seams meet here:
//
//  1. Outbound — `Gateway` implements funnel.ChatGateway. The funnel
//     calls SendChat / ReactTo / ReadThread / AnnounceFile / ShareFile
//     during deliberation; this type forwards each call to the
//     equivalent wsasp.Client method, which translates to a frame and
//     ships it through the WS pipe.
//
//  2. Inbound — `Bridge` is the OnDeliver callback the aspect-binary
//     wires into wsasp.Config.OnDeliver. It translates DeliveredMessage
//     into bridle.InboxItem and calls funnel.ReceiveWithMsgID so the
//     deliberation loop picks it up. Lock 6 cursor advancement happens
//     inside wsasp before we get here, so a panic in the funnel doesn't
//     re-replay the same message.

package wsasp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// Gateway adapts a wsasp.Client to the funnel.ChatGateway interface.
// One Gateway per Client; thread-safe (the underlying client is).
type Gateway struct {
	client *Client
}

// NewGateway returns a Gateway wrapping the supplied client.
func NewGateway(client *Client) *Gateway {
	return &Gateway{client: client}
}

// SendChat delegates to the wsasp Client's SendChat. The msg_id
// returned is always 0 — chat.send is fire-and-forget, the broker
// doesn't ack with the assigned id (per transport spec). Callers that
// need the id read it from the next chat.deliver frame instead.
func (g *Gateway) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
	return g.client.SendChat(ctx, content, replyTo, topic)
}

// ReactTo delegates to the wsasp Client's ReactTo.
func (g *Gateway) ReactTo(ctx context.Context, msgID int64, emoji string) error {
	return g.client.ReactTo(ctx, msgID, emoji)
}

// ReadThread translates the chat.read.result payload into the funnel's
// ChatMessage shape. Both are intentionally trivial maps — the wire
// format and the funnel's internal type are kept aligned in
// payloads.go + comms.go so this conversion stays one-to-one.
func (g *Gateway) ReadThread(ctx context.Context, threadID, sinceID int64) ([]funnel.ChatMessage, error) {
	msgs, err := g.client.ReadThread(ctx, threadID, sinceID)
	if err != nil {
		return nil, err
	}
	out := make([]funnel.ChatMessage, len(msgs))
	for i, m := range msgs {
		out[i] = funnel.ChatMessage{
			ID:         int64(m.ID),
			From:       m.From,
			Content:    m.Content,
			ReplyTo:    int64(m.ReplyTo),
			ReceivedAt: m.ReceivedAt,
		}
	}
	return out, nil
}

// AnnounceFile delegates to the Client.
func (g *Gateway) AnnounceFile(ctx context.Context, path, description string) (int64, error) {
	return g.client.AnnounceFile(ctx, path, description)
}

// ShareFile delegates to the Client.
func (g *Gateway) ShareFile(ctx context.Context, path string, recipients []string) (int64, error) {
	return g.client.ShareFile(ctx, path, recipients)
}

// Spawn implements funnel.SpawnGateway (NEX-609): the native-comms
// spawn tool emits spawn.request on this aspect's own authenticated
// WS via the Client and maps the broker handles into the funnel's
// local shape.
func (g *Gateway) Spawn(ctx context.Context, brief string, count int, thread string) ([]funnel.SpawnHandle, error) {
	hands, err := g.client.Spawn(ctx, brief, count, thread)
	if err != nil {
		return nil, err
	}
	out := make([]funnel.SpawnHandle, len(hands))
	for i, h := range hands {
		out[i] = funnel.SpawnHandle{RunID: h.RunID, Name: h.Name, Error: h.Error}
	}
	return out, nil
}

// ConveneClose implements funnel.ConveneGateway (roundtable P3): the
// facilitator's convene_close tool emits convene.close on this
// aspect's own authenticated WS via the Client.
func (g *Gateway) ConveneClose(ctx context.Context, conveneID, status string, summaryMsgID int64) (string, error) {
	return g.client.ConveneClose(ctx, conveneID, status, summaryMsgID)
}

// ReadMessage / ListShared / GetShared are not yet wired on the
// wsasp wire — out-of-process aspects can't reach the chat store
// directly, and the wire frames for these reads aren't part of the
// transport spec yet. Returning an error keeps the interface
// satisfied while making the gap visible to callers.

func (g *Gateway) ReadMessage(ctx context.Context, msgID int64) (funnel.ChatMessage, error) {
	return funnel.ChatMessage{}, fmt.Errorf("wsasp.Gateway: ReadMessage not implemented on wire")
}

func (g *Gateway) ListShared(ctx context.Context, limit int) ([]funnel.SharedFileRef, error) {
	return nil, fmt.Errorf("wsasp.Gateway: ListShared not implemented on wire")
}

func (g *Gateway) GetShared(ctx context.Context, shareID int64) (funnel.SharedFileRef, error) {
	return funnel.SharedFileRef{}, fmt.Errorf("wsasp.Gateway: GetShared not implemented on wire")
}

// Bridge connects wsasp's inbound stream to a funnel. Wire it as:
//
//	bridge := wsasp.NewBridge(myFunnel)
//	cfg := wsasp.Config{
//	    ...
//	    OnDeliver: bridge.OnDeliver,
//	}
//
// The bridge is intentionally minimal — translation only. Funnel
// owns the deliberation lifecycle; wsasp owns the wire. This module
// keeps them decoupled so each can be tested in isolation.
type Bridge struct {
	funnel ReceiveTarget
}

// ReceiveTarget is the subset of *funnel.Funnel that the bridge
// needs. Defined as an interface so tests can inject a recorder
// without standing up a real funnel.
type ReceiveTarget interface {
	ReceiveWithMsgID(item bridle.InboxItem, msgID int64)
}

// NewBridge wraps a ReceiveTarget. Pass *funnel.Funnel here in
// production; tests can pass a mock that records calls.
func NewBridge(target ReceiveTarget) *Bridge {
	return &Bridge{funnel: target}
}

// OnDeliver is the wsasp.Config.OnDeliver callback. Translates a
// DeliveredMessage into bridle.InboxItem and dispatches to the
// funnel. msg_id is forwarded so the funnel's usage attribution
// (F3.1/F3.2) and downstream frames carry the chat row id.
//
// Replay flag is currently dropped — the funnel doesn't yet branch
// on it. When it does (e.g. age-aware deliberation per Lock 6), the
// translation grows to forward the flag.
func (b *Bridge) OnDeliver(msg DeliveredMessage) {
	item := bridle.InboxItem{
		From:       msg.From,
		Content:    msg.Content,
		ThreadRoot: msg.ThreadRoot,
		// ReplyTo intentionally omitted — bridle.InboxItem doesn't
		// carry a reply context today (the funnel reconstructs reply
		// chains via chat.read when the model asks). When InboxItem
		// grows a ReplyTo field, this is the wire-up point.
	}
	b.funnel.ReceiveWithMsgID(item, msg.ID)
}

// MarshalDebug renders the bridge's last-seen state for diagnostics —
// useful for a future status endpoint or supervisor surface. Unused
// today; the symbol is exported so external callers can inspect.
func (b *Bridge) MarshalDebug() json.RawMessage {
	out, _ := json.Marshal(map[string]any{
		"funnel_attached": b.funnel != nil,
	})
	return out
}

// Compile-time interface checks.
var (
	_ funnel.ChatGateway    = (*Gateway)(nil)
	_ funnel.SpawnGateway   = (*Gateway)(nil)
	_ funnel.ConveneGateway = (*Gateway)(nil)
)

// (frames import retained for future inbound-translation logic — the
// gateway-side translation today happens via ReadThread inline. If
// new inbound frames need bridging, the imports are already in scope.)
var _ = frames.KindChatDeliver
