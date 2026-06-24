// Command demo-backend is the demo control plane and the ONLY surface a
// viewer touches (never the apiserver). It loads run/session.json, tails the
// BigFleet shard's real /metrics + each workspace's nodes/UpcomingNodes/pods,
// streams a derived FleetState to the browser over SSE, synthesizes an honest
// Decision Feed, and mints validated workloads server-side.
//
// Honesty: everything it ships is DERIVED state (counts + synthesized lines) —
// never a raw kubeconfig, port, or CRD. The cloud/nodes are simulated; the
// shard's decisions (actions, inventory) are real and labelled as such.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	bfv1alpha1 "github.com/intUnderflow/bigfleet/pkg/apis/bigfleet/v1alpha1"
)

type session struct {
	ShardMetrics string `json:"shardMetrics"`
	Workspaces   []struct {
		Name       string `json:"name"`
		Kubeconfig string `json:"kubeconfig"`
		Dashboard  string `json:"dashboard"`
	} `json:"workspaces"`
}

type clusterState struct {
	Name         string         `json:"name"`
	Nodes        int            `json:"nodes"`
	NodesReady   int            `json:"nodesReady"`
	NodesByCloud map[string]int `json:"nodesByCloud"` // READY nodes attributed to a cloud (GCP/AWS/Azure) via the instance-type label
	Provisioning int            `json:"provisioning"` // UpcomingNodes not yet Ready
	PodsRunning  int            `json:"podsRunning"`
	PodsPending  int            `json:"podsPending"`
	BatchPending int            `json:"batchPending"` // pending low-priority BATCH pods — the preemption victim under scarcity (§4)
	Demand       int            `json:"demand"`    // standard demand-tier pods (user-controlled)
	Critical     int            `json:"critical"`  // critical demand-tier pods (section 8)
	Baseline     int            `json:"baseline"`  // baseline-tier pods (pre-loaded)
	Dashboard    string         `json:"dashboard"` // URL of this cluster's Kubernetes dashboard
}

type fleetState struct {
	Clusters     []clusterState `json:"clusters"`
	Shard        map[string]int `json:"shard"`        // bootstrap/provision/reclaim/preempt action counters (for the feed)
	FleetByCloud map[string]int `json:"fleetByCloud"` // READY nodes per provider across the whole fleet (On-prem/GCP/AWS)
	Clouds       []string       `json:"clouds"`       // stable display order of the providers in the fleet
	FleetSize    int            `json:"fleetSize"`    // total machines in the finite fleet (hard cap = owned + cloud quota)
	Capacity     capacity       `json:"capacity"`     // the two-tier owned/cloud split + illustrative cost
	Scenario     string         `json:"scenario"`     // running teaching scenario (move|saturate|critical), "" if none
	Feed         []string       `json:"feed"`
	TS           string         `json:"ts"`
}

// capacity is the honest two-tier view: COMMITTED ($0 marginal — owned on-prem bare
// metal AND AWS reserved) vs elastic ON-DEMAND cloud (provisioned-on-demand, billed
// only while running; the rest is $0 available headroom, NOT idle-you-pay-for). The
// tier is decided by BILLING (owned/reserved vs on-demand), not by provider — AWS
// appears in BOTH (reserved in committed, on-demand in cloud).
type capacity struct {
	OnpremTotal         int            `json:"onpremTotal"`         // committed pool size (fixed: owned + reserved)
	OnpremInUse         int            `json:"onpremInUse"`         // committed nodes carrying workload
	OnpremIdle          int            `json:"onpremIdle"`          // committed nodes idle ($0 — already paid; genuinely idle)
	CommittedByProvider map[string]int `json:"committedByProvider"` // committed nodes in use per provider (On-prem owned / AWS reserved)
	CommittedByType     map[string]int `json:"committedByType"`     // committed nodes in use per SKU (bare-metal-8vcpu / m6i.2xlarge)
	CloudTotal          int            `json:"cloudTotal"`          // on-demand quota ceiling (max provisionable)
	CloudInUse          int            `json:"cloudInUse"`          // on-demand nodes provisioned & billed right now
	CloudAvailable      int            `json:"cloudAvailable"`      // on-demand quota not provisioned ($0 until used)
	CloudByCloud        map[string]int `json:"cloudByCloud"`        // provisioned cloud nodes per provider (GCP/AWS)
	CloudByType         map[string]int `json:"cloudByType"`         // provisioned ON-DEMAND nodes per SKU (n2-standard-8 / m5.2xlarge)
	OnDemandInUse       int            `json:"onDemandInUse"`       // cloud nodes on ON-DEMAND capacity (metered, stable)
	SpotInUse           int            `json:"spotInUse"`           // cloud nodes on SPOT capacity (cheaper, interruptible — BigFleet's real cost choice)
	SpotByType          map[string]int `json:"spotByType"`          // provisioned SPOT nodes per SKU
	CostPerHour         float64        `json:"costPerHour"`         // illustrative: onDemand×nodeHourly + spot×spotHourly (committed = $0)
	NodeHourly          float64        `json:"nodeHourly"`          // illustrative per-on-demand-node $/hr (for the caption)
	SpotHourly          float64        `json:"spotHourly"`          // illustrative per-spot-node $/hr
}

// cloudOrder is the stable display order; cloudOf maps a node's
// node.kubernetes.io/instance-type label to its (simulated) provider. The
// instance types come from scenarios/demo-fleet.yaml. "On-prem" is owned
// bare metal (BareMetal Idle, $0 marginal); GCP/AWS are elastic OnDemand
// cloud burst. These identities are author-chosen LABELS on kwok fakes —
// not real clouds.
var cloudOrder = []string{"On-prem", "GCP", "AWS"}

// cloudProviders is the elastic (pay-per-use) subset — everything except On-prem.
var cloudProviders = []string{"GCP", "AWS"}

func cloudOf(instanceType string) string {
	switch {
	case strings.HasPrefix(instanceType, "bare-metal"), strings.HasPrefix(instanceType, "onprem"):
		return "On-prem"
	case strings.HasPrefix(instanceType, "n2-"), strings.HasPrefix(instanceType, "e2-"), strings.HasPrefix(instanceType, "n1-"):
		return "GCP"
	default: // m6i.* (reserved), m5.* (on-demand), c6i.*, … — AWS-style families
		return "AWS"
	}
}

// billingOf decides the TIER from the instance type (must match node-creator's rule):
// committed/$0-marginal = owned bare metal OR AWS reserved (m6i); on-demand = priced
// cloud burst (n2/m5). This is how AWS lands in BOTH tiers without confusing them.
func billingOf(instanceType string) string {
	switch {
	case strings.HasPrefix(instanceType, "bare-metal"), strings.HasPrefix(instanceType, "onprem"):
		return "owned"
	case strings.HasPrefix(instanceType, "m6i"):
		return "reserved"
	default: // n2-*, m5-*, … on-demand cloud
		return "on-demand"
	}
}

