package broker

import "github.com/CarriedWorldUniverse/nexus/nexus/frames"

// handleCWBRequest relays an aspect's CWB API call through the connection's
// custodied herald client (token injected) and returns the response. Requires
// a herald-bound connection; the aspect acts as its own org identity and CWB
// enforces authz. The frame carries pillar+path (not a URL), so the broker
// pins the destination host to the configured CWB edge.
func (c *wsConn) handleCWBRequest(env frames.Envelope) {
	if c.heraldClient == nil {
		c.respondError(env, "cwb.request requires a herald-bound connection")
		return
	}
	var p frames.CWBRequestPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.respondError(env, "cwb.request: bad payload: "+err.Error())
		return
	}
	if p.Pillar == "" || p.Method == "" || p.Path == "" {
		c.respondError(env, "cwb.request: pillar, method, path required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	resp, raw, err := c.heraldClient.Do(ctx, p.Method, p.Pillar, p.Path, p.Body)
	if err != nil {
		c.respondError(env, "cwb relay: "+err.Error())
		return
	}
	out, nerr := frames.NewResponse(frames.KindCWBResponse, env.ID, frames.CWBResponsePayload{
		Status: resp.StatusCode,
		Body:   raw,
	})
	if nerr != nil {
		c.respondError(env, "cwb.request: build response: "+nerr.Error())
		return
	}
	c.send(out)
}
