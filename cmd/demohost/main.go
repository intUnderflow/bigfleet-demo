// Command demohost runs many isolated BigFleet demo sessions on one machine and is the
// ONLY thing allowed to create them: every create is gated by a secret key, so whoever
// holds the key is the central coordinator. Each session is a full demo stack (3 kwokctl
// clusters + shard + 3 fakecloud providers + per-cluster controllers + backend/UI) on its
// own port block, running for up to an hour and reaped a few minutes after its browser tab
// goes idle. The host reserves a fixed memory budget to demos and refuses any new session
// that would push past it (cross-checked against measured RSS), so it never blows the limit.
//
// Honesty: the per-session memory figure is a configured RESERVATION (measured to pick a
// sane default — ~1.9 GB/session on the dev Mac), not a live cgroup cap; the `capacity`
// endpoint always also reports the MEASURED actual so reality is visible. demohost never
// touches core BigFleet — it only drives this repo's hack/demo-*.sh.
//
// Subcommands:
//
//	demohost serve     run the daemon (key-gated HTTP API + reaper)
//	demohost create    ask a running daemon for a new session (prints its URL)
//	demohost ls        list live sessions
//	demohost capacity  show the machine/budget/measured picture
//	demohost rm <id>   tear a session down now
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	reapInterval = 15 * time.Second
	backendGrace = 60 * time.Second // backend unreachable this long => assume dead, reap
	spawnTimeout = 4 * time.Minute  // demo-up.sh upper bound (cold image pulls etc.)
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serveMain(os.Args[2:])
	case "create", "ls", "capacity", "rm":
		clientMain(os.Args[1], os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "demohost: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `demohost — run many isolated BigFleet demo sessions on one machine

  demohost serve [flags]     run the daemon (key-gated API + reaper)
  demohost create [flags]    create a session (prints its URL)
  demohost ls [flags]        list live sessions
  demohost capacity [flags]  machine / budget / measured memory
  demohost rm <id> [flags]   tear a session down now

Run "demohost serve -h" or "demohost create -h" for flags.
`)
}

// ── config ───────────────────────────────────────────────────────────────────

type config struct {
	addr         string
	repo         string
	advertise    string
	key          string
	portBase     int
	stride       int
	maxSessions  int
	demoBudgetMB int
	sessionMB    int
	ttl          time.Duration
	idle         time.Duration
	dashboards   bool
	warmPool     int
	warmSettle   time.Duration
	warmMaxAge   time.Duration
}

// ── session + host ─────────────────────────────────────────────────────────────

type session struct {
	ID         string    `json:"id"`
	PortBase   int       `json:"portBase"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"` // hard cap = CreatedAt + ttl
	ReservedMB int       `json:"reservedMB"`
	State      string    `json:"state"`  // starting | warming | pooled | running | reaping
	Pooled     bool      `json:"pooled"` // a pre-warmed, not-yet-assigned session

	lastSeenOK time.Time // last successful backend poll (reaper grace clock)
	assignedAt time.Time // when handed to a visitor (pool hand-out or direct create); zero = never
}

// completion is a finished visitor session, kept in a small ring so the coordinator's stats
// cron can scrape real session durations (the host is the only thing that knows them).
type completion struct {
	EndedAt     int64   `json:"endedAt"` // unix seconds
	DurationSec float64 `json:"durationSec"`
	Reason      string  `json:"reason"` // idle | hard cap | manual
}

const maxCompletions = 1000

type host struct {
	cfg config

	mu        sync.Mutex
	sessions  map[string]*session
	usedBases map[int]bool

	completions []completion // ring of finished visitor sessions (for /v1/stats)

	machineMB    int
	memActualMB  int // last measured actual demo RSS (0 = unknown/not yet sampled)
	memSampledAt time.Time
	poolInFlight int // pooled sessions being created but not yet in the map (admission accounting)
}