func isCommitted(instanceType string) bool { return billingOf(instanceType) != "on-demand" }

// nodeTier returns a ready node's billing tier: owned | reserved | on-demand | spot.
// It prefers the PROVIDER-declared bigfleet.demo/billing label (BigFleet's real
// CapacityType, set by the fakecloud provider), falling back to the SKU heuristic
// only for an unlabeled legacy node. owned/reserved are committed ($0 marginal);
// on-demand is metered; spot is cheaper-but-interruptible — which tier a workload
// lands on is BigFleet's real EffectiveCost decision, not a guess.
func nodeTier(n *corev1.Node) string {
	if b := n.Labels["bigfleet.demo/billing"]; b != "" {
		return b
	}
	return billingOf(n.Labels["node.kubernetes.io/instance-type"])
}

type cluster struct {
	name      string
	dashboard string
	c         ctrlclient.Client
}

var (
	mu             sync.Mutex
	feed           []string
	prev           = map[string]int{}
	prevNodes      = map[string]int{}
	prevBatch      = map[string]int{} // per-cluster batch POD count (for scheduler-preemption detection)
	feedPrimed     bool               // first poll seeds prev/prevNodes silently (avoids diffing cumulative counters against 0 on backend restart)
)

type dashMount struct{ prefix, target, token string }

// dashHandler fronts one cluster's Kubernetes Dashboard, mounted on the MAIN
// server under a path prefix (e.g. /dash/a). It (1) strips the prefix so the
// dashboard sees root paths, (2) injects the workspace's bearer token so the
// dashboard skips its login page (headerPresent:true), and (3) injects
// `<base href="/dash/a/">` into the served HTML so the SPA's RELATIVE asset and
// api/v1 paths resolve under the prefix. Same-origin path mount => the button is
// a relative link that works locally and over the tunnel, and only :8090 is ever
// exposed (no separate dashboard ports, no extra hostnames).
func dashHandler(prefix, target, token string) http.Handler {
	u, err := url.Parse(target)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad dashboard target: "+err.Error(), http.StatusInternalServerError)
		})
	}
	baseHref := []byte(`<head><base href="` + prefix + `/">`)
	rp := httputil.NewSingleHostReverseProxy(u)
	director := rp.Director
	rp.Director = func(req *http.Request) {
		director(req)
		req.Host = u.Host
		req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		req.Header.Set("Accept-Encoding", "identity") // so ModifyResponse can rewrite the HTML
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			return nil
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		b = bytes.Replace(b, []byte("<head>"), baseHref, 1) // pin all relative URLs under the prefix
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.Header.Del("Content-Encoding")
		resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
		resp.ContentLength = int64(len(b))
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "dashboard not reachable: "+e.Error(), http.StatusBadGateway)
	}
	return rp
}

// sessionClock bounds a HOSTED demo session: a hard wall-clock deadline (default 1h)
// and an idle timeout (default 5m) measured from the last browser heartbeat. The UI
// reads /api/session to render the top-bar countdown and POSTs /api/heartbeat while
// its tab is open; the demohost reaper polls /api/session and tears the session down
// when it reports expired (and enforces the hard cap itself too, as a backstop). The
// standalone local demo passes no --session-id, so clock is nil and there is no bar.
type sessionClock struct {
	id           string
	ttl          time.Duration
	idleTimeout  time.Duration
	mu           sync.Mutex
	startedAt    time.Time
	hardDeadline time.Time
	lastBeat     time.Time
	begun        bool
}

func newSessionClock(id string, ttl, idle time.Duration, now time.Time) *sessionClock {
	return &sessionClock{id: id, ttl: ttl, startedAt: now, hardDeadline: now.Add(ttl), idleTimeout: idle, lastBeat: now}
}

func (s *sessionClock) beat(now time.Time) { s.mu.Lock(); s.lastBeat = now; s.mu.Unlock() }

// begin (re)starts the visitor-facing clock at hand-out. A warm-pool session's backend boots
// minutes before a visitor is assigned it, so the hard deadline (and idle timer) must restart
// from the moment it's claimed — otherwise the top-bar countdown is already partly spent.
// Idempotent: only the FIRST call (demohost's server-side hand-out) takes effect, so the
// proxied /api/begin can't be replayed by the visitor's browser to extend the session.
func (s *sessionClock) begin(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.begun {
		return
	}
	s.begun = true
	s.startedAt = now
	s.hardDeadline = now.Add(s.ttl)
	s.lastBeat = now
}

type sessionStatus struct {
	Hosted           bool   `json:"hosted"`
	ID               string `json:"id"`
	StartedAt        string `json:"startedAt"`
	HardDeadline     string `json:"hardDeadline"`
	IdleDeadline     string `json:"idleDeadline"`
	ExpiresAt        string `json:"expiresAt"`     // whichever of the two comes first
	ExpiresReason    string `json:"expiresReason"` // "scheduled" (1h cap) | "idle" (tab gone)
	Now              string `json:"now"`
	RemainingSeconds int    `json:"remainingSeconds"`
	Expired          bool   `json:"expired"`
}

func (s *sessionClock) snapshot(now time.Time) sessionStatus {
	s.mu.Lock()
	last := s.lastBeat
	started := s.startedAt
	hard := s.hardDeadline
	s.mu.Unlock()
	idleDeadline := last.Add(s.idleTimeout)
	exp, reason := hard, "scheduled"
	if idleDeadline.Before(exp) {
		exp, reason = idleDeadline, "idle"
	}
	rem := int(exp.Sub(now).Seconds())
	if rem < 0 {
		rem = 0
	}
	return sessionStatus{
		Hosted: true, ID: s.id,
		StartedAt:    started.UTC().Format(time.RFC3339),
		HardDeadline: hard.UTC().Format(time.RFC3339),
		IdleDeadline: idleDeadline.UTC().Format(time.RFC3339),
		ExpiresAt:    exp.UTC().Format(time.RFC3339), ExpiresReason: reason,
		Now: now.UTC().Format(time.RFC3339), RemainingSeconds: rem,
		Expired: now.After(hard) || now.After(idleDeadline),
	}
}

func sessionHandler(clock *sessionClock) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if clock == nil { // standalone local demo — no session clock, no top bar
			_ = json.NewEncoder(w).Encode(sessionStatus{Hosted: false})
			return
		}
		_ = json.NewEncoder(w).Encode(clock.snapshot(time.Now()))
	}
}

