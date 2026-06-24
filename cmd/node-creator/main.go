// Command node-creator is the ONLY demo-side glue between BigFleet and a real
// kwok-backed cluster: it turns each Ready UpcomingNode (capacity BigFleet
// provisioned) into a kwok Node, and cascade-deletes the kwok Node when BigFleet
// reclaims it. It also makes that lifecycle LOOK like a real cloud:
//   - a simulated provisioning DWELL (the node stays absent/NotReady for 15-40s
//     before it appears — the feed's "transfer speed simulated" made true),
//   - Allocatable < Capacity (a realistic kube/system reserve),
//   - realistic nodeInfo / addresses / kubelet identity,
//   - graceful reclaim (cordon -> drain dwell -> delete).
// Everything else is real/native: kwok marks Ready + runs pods, the stock
// kube-scheduler binds (priority + native preemption), controllers evict on delete.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	bfv1alpha1 "github.com/intUnderflow/bigfleet/pkg/apis/bigfleet/v1alpha1"
)

const (
	managedLabel = "bigfleet.demo/managed"
	kwokNode     = "kwok.x-k8s.io/node" // marks the node for kwok to manage (Ready + run pods)
)

// per-node lifecycle timing (in-memory; a restart just re-dwells, which is fine).
//   firstReady: when we first saw an UpcomingNode Ready — drives the provisioning dwell.
//   drainAt:    when a reclaimed (cordoned) node may finally be deleted — the drain window.
var (
	firstReady  = map[string]time.Time{}
	drainAt     = map[string]time.Time{}
	dwellMin    time.Duration
	dwellMax    time.Duration
	drainDwell  time.Duration
	warmupUntil time.Time // until this instant, mint nodes with NO dwell (fast initial baseline)
)

func main() {
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to the cluster kubeconfig")
	interval := flag.Duration("interval", 1500*time.Millisecond, "reconcile tick")
	nodePods := flag.String("node-pods", "110", "pods allocatable injected on minted nodes")
	dMin := flag.Duration("dwell-min", 15*time.Second, "min simulated provisioning dwell before a minted node appears Ready")
	dMax := flag.Duration("dwell-max", 40*time.Second, "max simulated provisioning dwell (jittered per node)")
	dDrain := flag.Duration("drain-dwell", 20*time.Second, "simulated drain window: a reclaimed node is cordoned this long before deletion")
	warmup := flag.Duration("warmup", 0, "startup warmup window during which nodes mint with NO dwell, so a fresh session's initial baseline settles fast (the dwell still applies after, for the interactive provisioning moments)")
	flag.Parse()
	dwellMin, dwellMax, drainDwell = *dMin, *dMax, *dDrain
	warmupUntil = time.Now().Add(*warmup)

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		die("kubeconfig", err)
	}
	cfg.QPS, cfg.Burst = 200, 400
	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(bfv1alpha1.AddToScheme(scheme))
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		die("client", err)
	}
	ctx := context.Background()
	fmt.Println("node-creator: started (UpcomingNode -> kwok Node, with provisioning dwell + graceful drain)")
	for {
		if err := tick(ctx, c, *nodePods); err != nil {
			fmt.Fprintln(os.Stderr, "node-creator tick:", err)
		}
		time.Sleep(*interval)
	}
}

// dwellFor returns a per-node provisioning dwell jittered in [dwellMin, dwellMax],
// deterministic from the UpcomingNode name (stable across ticks).
func dwellFor(upName string) time.Duration {
	if dwellMax <= dwellMin {
		return dwellMin
	}
	spanSec := int64((dwellMax - dwellMin) / time.Second)
	if spanSec <= 0 {
		return dwellMin
	}
	return dwellMin + time.Duration(int64(fnv32(upName))%spanSec)*time.Second
}