// appendCompletionLocked records a finished visitor session. Caller holds h.mu. Sessions that
// were never handed to a visitor (e.g. an unclaimed pooled session) have a zero assignedAt and
// are skipped, so the stats only reflect real usage.
func (h *host) appendCompletionLocked(s *session, reason string, now time.Time) {
	if s.assignedAt.IsZero() {
		return
	}
	h.completions = append(h.completions, completion{
		EndedAt: now.Unix(), DurationSec: now.Sub(s.assignedAt).Seconds(), Reason: reason,
	})
	if len(h.completions) > maxCompletions {
		h.completions = h.completions[len(h.completions)-maxCompletions:]
	}
}

func newHost(cfg config) *host {
	return &host{
		cfg:       cfg,
		sessions:  map[string]*session{},
		usedBases: map[int]bool{},
		machineMB: machineTotalMB(),
	}
}

// activeCountLocked counts sessions that hold a reservation (starting + running). Caller
// holds h.mu.
func (h *host) activeCountLocked() int {
	n := 0
	for _, s := range h.sessions {
		if s.State != "reaping" {
			n++
		}
	}
	return n
}

// admitLocked decides whether one more session fits. Caller holds h.mu. The reservation
// check is the primary, deterministic guard ("reserve N×perSession, never exceed budget");
// the measured-actual check is a secondary safety net so a session running heavier than its
// reservation still can't push real usage past the budget.
func (h *host) admitLocked() error {
	n := h.activeCountLocked() + h.poolInFlight // count pooled sessions being created too
	if n >= h.cfg.maxSessions {
		return fmt.Errorf("at max sessions (%d)", h.cfg.maxSessions)
	}
	if reserved := (n + 1) * h.cfg.sessionMB; reserved > h.cfg.demoBudgetMB {
		return fmt.Errorf("reservation budget: %d running × %d MB + %d MB > %d MB budget",
			n, h.cfg.sessionMB, h.cfg.sessionMB, h.cfg.demoBudgetMB)
	}
	if h.memActualMB > 0 && h.memActualMB+h.cfg.sessionMB > h.cfg.demoBudgetMB {
		return fmt.Errorf("measured memory near budget: actual %d MB + %d MB > %d MB budget",
			h.memActualMB, h.cfg.sessionMB, h.cfg.demoBudgetMB)
	}
	return nil
}

// allocBaseLocked finds the lowest free, bindable port block. Caller holds h.mu.
func (h *host) allocBaseLocked() (int, error) {
	for k := 0; k < h.cfg.maxSessions*4+8; k++ {
		b := h.cfg.portBase + k*h.cfg.stride
		if h.usedBases[b] {
			continue
		}
		if !portFree(b) {
			continue
		}
		return b, nil
	}
	return 0, errors.New("no free port block in range")
}

func genID(taken map[string]*session) string {
	const first = "abcdefghijkmnpqrstuvwxyz" // drop l/o for legibility
	const rest = "abcdefghijkmnpqrstuvwxyz23456789"
	for {
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		id := []byte{first[int(b[0])%len(first)]}
		for i := 1; i < 5; i++ {
			id = append(id, rest[int(b[i])%len(rest)])
		}
		s := string(id)
		if _, dup := taken[s]; !dup {
			return s
		}
	}
}

// ── lifecycle: spawn / reap ────────────────────────────────────────────────────

func (h *host) spawn(s *session) error {
	ctx, cancel := context.WithTimeout(context.Background(), spawnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(h.cfg.repo, "hack", "demo-up.sh"))
	cmd.Dir = h.cfg.repo
	cmd.Env = append(os.Environ(),
		"SESSION_ID="+s.ID,
		"PORT_BASE="+strconv.Itoa(s.PortBase),
		"DASHBOARDS="+boolEnv(h.cfg.dashboards),
		"SESSION_TTL="+h.cfg.ttl.String(),
		"SESSION_IDLE_TIMEOUT="+h.cfg.idle.String(),
		"SKIP_BUILD=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("demo-up.sh: %v\n%s", err, lastLines(string(out), 12))
	}
	return nil
}

func (h *host) reap(s *session) {
	cmd := exec.Command("bash", filepath.Join(h.cfg.repo, "hack", "demo-down.sh"))
	cmd.Dir = h.cfg.repo
	cmd.Env = append(os.Environ(), "SESSION_ID="+s.ID, "PORT_BASE="+strconv.Itoa(s.PortBase))
	_ = cmd.Run()
}

