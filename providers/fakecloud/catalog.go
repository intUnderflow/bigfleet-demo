// Package fakecloud is the demo's fleet of fake CapacityProviders. Each cloud
// (on-prem / AWS / GCP) is a real providerkit.Backend over a SIMULATED kwok
// substrate: it speaks the genuine six-RPC provider contract and passes the
// conformance suite, but provisions nothing real — the kube Nodes are minted
// separately by cmd/node-creator off the UpcomingNode CR. The providers exist
// here, not in ../bigfleet-providers, precisely because they are fake (the
// demo's hard rule). One multiplexBackend fronts all three so a single shard
// drives one heterogeneous inventory (BareMetal + Reserved + OnDemand + Spot),
// and BigFleet's real EffectiveCost engine routes across it by cost.
//
// Honesty: every price and interruption probability below is an author-chosen
// ILLUSTRATIVE constant, NOT a cloud quote. The routing decision is real; the
// numbers, the substrate, and provisioning latency are simulated.
package fakecloud

import (
	"fmt"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// Every demo machine is an 8 vCPU / 32 GiB node. Allocatable is cpu/memory only
// (node-creator injects the pods resource + the capacity>allocatable reserve);
// this matches what BigFleet provisions against.
var (
	nodeResources   = map[string]string{"cpu": "8", "memory": "32Gi"}
	nodeAllocatable = map[string]string{"cpu": "7500m", "memory": "28Gi"}
)

// Illustrative, author-chosen $/node·hr and interruption probabilities (NOT
// cloud quotes). Committed capacity (owned bare metal + reserved) is $0 at the
// margin — already paid. On-demand is metered. Spot is cheaper but carries a
// real interruption probability, which is exactly what BigFleet's
// EffectiveCost = price + interruptionProbability×penalty trades off.
//
// Crossover (spot stops being the cheaper effective choice) for spot vs
// on-demand is at penalty M* = (priceOnDemand − priceSpot)/(probSpot − probOnDemand).
// With (0.38,0.05) vs (0.12,0.30): M* = 0.26/0.25 ≈ 1.04. So a workload whose
// bucketed interruption penalty exceeds ~$1 routes to on-demand; tolerant work
// (penalty 0) routes to spot. These constants are tunable; see
// docs/research/three-providers-design.md OPEN FORK #1.
const (
	priceReserved = 0.00 // pre-paid, $0 marginal
	priceBareMetal = 0.00
	priceOnDemand = 0.38
	priceSpotAWS  = 0.12
	priceSpotGCP  = 0.10

	probOnDemand = 0.05
	probSpot     = 0.30 // SPOT must declare a real (>0) interruption probability
)

// billing label values (ride to the Node so the demo labels tiers from the
// PROVIDER's declared capacity, not a guessed instance-type prefix).
const (
	billingOwned    = "owned"
	billingReserved = "reserved"
	billingOnDemand = "on-demand"
	billingSpot     = "spot"
)

// tier is one homogeneous slice of a cloud's advertised catalog: count machines
// of a SKU at a given billing/capacity/price/interruption, all in one resting
// state (Idle = committed host that already exists; Speculative = elastic quota
// slot the engine may provision).
type tier struct {
	count        int
	instanceType string
	capacity     providerkit.CapacityType
	price        float64
	interrupt    float64
	state        providerkit.State
	billing      string
}

// Catalog is one fake cloud's advertised substrate. The same shape serves all
// three clouds — they differ only in data. Cloud is also the machine-ID prefix
// the multiplexer routes actuation by.
type Catalog struct {
	Cloud string
	Zones []string
	Tiers []tier
}

// instances expands the catalog into providerkit.Instances with stable,
// cloud-prefixed IDs. Zones round-robin. The simulated + billing labels ride
// every machine through Profile.Labels → UpcomingNode → kwok Node.
func (c Catalog) instances() []providerkit.Instance {
	var out []providerkit.Instance
	n := 0
	for _, t := range c.Tiers {
		for i := 0; i < t.count; i++ {
			id := fmt.Sprintf("%s-%04d", c.Cloud, n)
			zone := ""
			if len(c.Zones) > 0 {
				zone = c.Zones[n%len(c.Zones)]
			}
			inst := providerkit.Instance{
				ID:                      id,
				State:                   t.state,
				InstanceType:            t.instanceType,
				Zone:                    zone,
				CapacityType:            t.capacity,
				PricePerHour:            t.price,
				InterruptionProbability: t.interrupt,
				Resources:               cloneMap(nodeResources),
				Allocatable:             cloneMap(nodeAllocatable),
				Labels: map[string]string{
					// instance-type + zone ride to the kwok Node so node-creator
					// names/classifies it and the demo-backend attributes its cloud;
					// billing is the PROVIDER's declared tier (owned/reserved/on-demand/
					// spot) — the demo reads this instead of guessing from the SKU.
					"node.kubernetes.io/instance-type": t.instanceType,
					"topology.kubernetes.io/zone":      zone,
					"bigfleet.demo/simulated":          "true",
					"bigfleet.demo/billing":            t.billing,
				},
			}
			// Committed (Idle) machines already exist, so they must carry a host
			// (the kit rejects an Idle machine with no host). Speculative slots
			// carry none.
			if t.state == providerkit.StateIdle {
				inst.Host = providerkit.HostRef{Provider: c.Cloud, Ref: id}
			}
			out = append(out, inst)
			n++
		}
	}
	return out
}

// onpremCatalog: the owned bare-metal free pool — committed BareMetal Idle at $0
// marginal, no Speculative quota (you can't conjure more owned racks) and no
// Deleter (a drained box returns to the pool, it isn't torn down).
func onpremCatalog(baremetal int) Catalog {
	return Catalog{
		Cloud: "onprem",
		Zones: []string{"dc1-rack-1", "dc1-rack-2", "dc1-rack-3"},
		Tiers: []tier{
			{count: baremetal, instanceType: "bare-metal-8vcpu", capacity: providerkit.CapacityBareMetal, price: priceBareMetal, interrupt: 0, state: providerkit.StateIdle, billing: billingOwned},
		},
	}
}

// awsCatalog: reserved (committed Idle, $0 marginal) + on-demand + spot
// (elastic Speculative). On-demand and spot share the SKU m5.2xlarge — the
// honest "spot is the same machine, cheaper, interruptible" modeling.
func awsCatalog(reserved, ondemand, spot int) Catalog {
	return Catalog{
		Cloud: "aws",
		Zones: []string{"us-east-1a", "us-east-1b", "us-east-1c"},
		Tiers: []tier{
			{count: reserved, instanceType: "m6i.2xlarge", capacity: providerkit.CapacityReserved, price: priceReserved, interrupt: 0, state: providerkit.StateIdle, billing: billingReserved},
			{count: ondemand, instanceType: "m5.2xlarge", capacity: providerkit.CapacityOnDemand, price: priceOnDemand, interrupt: probOnDemand, state: providerkit.StateSpeculative, billing: billingOnDemand},
			{count: spot, instanceType: "m5.2xlarge", capacity: providerkit.CapacitySpot, price: priceSpotAWS, interrupt: probSpot, state: providerkit.StateSpeculative, billing: billingSpot},
		},
	}
}

// gcpCatalog: on-demand + spot (elastic Speculative), sharing SKU n2-standard-8.
func gcpCatalog(ondemand, spot int) Catalog {
	return Catalog{
		Cloud: "gcp",
		Zones: []string{"us-central1-a", "us-central1-b", "us-central1-c"},
		Tiers: []tier{
			{count: ondemand, instanceType: "n2-standard-8", capacity: providerkit.CapacityOnDemand, price: priceOnDemand, interrupt: probOnDemand, state: providerkit.StateSpeculative, billing: billingOnDemand},
			{count: spot, instanceType: "n2-standard-8", capacity: providerkit.CapacitySpot, price: priceSpotGCP, interrupt: probSpot, state: providerkit.StateSpeculative, billing: billingSpot},
		},
	}
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
