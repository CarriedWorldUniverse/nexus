package broker

import (
	"context"

	"github.com/CarriedWorldUniverse/cwb-client/client"
)

// Custodian is the broker's view of the per-aspect herald token custodian
// (nexus/cwb/custodian). An interface so tests inject a fake. When the broker
// has no HeraldEdge configured, the field is nil and herald-auth is skipped.
type Custodian interface {
	Redeem(ctx context.Context, assertion string) (subject string, err error)
	Client(subject string) (*client.Client, error)
	Forget(subject string)
}