// sweepOrphans reaps clusters + run/ state left by a PREVIOUS demohost PID. Sessions live
// only in h.sessions (in-memory), so on a fresh boot the map is empty and ANY kwokctl
// cluster matching our per-session naming (<id>-cluster-a/b/c) or any run/sessions/<id>
// dir is an unreachable zombie — the new demohost can never proxy to it. Without this, a
// hard restart (launchd kickstart -k that outruns the SIGTERM reapAll) leaks the prior
// life's clusters; they pile up as Docker containers until the Docker VM is exhausted and
// every new session fails to create. Runs once at startup, before the warm pool, so the
// fleet boots onto a clean substrate. Best-effort; reaps concurrently but bounded.
func (h *host) sweepOrphans() {
	ids := map[string]struct{}{}
	if out, err := exec.Command("kwokctl", "get", "clusters").Output(); err == nil {
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			ln = strings.TrimSpace(ln)
			for _, suf := range []string{"-cluster-a", "-cluster-b", "-cluster-c"} {
				if strings.HasSuffix(ln, suf) {
					ids[strings.TrimSuffix(ln, suf)] = struct{}{}
				}
			}
		}
	}
	// Also catch sessions whose clusters already died but whose processes/ports/state linger.
	if entries, err := os.ReadDir(filepath.Join(h.cfg.repo, "run", "sessions")); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != "" {
				ids[e.Name()] = struct{}{}
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	fmt.Printf("demohost: sweeping %d orphaned session(s) left by a previous run…\n", len(ids))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			h.reap(&session{ID: id})
		}(id)
	}
	wg.Wait()
	fmt.Println("demohost: orphan sweep complete")
}

// removeLocked drops a session from the registry and frees its port block.
func (h *host) removeLocked(id string) {
	if s, ok := h.sessions[id]; ok {
		delete(h.usedBases, s.PortBase)
		delete(h.sessions, id)
	}
}

// ── reaper ─────────────────────────────────────────────────────────────────────

func (h *host) reaperLoop(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.reapOnce()
			h.sampleMemory()
		}
	}
}

func (h *host) reapOnce() {
	now := time.Now()
	h.mu.Lock()
	var due []*session
	for _, s := range h.sessions {
		if s.State != "running" {
			continue
		}
		hardExpired := now.After(s.ExpiresAt)
		idleExpired, ok := backendExpired(s.PortBase)
		if ok {
			s.lastSeenOK = now
		}
		unreachable := !ok && now.Sub(s.lastSeenOK) > backendGrace
		if hardExpired || idleExpired || unreachable {
			s.State = "reaping"
			due = append(due, s)
		}
	}
	h.mu.Unlock()

	for _, s := range due {
		reason := "idle"
		if now.After(s.ExpiresAt) {
			reason = "hard cap"
		}
		fmt.Printf("demohost: reaping session %s (port %d) — %s\n", s.ID, s.PortBase, reason)
		h.reap(s)
		h.mu.Lock()
		h.removeLocked(s.ID)
		h.appendCompletionLocked(s, reason, now)
		h.mu.Unlock()
	}
}

func (h *host) reapAll() {
	h.mu.Lock()
	all := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		all = append(all, s)
	}
	h.mu.Unlock()
	for _, s := range all {
		fmt.Printf("demohost: shutdown — reaping session %s\n", s.ID)
		h.reap(s)
	}
}

func (h *host) sampleMemory() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.sessions))
	bases := make([]int, 0, len(h.sessions))
	for _, s := range h.sessions {
		ids = append(ids, s.ID)
		bases = append(bases, s.PortBase)
	}
	h.mu.Unlock()
	mb := measureDemoMB(h.cfg.repo, ids)
	h.mu.Lock()
	h.memActualMB = mb
	h.memSampledAt = time.Now()
	h.mu.Unlock()
	_ = bases
}

// ── warm pool ──────────────────────────────────────────────────────────────────
//
// To make dive-in instant, keep --warm-pool sessions pre-created AND baseline-settled, ready
// to hand out the moment a visitor arrives. Pooled sessions are heartbeated so they don't
// idle-reap, and recycled past --warm-max-age so a hand-out always has plenty of session time
// left. No backend change: a handed-out session simply keeps the clock it started with.