func heartbeatHandler(clock *sessionClock) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if clock != nil {
			clock.beat(time.Now())
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// beginHandler is called once by demohost when a (possibly long-warmed) session is handed to
// a visitor — it starts the visitor clock from now. Idempotent on the clock, so it's safe even
// though it's reachable through the public /s/{id} proxy.
func beginHandler(clock *sessionClock) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if clock != nil {
			clock.begin(time.Now())
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func main() {
	sessionPath := flag.String("session", "run/session.json", "path to the session descriptor")
	addr := flag.String("addr", ":8090", "listen address")
	uiDir := flag.String("ui", "ui/dist", "static UI dir (falls back to ui/)")
	baseline := flag.Int("baseline", 6, "baseline (pre-loaded, low-priority batch) demand level per cluster — packed onto committed at rest, sized with margin under the committed ceiling so the busy fleet never spills to billed cloud (stays $0)")
	donorDemand := flag.Int("donor-demand", 16, "standing production demand on cluster-a at rest (node-equivalents) — the busy-but-stable workload the cross-cluster 'move' reclaims; sized to stay within committed ($0)")
	fleetSize := flag.Int("fleet-size", 120, "total machines in the finite fleet (= owned on-prem + cloud quota)")
	onpremSize := flag.Int("onprem-size", 48, "owned on-prem bare-metal pool size (match the shard's --seed-machines)")
	cloudSize := flag.Int("cloud-size", 72, "elastic cloud quota ceiling (match the shard's --seed-speculative)")
	cloudHourly := flag.Float64("cloud-node-hourly", 0.38, "illustrative $/hr per PROVISIONED 8vCPU/32GiB on-demand cloud node — author-chosen constant, NOT a cloud quote")
	spotHourly := flag.Float64("spot-node-hourly", 0.11, "illustrative $/hr per PROVISIONED spot cloud node (cheaper, interruptible) — author-chosen constant, NOT a cloud quote")
	nodeBudget := flag.Int("node-budget-cpu", 7000, "millicores of pod requests one demand 'level' unit maps to (~one 8-core node's schedulable cpu); demand L ≈ L nodes' worth, which BigFleet packs onto ~L machines")
	sessionID := flag.String("session-id", "", "hosted-session id (set by demohost); empty = standalone local demo with no session clock / top bar")
	sessionTTL := flag.Duration("session-ttl", time.Hour, "hosted session hard wall-clock lifetime")
	idleTimeout := flag.Duration("idle-timeout", 5*time.Minute, "hosted session tear-down delay after the browser tab stops sending heartbeats")
	flag.Parse()
	nodeBudgetMilli = *nodeBudget

	var clock *sessionClock
	if *sessionID != "" {
		clock = newSessionClock(*sessionID, *sessionTTL, *idleTimeout, time.Now())
		fmt.Printf("demo-backend: hosted session %q — ttl=%s idle=%s\n", *sessionID, *sessionTTL, *idleTimeout)
	}

	sess := loadSession(*sessionPath)
	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(bfv1alpha1.AddToScheme(scheme))

	var clusters []cluster
	var dashMounts []dashMount
	for _, w := range sess.Workspaces {
		cfg, err := clientcmd.BuildConfigFromFlags("", w.Kubeconfig)
		if err != nil {
			die("kubeconfig "+w.Name, err)
		}
		cfg.QPS, cfg.Burst = 100, 200
		c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
		if err != nil {
			die("client "+w.Name, err)
		}
		// Front the official K8s dashboard with a token-injecting reverse proxy mounted
		// on THIS server under /dash/<k> (k = a/b/c). The dashboard container stays bound
		// to localhost; only :8090 (validated, token-injecting) is ever exposed — so the
		// button is a same-origin relative link that works locally and over the tunnel.
		// Honesty/security: never the raw apiserver.
		dashURL := ""
		if w.Dashboard != "" {
			k := strings.TrimPrefix(w.Name, "cluster-")
			prefix := "/dash/" + k
			// kwokctl kubeconfigs use client-certs (no bearer token), so the dashboard
			// proxy injects a ServiceAccount token minted by hack/demo-dashboards.sh.
			token := cfg.BearerToken
			if token == "" {
				if b, err := os.ReadFile(strings.TrimSuffix(w.Kubeconfig, ".kubeconfig") + ".dashtoken"); err == nil {
					token = strings.TrimSpace(string(b))
				}
			}
			dashMounts = append(dashMounts, dashMount{prefix: prefix, target: w.Dashboard, token: token})
			// Deep-link to the Nodes view (the capacity BigFleet manages); relative so it
			// resolves against whatever origin the page is served from.
			dashURL = prefix + "/#/node"
			fmt.Printf("demo-backend: %s dashboard -> %s\n", w.Name, dashURL)
		}
		clusters = append(clusters, cluster{name: w.Name, c: c, dashboard: dashURL})
	}

	fmt.Printf("demo-backend: opening BUSY — baseline %d/cluster + cluster-a donor demand %d, all on committed ($0)\n", *baseline, *donorDemand)
	seedBusyFleet(clusters, *baseline, *donorDemand)

	hub := newHub()
	go pollLoop(sess, clusters, hub, *fleetSize, capConfig{onprem: *onpremSize, cloud: *cloudSize, nodeHourly: *cloudHourly, spotHourly: *spotHourly})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", hub.serveSSE)
	mux.HandleFunc("/api/session", sessionHandler(clock))
	mux.HandleFunc("/api/heartbeat", heartbeatHandler(clock))
	mux.HandleFunc("/api/begin", beginHandler(clock))
	mux.HandleFunc("/api/demand", limited(demandHandler(clusters)))
	mux.HandleFunc("/api/reset", limited(resetHandler(clusters, *baseline, *donorDemand)))
	mux.HandleFunc("/api/scenario", limited(scenarioHandler(clusters)))
	// snapshot of the latest fleet state (same data as the SSE) — used by demohost to poll a
	// warming pooled session until its busy baseline has converged before handing it to a visitor.
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		latestMu.Lock()
		st := latest
		latestMu.Unlock()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	for _, dm := range dashMounts {
		mux.Handle(dm.prefix+"/", dashHandler(dm.prefix, dm.target, dm.token)) // ServeMux redirects /dash/a -> /dash/a/
	}
	dir := *uiDir
	if _, err := os.Stat(dir); err != nil {
		dir = "ui"
	}
	mux.Handle("/", http.FileServer(http.Dir(dir)))

	fmt.Printf("demo-backend: http://localhost%s  (ui=%s, clusters=%d)\n", *addr, dir, len(clusters))
	// Hardening: bound header-read + idle time and header size so an anonymous visitor
	// (reaching us through the demohost /s/{id} proxy) can't tie up goroutines with slow
	// or oversized requests. NO ReadTimeout/WriteTimeout — those would kill the long-lived
	// SSE stream (/api/stream); MaxBytesReader caps request BODIES per-handler instead.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64 KiB
	}
	if err := srv.ListenAndServe(); err != nil {
		die("listen", err)
	}
}

// capConfig is the two-tier sizing/pricing (mirrors the shard's seed flags).
type capConfig struct {
	onprem     int     // owned bare-metal pool (provider committed BareMetal)
	cloud      int     // elastic cloud quota (provider OnDemand + Spot Speculative)
	nodeHourly float64 // illustrative $/hr per provisioned on-demand cloud node
	spotHourly float64 // illustrative $/hr per provisioned spot cloud node (cheaper)
}

func pollLoop(sess session, clusters []cluster, hub *hub, fleetSize int, cfg capConfig) {
	ctx := context.Background()
	for {
		st := fleetState{Shard: scrapeShard(sess.ShardMetrics), TS: time.Now().Format("15:04:05"),
			FleetByCloud: map[string]int{}, Clouds: cloudOrder, FleetSize: fleetSize}
		committedReady, cloudReady := 0, 0                                 // tier split by BILLING, not provider
		onDemandReady, spotReady := 0, 0                                   // cloud sub-split: on-demand (metered) vs spot (cheap, interruptible)
		committedByProv, cloudByProv := map[string]int{}, map[string]int{} // per-provider
		committedByType, cloudByType := map[string]int{}, map[string]int{} // per-SKU, for the supply bar segments
		spotByType := map[string]int{}                                     // per-SKU SPOT (same SKU as on-demand, separate tier)
		for _, cl := range clusters {
			cs := clusterState{Name: cl.name, Dashboard: cl.dashboard, NodesByCloud: map[string]int{}}
			var nodes corev1.NodeList
			if cl.c.List(ctx, &nodes) == nil {
				cs.Nodes = len(nodes.Items)
				for i := range nodes.Items {
					if nodeReady(&nodes.Items[i]) {
						cs.NodesReady++
						it := nodes.Items[i].Labels["node.kubernetes.io/instance-type"]
						prov := cloudOf(it)
						cs.NodesByCloud[prov]++
						st.FleetByCloud[prov]++
						switch nodeTier(&nodes.Items[i]) {
						case "owned", "reserved": // committed tier ($0 marginal)
							committedReady++
							committedByProv[prov]++
							committedByType[it]++
						case "spot": // cloud, but cheaper + interruptible — BigFleet's real cost choice
							cloudReady++
							spotReady++
							cloudByProv[prov]++
							spotByType[it]++
						default: // on-demand cloud (metered, stable)
							cloudReady++
							onDemandReady++
							cloudByProv[prov]++
							cloudByType[it]++
						}
					}
				}
			}
			var upns bfv1alpha1.UpcomingNodeList
			if cl.c.List(ctx, &upns) == nil {
				for i := range upns.Items {
					if upns.Items[i].Status.Phase != bfv1alpha1.UpcomingNodeReady {
						cs.Provisioning++
					}
				}
			}
			var pods corev1.PodList
			if cl.c.List(ctx, &pods) == nil {
				for i := range pods.Items {
					p := &pods.Items[i]
					if p.Spec.NodeName != "" {
						cs.PodsRunning++
					} else {
						cs.PodsPending++
						if p.Labels["tier"] == "baseline" {
							cs.BatchPending++ // displaced batch waiting for capacity — the §4 preemption victim
						}
					}
					if p.Labels["tier"] == "baseline" {
						cs.Baseline++ // pod count (internal); demand/critical show LEVEL, set below
					}
				}
			}
			// demand/critical show the LEVEL the visitor set (0-40), not the (bundle-inflated)
			// pod count — so the +/- control reads the right value.
			ensureMu.Lock()
			cs.Demand = demandLevel[cl.name]["demand"]
			cs.Critical = demandLevel[cl.name]["critical"]
			ensureMu.Unlock()
			// (Preempted pods are recreated natively by their ReplicaSet — no hack.)
			st.Clusters = append(st.Clusters, cs)
		}
		// Two-tier capacity split by BILLING (committed = owned on-prem + AWS reserved,
		// all $0 marginal; cloud = on-demand n2/m5, billed only while running). Computed
		// from the REAL ready-node attribution, not the shard's Idle gauge (which
		// double-counts draining machines under churn). Cloud's unprovisioned remainder
		// is $0 available headroom, NOT idle-you-pay-for.
		cp := capacity{OnpremTotal: cfg.onprem, CloudTotal: cfg.cloud, NodeHourly: cfg.nodeHourly, SpotHourly: cfg.spotHourly,
			CloudByCloud: cloudByProv, CommittedByProvider: committedByProv,
			CommittedByType: committedByType, CloudByType: cloudByType, SpotByType: spotByType}
		cp.OnpremInUse = clamp(committedReady, 0, cfg.onprem)
		cp.CloudInUse = clamp(cloudReady, 0, cfg.cloud)
		cp.OnDemandInUse = onDemandReady
		cp.SpotInUse = spotReady
		cp.OnpremIdle = cfg.onprem - cp.OnpremInUse
		cp.CloudAvailable = cfg.cloud - cp.CloudInUse
		// Illustrative metered spend: on-demand at the full rate, spot at the
		// cheaper rate — so routing batch onto spot visibly LOWERS $/hr vs the
		// same nodes on on-demand. Committed = $0 marginal.
		cp.CostPerHour = float64(onDemandReady)*cfg.nodeHourly + float64(spotReady)*cfg.spotHourly
		st.Capacity = cp
		scenMu.Lock()
		st.Scenario = curScenario
		scenMu.Unlock()
		setLatest(st)
		synthesizeFeed(&st)
		hub.broadcast(st)
		time.Sleep(1200 * time.Millisecond)
	}
}

// synthesizeFeed appends honest human-readable lines from real deltas.
func synthesizeFeed(st *fleetState) {
	mu.Lock()
	defer mu.Unlock()
	add := func(s string) {
		feed = append([]string{st.TS + "  " + s}, feed...)
		if len(feed) > 40 {
			feed = feed[:40]
		}
	}
	if !feedPrimed { // first poll: seed baselines (cumulative counters would otherwise read as one giant delta)
		feedPrimed = true
		for k, v := range st.Shard {
			prev[k] = v
		}
		prev["owned"] = st.Capacity.OnpremInUse
		prev["cloud"] = st.Capacity.CloudInUse
		prev["spot"] = st.Capacity.SpotInUse
		prev["ondemand"] = st.Capacity.OnDemandInUse
		for _, cs := range st.Clusters {
			prevNodes[cs.Name] = cs.Nodes
		}
		// Seed a friendly at-rest line so the feed isn't blank/frozen on arrival.
		feed = []string{st.TS + "  Fleet running on committed capacity — batch baseline across the fleet plus cluster-a's production workload; on-demand cloud is $0.00/hr. Press a button on the left to move capacity across the fleet."}
		st.Feed = append([]string(nil), feed...)
		return
	}
	// Capacity-type-aware lines (the cost story), driven by observed owned/cloud
	// node deltas: owned bare metal fills first at $0; cloud bursts only when
	// owned is exhausted (and is shed first on the way down).
	if o, p := st.Capacity.OnpremInUse, prev["owned"]; o > p {
		add(fmt.Sprintf("BigFleet placed %d node(s) on committed capacity (owned on-prem or reserved cloud) — $0 marginal  (real decision; transfer speed simulated)", o-p))
	}
	// Cloud growth splits by the engine's REAL effective-cost routing:
	// interruption-tolerant work lands on cheap SPOT, sensitive work on stable
	// ON-DEMAND (BigFleet reasons price + interruptionProbability×penalty).
	if s, p := st.Capacity.SpotInUse, prev["spot"]; s > p {
		add(fmt.Sprintf("⚡ BigFleet placed %d interruption-tolerant node(s) on cheap SPOT (~$%.2f/node·hr) — effective-cost routing preferred spot over on-demand  (real decision; transfer speed simulated)", s-p, st.Capacity.SpotHourly))
	}
	if d, p := st.Capacity.OnDemandInUse, prev["ondemand"]; d > p {
		add(fmt.Sprintf("☁ BigFleet placed %d node(s) on stable ON-DEMAND (~$%.2f/node·hr) — interruption-sensitive demand (spot's risk outweighs the saving) or tolerant overflow once cheap spot is full; ~$%.2f/hr total (illustrative)  (real decision; transfer speed simulated)", d-p, st.Capacity.NodeHourly, st.Capacity.CostPerHour))
	}
	if c, p := st.Capacity.CloudInUse, prev["cloud"]; c < p {
		add(fmt.Sprintf("BigFleet released %d cloud node(s) back to $0 — stopped paying for capacity it no longer needs  (real decision; transfer speed simulated)", p-c))
	}
	if o, p := st.Capacity.OnpremInUse, prev["owned"]; o < p {
		add(fmt.Sprintf("BigFleet reclaimed %d committed node(s) to the idle pool — ready for the next cluster to draw  (real decision; transfer speed simulated)", p-o))
	}
	if pr, p := st.Shard["preempt"], prev["preempt"]; pr > p {
		add(fmt.Sprintf("BigFleet drained %d batch-occupied node(s) (Phase-2) to free committed capacity for critical demand  (real decision; transfer speed simulated)", pr-p))
	}
	for k, v := range st.Shard {
		prev[k] = v
	}
	prev["owned"] = st.Capacity.OnpremInUse
	prev["cloud"] = st.Capacity.CloudInUse
	prev["spot"] = st.Capacity.SpotInUse
	prev["ondemand"] = st.Capacity.OnDemandInUse
	// Scheduler-level preemption: batch PODS evicted on a cluster running critical demand.
	// (BigFleet reclaims/reassigns NODES; the stock kube-scheduler preempts PODS — distinct.)
	for _, cs := range st.Clusters {
		if cs.Critical > 0 {
			if drop := prevBatch[cs.Name] - cs.Baseline; drop >= 4 {
				add(fmt.Sprintf("⚡ kube-scheduler preempted ~%d batch pod(s) on %s to place critical demand — the finite fleet has no room, so batch waits  (real native preemption; transfer speed simulated)", drop, cs.Name))
			}
		}
		prevBatch[cs.Name] = cs.Baseline
	}
	// Per-cluster node deltas (de-noised: skip ±1 churn). A cluster that GREW while the fleet
	// committed+cloud totals stayed flat drew REUSED capacity from the shared Idle hub (a
	// cross-cluster move) — not new provisioning.
	// Per-cluster node deltas, de-noised (skip ±1 churn). Cross-cluster MOVE is narrated by
	// the scenario itself (it KNOWS it dropped A and spiked B) rather than guessed from
	// coincidental deltas; reuse-from-hub already shows as the "placed on committed ($0)" line.
	for _, cs := range st.Clusters {
		if p := prevNodes[cs.Name]; p != 0 {
			if d := cs.Nodes - p; d >= 2 || d <= -2 {
				verb := "grew"
				if d < 0 {
					verb = "shrank"
					d = -d
				}
				add(fmt.Sprintf("%s %s by %d node(s) -> %d", cs.Name, verb, d, cs.Nodes))
			}
		}
		prevNodes[cs.Name] = cs.Nodes
	}
	st.Feed = append([]string(nil), feed...)
}

var (
	reAction = regexp.MustCompile(`bigfleet_shard_actions_total\{[^}]*kind="([A-Za-z]+)"[^}]*\}\s+([0-9.]+)`)
	reInv    = regexp.MustCompile(`bigfleet_shard_inventory_machines\{[^}]*state="([A-Za-z]+)"[^}]*\}\s+([0-9.]+)`)
)

func scrapeShard(addr string) map[string]int {
	out := map[string]int{"idle": 0, "configured": 0, "speculative": 0, "bootstrap": 0, "reclaim": 0, "preempt": 0}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, m := range reAction.FindAllStringSubmatch(string(body), -1) {
		switch m[1] {
		case "Bootstrap":
			out["bootstrap"] += atoi(m[2])
		case "Reclaim":
			out["reclaim"] += atoi(m[2])
		case "Preempt": // Phase-2: drained lower-priority capacity to serve higher-priority demand
			out["preempt"] += atoi(m[2])
		}
	}
	for _, m := range reInv.FindAllStringSubmatch(string(body), -1) {
		switch m[1] {
		case "Idle":
			out["idle"] += atoi(m[2])
		case "Configured":
			out["configured"] += atoi(m[2])
		case "Speculative":
			out["speculative"] += atoi(m[2])
		}
	}
	return out
}

// ---- demand model (native Deployments + PriorityClasses) ----
//
// Each tier is a set of real Deployments (one per "service") in a realistic
// namespace, so the dashboard's Workloads/Pods views look like a real platform and
// the stock scheduler handles priority + native preemption (and the ReplicaSet
// recreates preempted pods as Pending — no recreation hack needed):
//   baseline (PriorityClass "batch",    ns "batch")      — best-effort, preemptible.
//   demand   (PriorityClass "critical", ns "production") — what the visitor drives;
//                                                          under saturation it preempts batch.
// The visitor's "demand level" is a total replica count, spread across the tier's
// service Deployments.
// Three tiers: baseline batch (preemptible filler), demand (standard production —
// PriorityClass "production", preemptionPolicy Never, so it provisions rather than
// evicting batch), and critical (PriorityClass "critical" — the section-8 beat that
// genuinely preempts batch under scarcity).
var tierNS = map[string]string{"baseline": "batch", "demand": "production", "critical": "production-critical"}
var batchServices = []string{"etl-pipeline", "ml-training", "analytics-rollup", "data-export", "log-archival", "nightly-report", "backup-runner", "clickstream-agg"}
var prodServices = []string{"web-frontend", "checkout", "payments-api", "api-gateway", "search", "inventory", "user-auth", "recommendations", "cart-service", "order-processor"}

func tierServices(tier string) []string {
	if tier == "baseline" {
		return batchServices
	}
	return prodServices
}
func tierPriorityClass(tier string) string {
	switch tier {
	case "demand":
		return "production" // higher than batch, but preemptionPolicy:Never -> scales out, never evicts batch
	case "critical":
		return "critical" // preempts batch (section 8)
	default:
		return "batch"
	}
}

// Pod size classes — fractional shares of an 8 vCPU / 32 GiB node (~4Gi/core, the
// node ratio) so the scheduler bin-packs several heterogeneous pods per node instead
// of the old 1-pod-fills-the-whole-node. Illustrative author-chosen constants (like
// the prices); kwok runs each pod at its request.
type podSize struct {
	milliCPU int
	mem      string
}

var (
	sizeSmall  = podSize{500, "2Gi"}
	sizeMedium = podSize{1500, "6Gi"}
	sizeLarge  = podSize{3000, "12Gi"}
)

// per-service size class + relative weight, so the demand split is heterogeneous (not
// the old uniform total/N) and describe-node shows a believable pod mix.
var svcSize = map[string]podSize{
	"web-frontend": sizeMedium, "checkout": sizeSmall, "payments-api": sizeSmall,
	"api-gateway": sizeMedium, "search": sizeLarge, "inventory": sizeSmall,
	"user-auth": sizeSmall, "recommendations": sizeMedium, "cart-service": sizeSmall,
	"order-processor": sizeLarge,
	"etl-pipeline": sizeLarge, "ml-training": sizeLarge, "analytics-rollup": sizeMedium,
	"data-export": sizeSmall, "log-archival": sizeSmall, "nightly-report": sizeMedium,
	"backup-runner": sizeSmall, "clickstream-agg": sizeMedium,
}
var svcWeight = map[string]int{
	"web-frontend": 6, "checkout": 4, "payments-api": 1, "api-gateway": 4, "search": 2,
	"inventory": 3, "user-auth": 2, "recommendations": 3, "cart-service": 3, "order-processor": 1,
	"etl-pipeline": 3, "ml-training": 2, "analytics-rollup": 3, "data-export": 4,
	"log-archival": 2, "nightly-report": 2, "backup-runner": 3, "clickstream-agg": 3,
}

func sizeOf(svc string) podSize {
	if s, ok := svcSize[svc]; ok {
		return s
	}
	return sizeMedium
}
func weightOf(svc string) int {
	if w, ok := svcWeight[svc]; ok {
		return w
	}
	return 1
}

// nodeBudgetMilli: the schedulable CPU (millicores) one demand "level" unit maps to
// (~ one node's worth), set from --node-budget-cpu. So demand level L ≈ L nodes' worth
// of pod requests, which BigFleet packs onto ~L machines and the scheduler packs onto
// ~L nodes — node count is emergent from real bin-packing, not 1 pod = 1 node.
var nodeBudgetMilli = 7000

func makeDeployment(svc, ns, tier string, replicas int32) *appsv1.Deployment {
	sz := sizeOf(svc)
	req := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", sz.milliCPU)),
		corev1.ResourceMemory: resource.MustParse(sz.mem),
	}
	lim := corev1.ResourceList{ // cpu burstable (2x), mem == request -> Burstable QoS
		corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", sz.milliCPU*2)),
		corev1.ResourceMemory: resource.MustParse(sz.mem),
	}
	probe := &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(8080)}},
		InitialDelaySeconds: 3, PeriodSeconds: 10,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: ns,
			Labels:      map[string]string{"app": svc, "tier": tier},
			Annotations: map[string]string{"bigfleet.demo/simulated": "true"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": svc}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": svc, "tier": tier, "bigfleet.demo/simulated": "true"}},
				Spec: corev1.PodSpec{
					PriorityClassName:             tierPriorityClass(tier),
					TerminationGracePeriodSeconds: int64p(30),
					Containers: []corev1.Container{{Name: "app", Image: "registry.k8s.io/pause:3.9",
						Ports:          []corev1.ContainerPort{{ContainerPort: 8080}},
						Resources:      corev1.ResourceRequirements{Requests: req, Limits: lim},
						ReadinessProbe: probe, LivenessProbe: probe}},
				},
			},
		},
	}
}

