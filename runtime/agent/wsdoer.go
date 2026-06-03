package agent

import (
	"context"
	"fmt"
	"net/http"

	cwbclient "github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// wsDoer is a cwb-client client.Doer that relays CWB API calls over the
// aspect's WS to nexus (which executes them as the aspect via its custodied
// herald token). The aspect thus holds no CWB bearer and makes no HTTP call —
// its pillar wrappers run over this transport.
type wsDoer struct{ a *Agent }

func (d *wsDoer) Do(ctx context.Context, method, pillar, path string, body []byte) (*http.Response, []byte, error) {
	req, err := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{
		Pillar: pillar, Method: method, Path: path, Body: body,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("wsDoer: build request: %w", err)
	}
	resp, err := d.a.ws.Request(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("wsDoer: relay: %w", err)
	}
	if string(resp.Kind) == string(frames.KindCWBRequest)+".error" {
		var e map[string]string
		_ = frames.PayloadAs(resp, &e)
		msg := e["error"]
		if msg == "" {
			msg = "cwb relay failed"
		}
		return nil, nil, fmt.Errorf("wsDoer: nexus error: %s", msg)
	}
	var p frames.CWBResponsePayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		return nil, nil, fmt.Errorf("wsDoer: bad response: %w", err)
	}
	return &http.Response{StatusCode: p.Status}, p.Body, nil
}

// CWBDoer returns a client.Doer that routes the aspect's CWB calls over the WS.
func (a *Agent) CWBDoer() cwbclient.Doer { return &wsDoer{a: a} }