// takePooledLocked claims a ready pooled session for a visitor, or nil. Caller holds h.mu.
func (h *host) takePooledLocked(now time.Time) *session {
	for _, s := range h.sessions {
		if s.Pooled && s.State == "pooled" {
			s.Pooled = false
			s.State = "running"
			s.lastSeenOK = now
			s.assignedAt = now            // visitor's session clock starts at hand-out
			s.ExpiresAt = now.Add(h.cfg.ttl) // restart the reap hard-cap from hand-out, not warm-time
			return s
		}
	}
	return nil
}

// beginBackend tells a just-handed-out session's backend to (re)start the visitor-facing
// clock NOW. A warm-pool backend booted minutes before the visitor arrived, so without this
// its top-bar countdown and idle timer would already be partly spent. Best-effort + idempotent
// on the backend (so the public /s/{id} proxy can't be used to extend a session).
func (h *host) beginBackend(s *session) {
	cl := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/begin", s.PortBase), nil)
	resp, err := cl.Do(req)
	if err != nil {
		fmt.Printf("demohost: begin clock for %s failed: %v\n", s.ID, err)
		return
	}
	_ = resp.Body.Close()
}

func (h *host) poolLoop(ctx context.Context) {
	t := time.NewTicker(8 * time.Second)
	defer t.Stop()
	h.poolTick() // fill immediately at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.poolTick()
		}
	}
}

func (h *host) poolTick() {
	now := time.Now()
	h.mu.Lock()
	var recycle []*session
	var keepAlive []int
	pooled := 0
	for _, s := range h.sessions {
		if !s.Pooled || s.State == "reaping" {
			continue
		}
		if now.Sub(s.CreatedAt) > h.cfg.warmMaxAge {
			s.State = "reaping"
			recycle = append(recycle, s)
			continue
		}
		pooled++
		if s.State == "warming" || s.State == "pooled" {
			keepAlive = append(keepAlive, s.PortBase)
		}
	}
	launch := 0
	for pooled+h.poolInFlight+launch < h.cfg.warmPool && h.admitLocked() == nil {
		h.poolInFlight++ // reserve the slot for admission accounting
		launch++
	}
	h.mu.Unlock()

	for _, p := range keepAlive {
		pokeHeartbeat(p) // keep pooled/warming sessions from idle-reaping
	}
	for _, s := range recycle {
		fmt.Printf("demohost: recycling stale pooled session %s\n", s.ID)
		h.reap(s)
		h.mu.Lock()
		h.removeLocked(s.ID)
		h.mu.Unlock()
	}
	for i := 0; i < launch; i++ {
		go h.createPooled()
	}
}