var (
	ensureMu    sync.Mutex
	ensureDone  = map[string]bool{}
	demandLevel = map[string]map[string]int{} // cluster -> tier -> last-set LEVEL (0-40) for the UI control
)

// systemDaemonSets: the baseline every real node runs (CNI + kube-proxy + node-exporter),
// so a node is never empty — even idle nodes show ~3 system pods + non-zero Allocated.
// Tiny best-effort pause pods at node-critical priority (never preempted), tolerate every
// taint so they land on every node. (~250m/320Mi/node; the engine over-provisions a hair
// to absorb it, which IS realistic headroom.)
var systemDaemonSets = []struct {
	name string
	cpu  int
	mem  string
}{
	{"kube-proxy", 100, "128Mi"},
	{"cni-node-agent", 100, "128Mi"},
	{"node-exporter", 50, "64Mi"},
}

func makeDaemonSet(name string, milliCPU int, mem string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system",
			Labels:      map[string]string{"app": name, "tier": "system"},
			Annotations: map[string]string{"bigfleet.demo/simulated": "true"}},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name, "tier": "system", "bigfleet.demo/simulated": "true"}},
				Spec: corev1.PodSpec{
					PriorityClassName:             "node-critical",
					TerminationGracePeriodSeconds: int64p(5),
					Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, // every node, incl. tainted
					Containers: []corev1.Container{{Name: name, Image: "registry.k8s.io/pause:3.9",
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", milliCPU)),
							corev1.ResourceMemory: resource.MustParse(mem)}}}},
				},
			},
		},
	}
}

