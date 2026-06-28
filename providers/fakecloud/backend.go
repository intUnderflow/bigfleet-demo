package fakecloud

import (
	"context"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// fakeBackend is one fake cloud's providerkit.Backend. It advertises a Catalog
// and actuates the abstract Machine lifecycle as substrate NO-OPS — the kube
// Node is minted separately by cmd/node-creator off the UpcomingNode CR, so the
// provider never touches Kubernetes (that separation keeps the demo's one
// Node-writer legible and avoids a double-mint race).
//
// The only deliberate substrate effect is the optional provisioning dwell in
// CreateInstance (the demo's honest "transfer speed simulated" window). It
// implements Backend but NOT Deleter: a bare-metal sub-backend never deletes,
// and the cloud sub-backends are composed behind multiplexBackend, which owns
// the Deleter capability and routes by machine-ID prefix.
type fakeBackend struct {
	catalog Catalog
	dwell   func(context.Context) // ctx-honoring provisioning dwell; nil = instant
}

func (b *fakeBackend) Describe(context.Context) ([]providerkit.Instance, error) {
	return b.catalog.instances(), nil
}

// CreateInstance actuates a Speculative slot into an Idle host. For the fake
// substrate that is just the (optional) dwell + a synthetic HostRef; the kit
// settles the record at Idle. Honors ctx (the kit cancels it on the Create
// timeout).
func (b *fakeBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	if b.dwell != nil {
		b.dwell(ctx)
	}
	return providerkit.CreateInstanceResult{
		Host: providerkit.HostRef{Provider: b.catalog.Cloud, Ref: "host-" + req.Machine.ID},
	}, nil
}

// ConfigureInstance binds the host to a cluster. No-op on the fake substrate —
// node-creator mints the kwok Node once the operator emits the UpcomingNode.
func (b *fakeBackend) ConfigureInstance(context.Context, providerkit.ConfigureInstanceRequest) error {
	return nil
}

// DrainInstance returns a Configured host to Idle. No-op (node-creator owns the
// real cordon→drain→delete of the kwok Node).
func (b *fakeBackend) DrainInstance(context.Context, providerkit.DrainInstanceRequest) error {
	return nil
}

// deleteInstance tears a cloud host down, returning the slot to Speculative. It
// is an unexported helper the multiplexer calls for cloud sub-backends; the
// on-prem sub-backend's machines (Idle, never deletable) never reach it.
func (b *fakeBackend) deleteInstance(context.Context, providerkit.DeleteInstanceRequest) error {
	return nil
}

var _ providerkit.Backend = (*fakeBackend)(nil)