func (h *host) createPooled() {
	now := time.Now()
	h.mu.Lock()
	base, err := h.allocBaseLocked()
	if err != nil {
		h.poolInFlight--
		h.mu.Unlock()
		return
	}
	id := genID(h.sessions)
	s := &session{
		ID: id, PortBase: base,
		URL:        fmt.Sprintf("http://%s:%d", h.cfg.advertise, base),
		CreatedAt:  now,
		ExpiresAt:  now.Add(h.cfg.ttl),
		ReservedMB: h.cfg.sessionMB,
		State:      "starting", Pooled: true,
		lastSeenOK: now,
	}
	h.sessions[id] = s
	h.usedBases[base] = true
	h.poolInFlight-- // now counted via the map, no longer in-flight
	h.mu.Unlock()

	fmt.Printf("demohost: warming pooled session %s on port %d\n", id, base)
	if err := h.spawn(s); err != nil {
		fmt.Printf("demohost: pooled session %s failed to warm: %v\n", id, err)
		h.reap(s)
		h.mu.Lock()
		h.removeLocked(id)
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	if s.State == "starting" {
		s.State = "warming"
	}
	s.lastSeenOK = time.Now()
	h.mu.Unlock()

	waitForSettle(s.PortBase, h.cfg.warmSettle) // poll until the busy baseline converges (capped at warmSettle)

	h.mu.Lock()
	if s.State == "warming" { // not reaped/claimed mid-settle
		s.State = "pooled"
		fmt.Printf("demohost: pooled session %s ready\n", id)
	}
	h.mu.Unlock()
}

// pokeHeartbeat resets a session backend's idle clock (used to keep pooled sessions alive).
func pokeHeartbeat(portBase int) {
	cl := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/heartbeat", portBase), nil)
	if resp, err := cl.Do(req); err == nil {
		resp.Body.Close()
	}
}

// ── HTTP ───────────────────────────────────────────────────────────────────────

func (h *host) auth(next http.HandlerFunc) http.HandlerFunc {
	want := []byte(h.cfg.key)
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Demo-Key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *host) createSession(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	h.mu.Lock()
	// Warm pool: if a pre-warmed, baseline-settled session is ready, hand it out instantly.
	if s := h.takePooledLocked(now); s != nil {
		h.mu.Unlock()
		h.beginBackend(s) // start the visitor clock at hand-out, not at warm-boot
		fmt.Printf("demohost: handed pooled session %s to a visitor (instant)\n", s.ID)
		writeJSON(w, http.StatusCreated, s)
		return
	}
	if err := h.admitLocked(); err != nil {
		h.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	base, err := h.allocBaseLocked()
	if err != nil {
		h.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	id := genID(h.sessions)
	s := &session{
		ID: id, PortBase: base,
		URL:        fmt.Sprintf("http://%s:%d", h.cfg.advertise, base),
		CreatedAt:  now,
		ExpiresAt:  now.Add(h.cfg.ttl),
		ReservedMB: h.cfg.sessionMB,
		State:      "starting",
		lastSeenOK: now,
		assignedAt: now, // direct-created for a waiting visitor — clock starts now
	}
	h.sessions[id] = s
	h.usedBases[base] = true
	h.mu.Unlock()

	fmt.Printf("demohost: starting session %s on port %d\n", id, base)
	if err := h.spawn(s); err != nil {
		fmt.Printf("demohost: session %s failed to start: %v\n", id, err)
		h.reap(s) // clean up any partial stack
		h.mu.Lock()
		h.removeLocked(id)
		h.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session failed to start: " + err.Error()})
		return
	}
	h.mu.Lock()
	s.State = "running"
	s.lastSeenOK = time.Now()
	h.mu.Unlock()
	h.beginBackend(s) // spawn took ~a minute; start the visitor clock now that it's ready
	fmt.Printf("demohost: session %s up at %s\n", id, s.URL)
	writeJSON(w, http.StatusCreated, s)
}

func (h *host) listSessions(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	out := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *host) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mu.Lock()
	s, ok := h.sessions[id]
	if ok {
		s.State = "reaping"
	}
	h.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
		return
	}
	h.reap(s)
	h.mu.Lock()
	h.removeLocked(id)
	h.appendCompletionLocked(s, "manual", time.Now())
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"reaped": id})
}

type capacityView struct {
	MachineTotalMB   int     `json:"machineTotalMB"`
	DemoBudgetMB     int     `json:"demoBudgetMB"`
	PerSessionMB     int     `json:"perSessionMB"`
	RunningSessions  int     `json:"runningSessions"`  // all non-reaping sessions incl. the warm pool (admission accounting)
	VisitorSessions  int     `json:"visitorSessions"`  // sessions actually handed to a visitor (true "busy")
	MaxSessions      int     `json:"maxSessions"`
	ReservedMB       int     `json:"reservedMB"`
	MeasuredActualMB int     `json:"measuredActualMB"`
	MeasuredAgeSec   int     `json:"measuredAgeSec"`
	WarmReady        int     `json:"warmReady"` // pre-warmed sessions ready for instant hand-out
	HeadroomSessions int     `json:"headroomSessions"`
	BudgetUsedPct    float64 `json:"budgetUsedPct"`
}