func ensureDaemonSets(ctx context.Context, cl *cluster) {
	_ = cl.c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}) // usually exists
	for _, ds := range systemDaemonSets {
		_ = cl.c.Create(ctx, makeDaemonSet(ds.name, ds.cpu, ds.mem)) // ignore AlreadyExists
	}
}

// ensureDeployments creates one replicas:0 Deployment per service per tier, once per cluster.
func ensureDeployments(ctx context.Context, cl *cluster) {
	ensureMu.Lock()
	done := ensureDone[cl.name]
	ensureDone[cl.name] = true
	ensureMu.Unlock()
	if done {
		return
	}
	ensureDaemonSets(ctx, cl)
	for _, tier := range []string{"baseline", "demand", "critical"} {
		ns := tierNS[tier]
		_ = cl.c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		for _, svc := range tierServices(tier) {
			_ = cl.c.Create(ctx, makeDeployment(svc, ns, tier, 0)) // ignore AlreadyExists
		}
	}
}

// setTierReplicas converts a demand LEVEL (node-equivalents) into a heterogeneous
// BUNDLE of fractional pods whose summed requests ≈ level node-budgets, spread across
// the tier's services by weight + size class. The pods go Pending; BigFleet packs their
// needs onto 8-core machines (real bin-packing) and provisions ~level nodes — node count
// is emergent, not 1 pod = 1 node.
func setTierReplicas(ctx context.Context, cl *cluster, tier string, level int) {
	ensureDeployments(ctx, cl)
	ensureMu.Lock()
	if demandLevel[cl.name] == nil {
		demandLevel[cl.name] = map[string]int{}
	}
	demandLevel[cl.name][tier] = level // remember the level so the UI shows the slider value, not pod count
	ensureMu.Unlock()
	svcs := tierServices(tier)
	ns := tierNS[tier]
	targetMilli := level * nodeBudgetMilli
	totalW := 0
	for _, s := range svcs {
		totalW += weightOf(s)
	}
	for _, svc := range svcs {
		r := 0
		if totalW > 0 {
			share := float64(targetMilli) * float64(weightOf(svc)) / float64(totalW)
			r = int(math.Round(share / float64(sizeOf(svc).milliCPU)))
		}
		patch := ctrlclient.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, r)))
		// Retry + log: swallowing this (the old `_ =`) silently dropped demand mutations
		// under concurrent load, wedging the fleet at a non-baseline state.
		var perr error
		for attempt := 0; attempt < 4; attempt++ {
			if perr = cl.c.Patch(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: ns}}, patch); perr == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if perr != nil {
			fmt.Fprintf(os.Stderr, "demo-backend: setTierReplicas %s/%s=%d: %v\n", ns, svc, r, perr)
		}
	}
}

