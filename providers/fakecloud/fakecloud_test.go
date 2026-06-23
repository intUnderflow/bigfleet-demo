package fakecloud

import (
	"context"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// TestDemoFleetShape asserts the default multiplexed catalog is the demo's
// 48-committed + 72-cloud fleet and that every machine satisfies the kit's
// field-shape contract (mirrored here so a bad catalog fails fast, before
// conformance).
func TestDemoFleetShape(t *testing.T) {
	m := New(Options{})
	ins, err := m.Describe(context.Background())
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(ins) != 120 {
		t.Fatalf("fleet size = %d, want 120", len(ins))
	}

	byCap := map[providerkit.CapacityType]int{}
	idle, spec := 0, 0
	for _, in := range ins {
		byCap[in.CapacityType]++
		switch in.State {
		case providerkit.StateIdle:
			spec += 0
			idle++
			if in.Host.Ref == "" {
				t.Errorf("%s: Idle machine must carry a host", in.ID)
			}
		case providerkit.StateSpeculative:
			spec++
			if in.Host.Ref != "" {
				t.Errorf("%s: Speculative machine must not carry a host", in.ID)
			}
		default:
			t.Errorf("%s: Describe may only report Idle/Speculative, got %v", in.ID, in.State)
		}
		// kit field-shape invariants (providerkit/backend.go validate)
		if in.ID == "" || in.InstanceType == "" {
			t.Errorf("%q: id and instance_type required", in.ID)
		}
		if in.CapacityType == providerkit.CapacityUnspecified {
			t.Errorf("%s: capacity_type required", in.ID)
		}
		if in.PricePerHour < 0 {
			t.Errorf("%s: price %v must be >= 0", in.ID, in.PricePerHour)
		}
		if in.InterruptionProbability < 0 || in.InterruptionProbability > 1 {
			t.Errorf("%s: interruption_probability %v out of [0,1]", in.ID, in.InterruptionProbability)
		}
		if in.CapacityType == providerkit.CapacitySpot && in.InterruptionProbability <= 0 {
			t.Errorf("%s: SPOT must declare interruption_probability > 0", in.ID)
		}
		if in.Labels["bigfleet.demo/simulated"] != "true" {
			t.Errorf("%s: missing simulated honesty label", in.ID)
		}
	}

	// committed = BareMetal(28) + Reserved(20) = 48, all Idle.
	committed := byCap[providerkit.CapacityBareMetal] + byCap[providerkit.CapacityReserved]
	if committed != 48 || idle != 48 {
		t.Errorf("committed=%d idle=%d, want 48/48", committed, idle)
	}
	// cloud = OnDemand(48) + Spot(24) = 72, all Speculative.
	cloud := byCap[providerkit.CapacityOnDemand] + byCap[providerkit.CapacitySpot]
	if cloud != 72 || spec != 72 {
		t.Errorf("cloud=%d speculative=%d, want 72/72", cloud, spec)
	}
	if byCap[providerkit.CapacitySpot] != 24 {
		t.Errorf("spot=%d, want 24", byCap[providerkit.CapacitySpot])
	}
}

// TestKitAcceptsSeed proves the kit constructs over the multiplexer without a
// seed-validation error (the real, deep validation is the conformance suite).
func TestKitAcceptsSeed(t *testing.T) {
	_, err := providerkit.New(New(Options{}), providerkit.NewMemStore(), providerkit.Options{RequireZone: true})
	if err != nil {
		t.Fatalf("providerkit.New rejected the seed: %v", err)
	}
}

// TestMultiplexRouting proves actuator calls route to the right cloud by ID
// prefix, and an unknown prefix errors rather than mis-routing.
func TestMultiplexRouting(t *testing.T) {
	m := New(Options{})
	for _, tc := range []struct{ id, cloud string }{
		{"onprem-0000", "onprem"}, {"aws-0050", "aws"}, {"gcp-0010", "gcp"},
	} {
		res, err := m.CreateInstance(context.Background(), providerkit.CreateInstanceRequest{
			Machine: providerkit.Machine{ID: tc.id},
		})
		if err != nil {
			t.Fatalf("create %s: %v", tc.id, err)
		}
		if res.Host.Provider != tc.cloud {
			t.Errorf("create %s routed to provider %q, want %q", tc.id, res.Host.Provider, tc.cloud)
		}
	}
	if _, err := m.CreateInstance(context.Background(), providerkit.CreateInstanceRequest{
		Machine: providerkit.Machine{ID: "zzz-1"},
	}); err == nil {
		t.Error("create with unknown prefix should error, got nil")
	}
}