func (h *host) capacityView() capacityView {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.activeCountLocked()
	visitors := 0 // sessions actually serving a visitor (assignedAt set) — the true "busy"
	for _, s := range h.sessions {
		if !s.assignedAt.IsZero() && s.State != "reaping" {
			visitors++
		}
	}
	reserved := n * h.cfg.sessionMB
	// headroom = how many more sessions fit under BOTH the reservation budget and (if we
	// have a measurement) the measured-actual budget, and under maxSessions.
	byReserve := (h.cfg.demoBudgetMB - reserved) / h.cfg.sessionMB
	headroom := byReserve
	if h.memActualMB > 0 {
		byActual := (h.cfg.demoBudgetMB - h.memActualMB) / h.cfg.sessionMB
		if byActual < headroom {
			headroom = byActual
		}
	}
	if byMax := h.cfg.maxSessions - n; byMax < headroom {
		headroom = byMax
	}
	if headroom < 0 {
		headroom = 0
	}
	// A ready pooled session serves a visitor by instant hand-out (no new slot needed), so it
	// adds to the visitor-servable headroom the coordinator routes on.
	warmReady := 0
	for _, s := range h.sessions {
		if s.Pooled && s.State == "pooled" {
			warmReady++
		}
	}
	headroom += warmReady
	age := 0
	if !h.memSampledAt.IsZero() {
		age = int(time.Since(h.memSampledAt).Seconds())
	}
	return capacityView{
		MachineTotalMB: h.machineMB, DemoBudgetMB: h.cfg.demoBudgetMB, PerSessionMB: h.cfg.sessionMB,
		RunningSessions: n, VisitorSessions: visitors, MaxSessions: h.cfg.maxSessions, ReservedMB: reserved,
		MeasuredActualMB: h.memActualMB, MeasuredAgeSec: age, WarmReady: warmReady, HeadroomSessions: headroom,
		BudgetUsedPct: pct(reserved, h.cfg.demoBudgetMB),
	}
}

func (h *host) capacity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.capacityView())
}

// stats returns finished visitor sessions newer than ?since=<unix-seconds>, so the coordinator's
// stats cron can record real session durations without double-counting (it advances `since` to
// the returned `now` each tick). The ring holds the most recent maxCompletions.
func (h *host) stats(w http.ResponseWriter, r *http.Request) {
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}
	h.mu.Lock()
	out := make([]completion, 0, len(h.completions))
	for _, c := range h.completions {
		if c.EndedAt > since {
			out = append(out, c)
		}
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"now": time.Now().Unix(), "completions": out})
}