// seedBusyFleet puts the fleet in its at-rest BUSY state: a batch baseline on every cluster
// PLUS a standing production workload on the donor (cluster-a, index 0) — all packed onto
// COMMITTED capacity ($0 marginal, cloud untouched). So the demo OPENS on a busy-but-stable
// fleet and the cross-cluster 'move' has a real donor to reclaim from with no self-inflicted
// scale-up. Everything stays within committed, so CostPerHour is $0 and "committed only / $0/hr" holds.
func seedBusyFleet(clusters []cluster, baseline, donorDemand int) {
	ctx := context.Background()
	for i := range clusters {
		setTierReplicas(ctx, &clusters[i], "baseline", baseline)
		setTierReplicas(ctx, &clusters[i], "critical", 0)
		d := 0
		if i == 0 { // cluster-a carries the standing production workload (the move's donor)
			d = donorDemand
		}
		setTierReplicas(ctx, &clusters[i], "demand", d)
	}
}

// POST /api/demand {workspace, level, tier?} -> set that cluster's demand-tier replica
// total. tier defaults to "demand" (standard production, non-preempting); section 8 sends
// tier:"critical" to drive the preempting tier.
func demandHandler(clusters []cluster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Workspace string `json:"workspace"`
			Level     int    `json:"level"`
			Tier      string `json:"tier"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cl := findCluster(clusters, req.Workspace)
		if cl == nil {
			http.Error(w, "unknown workspace", 400)
			return
		}
		tier := req.Tier
		if tier != "critical" {
			tier = "demand"
		}
		setTierReplicas(context.Background(), cl, tier, clamp(req.Level, 0, 40))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// POST /api/reset -> back to the at-rest BUSY fleet (baseline batch + cluster-a donor demand,
// all on committed / $0), not an empty fleet — so "Reset" returns to the loaded starting state.
func resetHandler(clusters []cluster, baseline, donorDemand int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cancelScenario()
		seedBusyFleet(clusters, baseline, donorDemand)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// ---- staged scenarios (deterministic teaching beats) ----
//
// The §6/§7/§8 buttons drive the fleet to the required PRECONDITION, then inject the beat
// and narrate it — so the lesson reproduces on every click (a thin demand-poke can't
// guarantee a full/contended fleet). One scenario runs at a time; reset or a new one
// cancels it. Narration is honest: the scenario states what it's about to do; the viewer
// watches the real cluster cards + supply bars react.
var (
	scenMu      sync.Mutex
	scenCancel  context.CancelFunc
	curScenario string // running teaching scenario (move|saturate|critical), "" when idle — lets the UI disable buttons mid-run
	scenGen     int    // bumped per launch; the finishing goroutine clears curScenario only if its gen is still current (avoids a name-based ABA)
	latestMu    sync.Mutex
	latest      fleetState
)

func setLatest(st fleetState) { latestMu.Lock(); latest = st; latestMu.Unlock() }
func capNow() capacity        { latestMu.Lock(); defer latestMu.Unlock(); return latest.Capacity }
func clusterNodesNow(name string) int {
	latestMu.Lock()
	defer latestMu.Unlock()
	for _, c := range latest.Clusters {
		if c.Name == name {
			return c.Nodes
		}
	}
	return 0
}

// pushFeed prepends a scenario narration line to the decision feed.
func pushFeed(line string) {
	mu.Lock()
	defer mu.Unlock()
	feed = append([]string{time.Now().Format("15:04:05") + "  " + line}, feed...)
	if len(feed) > 40 {
		feed = feed[:40]
	}
}

func cancelScenario() {
	scenMu.Lock()
	if scenCancel != nil {
		scenCancel()
		scenCancel = nil
	}
	curScenario = ""
	scenMu.Unlock()
}

// waitFor polls cond until true, ctx cancelled, or timeout.
func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if cond() {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return cond()
}

func runScenario(ctx context.Context, clusters []cluster, name string) {
	switch name {
	case "saturate":
		pushFeed("▶ Saturating the finite fleet — driving demand onto all three clusters until every machine is committed…")
		for i := range clusters {
			setTierReplicas(ctx, &clusters[i], "demand", 40)
		}
	case "critical":
		pushFeed("▶ Filling the fleet first, then sending CRITICAL demand into it — with nowhere left to provision, priority must decide…")
		for i := range clusters {
			setTierReplicas(ctx, &clusters[i], "demand", 40)
		}
		waitFor(ctx, 75*time.Second, func() bool { return capNow().CloudAvailable <= 8 })
		if ctx.Err() != nil {
			return
		}
		pushFeed("⚡ Fleet is full. Injecting critical demand on cluster-a — the scheduler must preempt lower-priority batch to place it.")
		setTierReplicas(ctx, &clusters[0], "critical", 28)
	case "move":
		if len(clusters) < 2 {
			return
		}
		// The move is the from-REST lead payoff: cluster-a's standing donor demand is reclaimed to
		// cluster-b, entirely within committed ($0). If the fleet ISN'T at rest — e.g. right after
		// saturate/critical, which leave it on billed cloud and a chaotically-draining mix — we do
		// NOT fake it (that's how the "$/hr stays $0" line could become a lie). Ask for a reset
		// instead; Reset restores the at-rest busy fleet. (Tolerate one unconsolidated cloud node.)
		ensureMu.Lock()
		aDemand := demandLevel[clusters[0].name]["demand"]
		ensureMu.Unlock()
		if capNow().CloudInUse > 1 || aDemand < 8 {
			pushFeed("▶ The fleet isn't at its at-rest state yet (a previous scenario is still draining). Press ↺ Reset, then Move.")
			return
		}
		// Drop cluster-a's donor demand, wait for it to drain back to the shared Idle pool, THEN
		// spike cluster-b within the freed committed slice — B reuses committed, no fresh cloud.
		pushFeed("➡ Dropping cluster-a's production demand — its committed nodes drain back to the shared Idle pool…")
		setTierReplicas(ctx, &clusters[0], "demand", 0)
		if !waitFor(ctx, 60*time.Second, func() bool { return clusterNodesNow(clusters[0].name) <= 16 }) {
			return
		}
		// Claim absolute "$/hr stays $0" only when it's actually true; if a stray node lingered,
		// claim only what the MOVE itself does (provisions no new cloud — B reuses freed committed).
		costLine := "that capacity reused, NO new cloud spend ($/hr stays $0)."
		if capNow().CostPerHour != 0 {
			costLine = "the move itself provisions no new cloud — cluster-b reuses cluster-a's freed committed capacity."
		}
		pushFeed("➡ …and now cluster-b grows from the committed capacity cluster-a just freed, drawn from the shared Idle pool — " + costLine)
		setTierReplicas(ctx, &clusters[1], "demand", 14)
	}
}

// POST /api/scenario {name: saturate|critical|move}
func scenarioHandler(clusters []cluster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Name { // allowlist — reject anything but the three teaching scenarios
		case "saturate", "critical", "move":
		default:
			http.Error(w, "unknown scenario", http.StatusBadRequest)
			return
		}
		scenMu.Lock()
		if scenCancel != nil {
			scenCancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		scenCancel = cancel
		scenGen++
		gen := scenGen
		curScenario = req.Name
		scenMu.Unlock()
		go func(g int) {
			runScenario(ctx, clusters, req.Name)
			scenMu.Lock()
			if scenGen == g { // clear only if a newer scenario hasn't superseded this run
				curScenario = ""
			}
			scenMu.Unlock()
		}(gen)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// ---- abuse limits ----
//
// One demo-backend serves ONE session, reached by an anonymous visitor through the demohost
// /s/{id} proxy. These bound what that single visitor can do to their own session's backend:
// a token-bucket on the write endpoints (demand/scenario/reset) and a hard cap on concurrent
// SSE streams. All in-process and session-scoped — they don't touch the host.

const maxSSEClients = 64 // concurrent /api/stream connections per session backend

const maxBodyBytes = 4 << 10 // 4 KiB — every /api/* body is a tiny JSON object

// rateLimiter is a minimal token bucket (no deps). Shared across the write endpoints of one
// backend, so a visitor spamming /api/demand|scenario|reset is throttled to a sane rate.
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	perSec float64
	last   time.Time
}

func newRateLimiter(perSec, burst float64) *rateLimiter {
	return &rateLimiter{tokens: burst, max: burst, perSec: perSec, last: time.Now()}
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	rl.tokens += now.Sub(rl.last).Seconds() * rl.perSec
	if rl.tokens > rl.max {
		rl.tokens = rl.max
	}
	rl.last = now
	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	return false
}

// writeLimiter throttles the state-changing endpoints (generous for real UI clicks, hostile
// to a spam loop): ~5 req/s sustained, burst 10.
var writeLimiter = newRateLimiter(5, 10)

// limited wraps a write handler with the body-size cap + the rate limit.
func limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !writeLimiter.allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r)
	}
}

// ---- SSE hub ----

type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]bool
	last    []byte
}

func newHub() *hub { return &hub{clients: map[chan []byte]bool{}} }

func (h *hub) broadcast(st fleetState) {
	b, _ := json.Marshal(st)
	h.mu.Lock()
	h.last = b
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan []byte, 4)
	h.mu.Lock()
	if len(h.clients) >= maxSSEClients { // cap concurrent streams per session backend
		h.mu.Unlock()
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	h.clients[ch] = true
	last := h.last
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, ch); h.mu.Unlock() }()
	if last != nil {
		fmt.Fprintf(w, "data: %s\n\n", last)
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// ---- helpers ----

func loadSession(p string) session {
	b, err := os.ReadFile(p)
	if err != nil {
		die("session", err)
	}
	var s session
	if err := json.Unmarshal(b, &s); err != nil {
		die("session json", err)
	}
	return s
}
func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
func findCluster(cs []cluster, name string) *cluster {
	for i := range cs {
		if cs[i].name == name {
			return &cs[i]
		}
	}
	return nil
}
func atoi(s string) int { f, _ := strconv.ParseFloat(s, 64); return int(f) }
func clamp(v, lo, hi int) int { if v < lo { return lo }; if v > hi { return hi }; return v }
func int32p(v int32) *int32 { return &v }
func int64p(v int64) *int64 { return &v }
func must(err error) { if err != nil { die("scheme", err) } }
func die(what string, err error) { fmt.Fprintf(os.Stderr, "demo-backend: %s: %v\n", what, err); os.Exit(1) }
