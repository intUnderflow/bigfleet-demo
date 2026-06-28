package fakecloud

import (
	"context"
	"time"
)

// newDwell returns a ctx-honoring sleep of d — the simulated provisioning
// latency a real cloud would take to boot an instance. It MUST be shorter than
// the kit's Create timeout (default 5m) or the machine lands in FAILED. d<=0
// returns nil (instant create); the demo's visible "transfer speed simulated"
// dwell normally lives in node-creator (the kwok Node appears late), so the
// provider defaults to instant.
func newDwell(d time.Duration) func(context.Context) {
	if d <= 0 {
		return nil
	}
	return func(ctx context.Context) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}
