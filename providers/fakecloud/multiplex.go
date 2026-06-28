package fakecloud

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// multiplexBackend fronts the three fake clouds behind ONE providerkit.Server,
// so a single BigFleet shard (the per-session isolation boundary) drives one
// heterogeneous inventory spanning on-prem + AWS + GCP. This is NOT a network
// multiplexer: the three sub-backends are plain Go values composed in one
// process, so all fencing/idempotency/persistence stays in the single kit that
// wraps this type.
//
// It routes every actuator call by the machine-ID prefix (onprem-/aws-/gcp-)
// that Catalog.instances() stamps. It implements Deleter (cloud hosts tear
// down); a Delete targeting an on-prem id is structurally unreachable in
// practice — the shard's idle-release never deletes fixed capacity, and the
// conformance suite only ever walks Speculative slots, which on-prem has none
// of — so it is handled as a benign no-op rather than a special error code.
type multiplexBackend struct {
	subs  map[string]*fakeBackend
	order []string // stable cloud order for a deterministic Describe
}

// Options configures the multiplexed fleet. Sizes default (when zero) to the
// demo's 48-committed + 72-cloud split; conformance overrides them with a much
// larger Speculative seed (the suite consumes a fresh Speculative machine per
// behavior — see hack/conformance.sh).
type Options struct {
	OnpremBareMetal int
	AWSReserved     int
	AWSOnDemand     int
	AWSSpot         int
	GCPOnDemand     int
	GCPSpot         int
	CreateDwell     time.Duration // simulated provisioning latency in CreateInstance (0 = instant; node-creator owns the visible dwell)
}

func (o Options) withDefaults() Options {
	if o.OnpremBareMetal == 0 {
		o.OnpremBareMetal = 28
	}
	if o.AWSReserved == 0 {
		o.AWSReserved = 20
	}
	if o.AWSOnDemand == 0 {
		o.AWSOnDemand = 24
	}
	if o.AWSSpot == 0 {
		o.AWSSpot = 12
	}
	if o.GCPOnDemand == 0 {
		o.GCPOnDemand = 24
	}
	if o.GCPSpot == 0 {
		o.GCPSpot = 12
	}
	return o
}

// New builds the multiplexed three-cloud backend. The returned value is both a
// providerkit.Backend and a providerkit.Deleter; hand it straight to
// providerkit.New.
func New(o Options) *multiplexBackend {
	o = o.withDefaults()
	dwell := newDwell(o.CreateDwell)
	mk := func(c Catalog) *fakeBackend { return &fakeBackend{catalog: c, dwell: dwell} }
	return &multiplexBackend{
		subs: map[string]*fakeBackend{
			"onprem": mk(onpremCatalog(o.OnpremBareMetal)),
			"aws":    mk(awsCatalog(o.AWSReserved, o.AWSOnDemand, o.AWSSpot)),
			"gcp":    mk(gcpCatalog(o.GCPOnDemand, o.GCPSpot)),
		},
		order: []string{"onprem", "aws", "gcp"},
	}
}

func (m *multiplexBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	var out []providerkit.Instance
	for _, cloud := range m.order {
		ins, err := m.subs[cloud].Describe(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", cloud, err)
		}
		out = append(out, ins...)
	}
	return out, nil
}

// route resolves the sub-backend that owns a machine id by its cloud prefix.
func (m *multiplexBackend) route(id string) (*fakeBackend, error) {
	prefix := id
	if i := strings.IndexByte(id, '-'); i >= 0 {
		prefix = id[:i]
	}
	sub, ok := m.subs[prefix]
	if !ok {
		return nil, fmt.Errorf("%w: unknown machine id prefix %q", providerkit.ErrInvalidMachine, id)
	}
	return sub, nil
}

func (m *multiplexBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	sub, err := m.route(req.Machine.ID)
	if err != nil {
		return providerkit.CreateInstanceResult{}, err
	}
	return sub.CreateInstance(ctx, req)
}

func (m *multiplexBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	sub, err := m.route(req.Machine.ID)
	if err != nil {
		return err
	}
	return sub.ConfigureInstance(ctx, req)
}

func (m *multiplexBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	sub, err := m.route(req.Machine.ID)
	if err != nil {
		return err
	}
	return sub.DrainInstance(ctx, req)
}

func (m *multiplexBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	sub, err := m.route(req.Machine.ID)
	if err != nil {
		return err
	}
	return sub.deleteInstance(ctx, req)
}

var (
	_ providerkit.Backend = (*multiplexBackend)(nil)
	_ providerkit.Deleter = (*multiplexBackend)(nil)
)