func tick(ctx context.Context, c ctrlclient.Client, nodePods string) error {
	now := time.Now()
	var upns bfv1alpha1.UpcomingNodeList
	if err := c.List(ctx, &upns); err != nil {
		return fmt.Errorf("list upcomingnodes: %w", err)
	}
	want := map[string]bool{}
	wantCap := map[string]corev1.ResourceList{}   // node -> Capacity (kwok defaults too big; we correct it)
	wantAlloc := map[string]corev1.ResourceList{} // node -> Allocatable (schedulable; = BigFleet's machine profile)
	liveUpn := map[string]bool{}
	for i := range upns.Items {
		u := &upns.Items[i]
		liveUpn[u.Name] = true
		// Mint once the machine is Ready (CONFIGURED). With async out-of-tree
		// providers the terminal CONFIGURED transition completes out-of-band and is
		// learned via reconcile; BigFleet ADR-0057 makes the shard emit the
		// NodeStateUpdate on that reconcile (and resync on operator reconnect), so the
		// UpcomingNode reliably reaches Ready — no need to mint earlier at Registered.
		if u.Status.Phase != bfv1alpha1.UpcomingNodeReady {
			continue
		}
		name := realisticNodeName(u)
		want[name] = true
		// Spec.Resources is the node's ALLOCATABLE (BigFleet's machine profile, and what
		// the kube-scheduler packs against); capacity = allocatable + a realistic reserve.
		allocRes := corev1.ResourceList{}
		for k, v := range u.Spec.Resources {
			allocRes[k] = v
		}
		allocRes[corev1.ResourceEphemeralStorage] = resource.MustParse("95Gi")
		if _, ok := allocRes[corev1.ResourcePods]; !ok {
			allocRes[corev1.ResourcePods] = resource.MustParse(nodePods)
		}
		capRes := addReserve(allocRes)
		wantCap[name] = capRes
		wantAlloc[name] = allocRes

		var existing corev1.Node
		if err := c.Get(ctx, ctrlclient.ObjectKey{Name: name}, &existing); err == nil {
			continue // already minted (re-patched in the nodes loop below)
		}
		// Provisioning dwell: a real cloud node takes tens of seconds to come up. Hold off
		// creating the kwok Node until the dwell elapses — until then the pod stays Pending
		// (the demo shows "provisioning"), making the feed's "transfer speed simulated" true.
		if _, ok := firstReady[u.Name]; !ok {
			firstReady[u.Name] = now
		}
		// Warmup window: a fresh session's INITIAL baseline shouldn't make the visitor wait —
		// mint immediately so the fleet settles fast. The dwell still applies AFTER warmup, for
		// the interactive "watch BigFleet provision" moments where "transfer speed simulated"
		// matters; a user acting during warmup just sees quick provisioning.
		if !now.Before(warmupUntil) && now.Sub(firstReady[u.Name]) < dwellFor(u.Name) {
			continue // past warmup and still within the dwell -> still provisioning
		}
		n := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels(u), Annotations: nodeAnnotations()},
			Spec:       corev1.NodeSpec{Taints: u.Spec.Taints, ProviderID: "kwok://" + name},
		}
		if err := c.Create(ctx, n); err != nil {
			return fmt.Errorf("create node %s: %w", name, err)
		}
		// Capacity > Allocatable + realistic nodeInfo/addresses. kwok then adds the Ready
		// condition and runs pods.
		n.Status = nodeStatus(name, capRes, allocRes)
		_ = c.Status().Update(ctx, n)
		fmt.Printf("node-creator: minted node %s\n", name)
	}
	// GC dwell bookkeeping for UpcomingNodes that are gone.
	for k := range firstReady {
		if !liveUpn[k] {
			delete(firstReady, k)
		}
	}

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[managedLabel] != "true" {
			continue
		}
		if !want[n.Name] {
			// Graceful reclaim: cordon, let pods drain for a window, THEN delete (instead of
			// the old instant hard-delete). The viewer sees a "SchedulingDisabled" draining
			// node before it disappears.
			if _, ok := drainAt[n.Name]; !ok {
				if !n.Spec.Unschedulable {
					_ = c.Patch(ctx, n, ctrlclient.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`)))
				}
				drainAt[n.Name] = now.Add(drainDwell)
				fmt.Printf("node-creator: draining node %s (cordon -> delete in %s)\n", n.Name, drainDwell)
				continue
			}
			if now.Before(drainAt[n.Name]) {
				continue // still draining
			}
			_ = c.Delete(ctx, n)
			delete(drainAt, n.Name)
			fmt.Printf("node-creator: removed node %s (drained)\n", n.Name)
			continue
		}
		// Node is wanted again (re-provisioned mid-drain): cancel the drain + uncordon.
		if _, ok := drainAt[n.Name]; ok {
			delete(drainAt, n.Name)
			if n.Spec.Unschedulable {
				_ = c.Patch(ctx, n, ctrlclient.RawPatch(types.MergePatchType, []byte(`{"spec":{"unschedulable":false}}`)))
			}
		}
		// kwok's node-initialize resets capacity/allocatable/nodeInfo to its defaults
		// (cpu:1k/mem:1Ti, kwok kubelet version), racing our mint. Correct it each tick;
		// kwok only sets it on init, so this sticks.
		if a, ok := wantAlloc[n.Name]; ok && !allocMatches(n, a) {
			_ = c.Status().Patch(ctx, n, ctrlclient.RawPatch(types.MergePatchType, statusPatch(n.Name, wantCap[n.Name], a)))
		}
	}
	return nil
}

func allocMatches(n *corev1.Node, res corev1.ResourceList) bool {
	for k, v := range res {
		have, ok := n.Status.Allocatable[k]
		if !ok || have.Cmp(v) != 0 {
			return false
		}
	}
	return true
}

// nodeStatus builds the full minted-node status: capacity/allocatable + realistic
// nodeInfo / addresses / kubelet endpoint.
func nodeStatus(name string, capRes, allocRes corev1.ResourceList) corev1.NodeStatus {
	cloud := cloudOfName(name)
	os, kernel := osOf(cloud)
	return corev1.NodeStatus{
		Capacity:    capRes,
		Allocatable: allocRes,
		Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: name},
			{Type: corev1.NodeInternalIP, Address: internalIP(name)},
		},
		DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250}},
		NodeInfo: corev1.NodeSystemInfo{
			MachineID:               randHex(name+"m", 32),
			SystemUUID:              uuidFrom(name + "s"),
			BootID:                  uuidFrom(name + "b"),
			KernelVersion:           kernel,
			OSImage:                 os,
			ContainerRuntimeVersion: "containerd://1.7.20",
			KubeletVersion:          "v1.33.0",
			KubeProxyVersion:        "v1.33.0",
			OperatingSystem:         "linux",
			Architecture:            "amd64",
		},
	}
}

// statusPatch is the per-tick correction (capacity/allocatable + identity) applied when
// kwok overwrites our values on node-initialize.
func statusPatch(name string, capRes, allocRes corev1.ResourceList) []byte {
	toMap := func(rl corev1.ResourceList) map[string]string {
		m := map[string]string{}
		for k, v := range rl {
			m[string(k)] = v.String()
		}
		return m
	}
	cloud := cloudOfName(name)
	os, kernel := osOf(cloud)
	b, _ := json.Marshal(map[string]any{"status": map[string]any{
		"capacity":    toMap(capRes),
		"allocatable": toMap(allocRes),
		"addresses": []map[string]string{
			{"type": "Hostname", "address": name},
			{"type": "InternalIP", "address": internalIP(name)},
		},
		"daemonEndpoints": map[string]any{"kubeletEndpoint": map[string]any{"Port": 10250}},
		"nodeInfo": map[string]string{
			"machineID": randHex(name+"m", 32), "systemUUID": uuidFrom(name + "s"), "bootID": uuidFrom(name + "b"),
			"kernelVersion": kernel, "osImage": os, "containerRuntimeVersion": "containerd://1.7.20",
			"kubeletVersion": "v1.33.0", "kubeProxyVersion": "v1.33.0", "operatingSystem": "linux", "architecture": "amd64",
		},
	}})
	return b
}

// addReserve returns Capacity = Allocatable + a realistic kube/system reserve, so a real
// node shows Capacity > Allocatable. Spec.Resources is the ALLOCATABLE (schedulable)
// amount BigFleet provisions against — adding the reserve HERE (not carving it off) keeps
// BigFleet's machine profile and the kube-scheduler's node view in agreement, so the
// engine provisions exactly enough nodes and pods don't stay permanently Pending.
func addReserve(alloc corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	for k, v := range alloc {
		out[k] = v
	}
	add := func(res corev1.ResourceName, amount string) {
		q := out[res].DeepCopy()
		q.Add(resource.MustParse(amount))
		out[res] = q
	}
	add(corev1.ResourceCPU, "500m")                                    // kube-reserved + system-reserved
	add(corev1.ResourceMemory, "4Gi")                                  // kube/system reserved + eviction-hard
	out[corev1.ResourceEphemeralStorage] = resource.MustParse("100Gi") // capacity disk > allocatable 95Gi
	return out
}

// --- cloud-realistic node identity (cosmetics; honesty via the simulated annotation + the panel) ---

func nodeLabels(u *bfv1alpha1.UpcomingNode) map[string]string {
	l := map[string]string{kwokNode: "fake", managedLabel: "true"}
	for k, v := range u.Spec.Labels {
		if k == "topology.bigfleet/rack" {
			continue
		}
		l[k] = v
	}
	it := l["node.kubernetes.io/instance-type"]
	cloud := cloudOf(it)
	l["kubernetes.io/os"] = "linux"
	l["kubernetes.io/arch"] = "amd64"
	l["bigfleet.demo/simulated"] = "true" // grep-able honesty LABEL: kubectl get nodes -l bigfleet.demo/simulated=true
	// Tier (owned | reserved | on-demand | spot) is the PROVIDER's declared
	// CapacityType, carried on the UpcomingNode label — read it verbatim. Only
	// fall back to the SKU guess for a machine the provider didn't label (legacy
	// --seed-machines path). This kills the cosmetic guess: spot vs on-demand is
	// BigFleet's real decision, not an instance-type heuristic.
	if _, ok := l["bigfleet.demo/billing"]; !ok {
		l["bigfleet.demo/billing"] = billingOf(it)
	}
	switch cloud {                             // cloud-plausible nodepool labels
	case "gcp":
		l["cloud.google.com/gke-nodepool"] = "default-pool"
	case "aws":
		l["eks.amazonaws.com/nodegroup"] = "default-ng"
		l["eks.amazonaws.com/capacityType"] = "ON_DEMAND" // node-level capacity type (reserved is billing-only)
	case "onprem":
		l["node-role.kubernetes.io/worker"] = ""
	}
	if _, ok := l["topology.kubernetes.io/zone"]; ok {
		idx := trailingNum(strings.TrimPrefix(u.Name, "un-"))
		l["topology.kubernetes.io/zone"] = realisticZone(cloud, idx)
		l["topology.kubernetes.io/region"] = regionOf(cloud)
	}
	return l
}

func nodeAnnotations() map[string]string {
	return map[string]string{
		"bigfleet.demo/simulated": "true",
		"bigfleet.demo/note":      "kwok-simulated node — the cloud is not real (see the demo's what's-real panel)",
	}
}

func cloudOf(it string) string {
	switch {
	case strings.HasPrefix(it, "bare-metal"), strings.HasPrefix(it, "onprem"):
		return "onprem" // owned bare metal (BareMetal Idle, $0 marginal)
	case strings.HasPrefix(it, "n2-"), strings.HasPrefix(it, "e2-"), strings.HasPrefix(it, "n1-"):
		return "gcp"
	default: // m6i.* (reserved), m5.* (on-demand), c6i.*, … — AWS-style families
		return "aws"
	}
}

// billingOf maps the instance type to its capacity billing model. The demo's COMMITTED
// tier ($0 marginal) is owned bare metal OR AWS RESERVED (m6i); on-demand (m5/n2) is the
// priced cloud burst. Surfaced as a node label so the dashboard shows it — real
// reserved-ness is billing-only and invisible on a node, so this is a disclosed demo
// annotation, kept in agreement with BigFleet's BareMetal-vs-OnDemand seed split.
func billingOf(it string) string {
	switch {
	case strings.HasPrefix(it, "bare-metal"), strings.HasPrefix(it, "onprem"):
		return "owned"
	case strings.HasPrefix(it, "m6i"):
		return "reserved"
	default: // n2-*, m5-*, … on-demand cloud
		return "on-demand"
	}
}

// cloudOfName derives the cloud from the minted node NAME (used post-mint when we only
// have the Node object) — mirrors realisticNodeName's prefixes.
func cloudOfName(name string) string {
	switch {
	case strings.HasPrefix(name, "bm-"):
		return "onprem"
	case strings.HasPrefix(name, "gke-"):
		return "gcp"
	default:
		return "aws"
	}
}

func osOf(cloud string) (osImage, kernel string) {
	switch cloud {
	case "gcp":
		return "Container-Optimized OS from Google", "6.1.85+"
	case "onprem":
		return "Ubuntu 22.04.4 LTS", "5.15.0-107-generic"
	default:
		return "Amazon Linux 2", "5.10.218-208.862.amzn2.x86_64"
	}
}

func internalIP(name string) string {
	h := fnv32(name)
	return fmt.Sprintf("10.%d.%d.%d", (h>>16)&0x3f, (h>>8)&0xff, (h&0x7f)+1)
}

func realisticNodeName(u *bfv1alpha1.UpcomingNode) string {
	id := strings.TrimPrefix(u.Name, "un-")
	h := fnv32(id)
	idx := trailingNum(id)
	switch cloudOf(u.Spec.Labels["node.kubernetes.io/instance-type"]) {
	case "onprem":
		return fmt.Sprintf("bm-dc1-rack%d-%02d", (idx%3)+1, idx) // on-prem datacenter host
	case "gcp":
		return "gke-bigfleet-default-pool-3f7a9c21-" + b36(0x10000+uint32(idx)*1327, 4)
	default:
		return fmt.Sprintf("ip-10-%d-%d-%d.us-east-1.compute.internal", (h>>8)&0x1f, h&0xff, idx&0xff)
	}
}

func realisticZone(cloud string, idx int) string {
	z := (idx / 3) % 3
	switch cloud {
	case "onprem":
		return "dc1-rack-" + string("123"[z])
	case "gcp":
		return "us-central1-" + string("abc"[z])
	default:
		return "us-east-1" + string("abc"[z])
	}
}

func regionOf(cloud string) string {
	switch cloud {
	case "onprem":
		return "dc1" // on-prem datacenter
	case "gcp":
		return "us-central1"
	default:
		return "us-east-1"
	}
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return h
}

func b36(v uint32, min int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if v == 0 {
		return strings.Repeat("0", min)
	}
	var out []byte
	for v > 0 {
		out = append([]byte{digits[v%36]}, out...)
		v /= 36
	}
	for len(out) < min {
		out = append([]byte{'0'}, out...)
	}
	return string(out)
}

// randHex returns n deterministic hex chars from an LCG seeded by the name — for
// realistic-looking machineID/UUIDs (not zero-padded).
func randHex(seed string, n int) string {
	const hexd = "0123456789abcdef"
	h := fnv32(seed)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		h = h*1664525 + 1013904223
		out[i] = hexd[(h>>24)&0xf]
	}
	return string(out)
}

func uuidFrom(seed string) string {
	h := randHex(seed, 32)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func trailingNum(id string) int {
	i := len(id)
	for i > 0 && id[i-1] >= '0' && id[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(id[i:])
	return n
}

func must(err error) {
	if err != nil {
		die("scheme", err)
	}
}
func die(what string, err error) { fmt.Fprintf(os.Stderr, "node-creator: %s: %v\n", what, err); os.Exit(1) }