// sessionProxy is the PUBLIC entry to a session: it reverse-proxies /s/{id}/... to that
// session's backend on 127.0.0.1:<portBase> (stripping the /s/{id} prefix). The coordinator
// (the Cloudflare Worker) proxies visitor traffic here. A reaped/unknown id returns 410 Gone
// so the coordinator knows to hand that visitor a fresh session. SSE is streamed immediately
// (FlushInterval=-1). No key required — this is the visitor-facing surface; /v1/* stays gated.
func (h *host) sessionProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mu.Lock()
	s, ok := h.sessions[id]
	port, running := 0, false
	if ok {
		port, running = s.PortBase, s.State == "running"
	}
	h.mu.Unlock()

	if !ok {
		http.Error(w, "session not found (it may have ended) — request a new one", http.StatusGone)
		return
	}
	if !running {
		http.Error(w, "session is still starting", http.StatusServiceUnavailable)
		return
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	prefix := "/s/" + id
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1 // stream SSE (/api/stream) without buffering
	director := rp.Director
	rp.Director = func(req *http.Request) {
		director(req)
		req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		req.Host = target.Host
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "session backend unreachable", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

// ── serve ──────────────────────────────────────────────────────────────────────

func serveMain(args []string) {
	c, keyFile, devNoAuth := parseServeFlags(args)
	c.key = resolveKey(c.key, keyFile)
	if c.key == "" && !devNoAuth {
		die("no key: set --key-file (default secrets/demohost.key), $DEMOHOST_KEY, or pass --dev-no-auth for local testing")
	}

	h := newHost(c)
	if h.machineMB > 0 {
		fmt.Printf("demohost: machine has %d MB RAM; reserving %d MB to demos (%d MB/session, max %d)\n",
			h.machineMB, c.demoBudgetMB, c.sessionMB, c.maxSessions)
		if c.demoBudgetMB > h.machineMB {
			fmt.Printf("demohost: WARNING — demo budget %d MB exceeds machine RAM %d MB\n", c.demoBudgetMB, h.machineMB)
		}
	}

	// Build the shared binaries once up front so the first session isn't slow / racy.
	fmt.Println("demohost: building shared demo binaries (hack/demo-build.sh)…")
	build := exec.Command("bash", filepath.Join(c.repo, "hack", "demo-build.sh"))
	build.Dir = c.repo
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		die("demo-build.sh failed: " + err.Error())
	}

	// Reap any clusters/state leaked by a previous demohost PID before we start warming —
	// a fresh boot has no live sessions, so they're all unreachable zombies hogging the
	// Docker VM. (Without this, hard restarts pile up orphans until creates fail.)
	h.sweepOrphans()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go h.reaperLoop(ctx)
	if c.warmPool > 0 {
		fmt.Printf("demohost: warm pool = %d (settle %s, recycle >%s)\n", c.warmPool, c.warmSettle, c.warmMaxAge)
		go h.poolLoop(ctx)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", h.auth(h.createSession))
	mux.HandleFunc("GET /v1/sessions", h.auth(h.listSessions))
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.auth(h.deleteSession))
	mux.HandleFunc("GET /v1/capacity", h.auth(h.capacity))
	mux.HandleFunc("GET /v1/stats", h.auth(h.stats))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	// public visitor surface — reverse-proxy to the session's backend (used by the coordinator)
	mux.HandleFunc("/s/{id}/", h.sessionProxy)
	mux.HandleFunc("/s/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
	})

	// Bound header-read + idle time and header size. NO ReadTimeout/WriteTimeout: the
	// /s/{id} proxy carries the session's long-lived SSE stream, which a WriteTimeout
	// would sever.
	srv := &http.Server{
		Addr:              c.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go func() {
		<-ctx.Done()
		fmt.Println("\ndemohost: shutting down — reaping all sessions")
		h.reapAll()
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shctx)
	}()

	fmt.Printf("demohost: listening on %s (auth %s)\n", c.addr, authMode(c.key, devNoAuth))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		die("listen: " + err.Error())
	}
	fmt.Println("demohost: stopped")
}

// ── small helpers ──────────────────────────────────────────────────────────────

func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// backendExpired polls a session backend's /api/session. Returns (expired, reachable).
func backendExpired(portBase int) (bool, bool) {
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/api/session", portBase))
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	var st struct {
		Expired bool `json:"expired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return false, true
	}
	return st.Expired, true
}

// waitForSettle polls a warming session's backend until its fleet has no pending or still-
// provisioning nodes for two consecutive checks (the busy baseline has fully converged), or
// `max` elapses — so a heavier busy baseline isn't handed to a visitor mid-provision.
func waitForSettle(portBase int, max time.Duration) {
	deadline := time.Now().Add(max)
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		pokeHeartbeat(portBase)
		// Converged = the busy baseline has actually come up (nodes > 0) AND nothing is pending.
		// Requiring nodes>0 avoids a false-early "0 pending" in the window after seedBusyFleet
		// patches replica counts but before the ReplicaSet controller has created any pods.
		if p, nodes, ok := backendPending(portBase); ok && p == 0 && nodes > 0 {
			if stable++; stable >= 2 {
				return
			}
		} else {
			stable = 0
		}
	}
}

// backendPending returns (pending+provisioning, ready+provisioning nodes) across the session's
// fleet via the backend's /api/state snapshot. pending==0 && nodes>0 means converged.
func backendPending(portBase int) (pending, nodes int, ok bool) {
	cl := &http.Client{Timeout: 2 * time.Second}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", portBase))
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, false
	}
	var st struct {
		Clusters []struct {
			Nodes        int `json:"nodes"`
			PodsPending  int `json:"podsPending"`
			Provisioning int `json:"provisioning"`
		} `json:"clusters"`
	}
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return 0, 0, false
	}
	for _, c := range st.Clusters {
		pending += c.PodsPending + c.Provisioning
		nodes += c.Nodes
	}
	return pending, nodes, true
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func authMode(key string, devNoAuth bool) string {
	if devNoAuth && key == "" {
		return "DISABLED (dev)"
	}
	return "on"
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "demohost: "+msg)
	os.Exit(1)
}
