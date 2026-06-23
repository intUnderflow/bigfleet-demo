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
}

// ── session + host ─────────────────────────────────────────────────────────────

type session struct {
	ID         string    `json:"id"`
	PortBase   int       `json:"portBase"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"` // hard cap = CreatedAt + ttl
	ReservedMB int       `json:"reservedMB"`
	State      string    `json:"state"` // starting | running | reaping

	lastSeenOK time.Time // last successful backend poll (reaper grace clock)
}

type host struct {
	cfg config

	mu        sync.Mutex
	sessions  map[string]*session
	usedBases map[int]bool

	machineMB    int
	memActualMB  int // last measured actual demo RSS (0 = unknown/not yet sampled)
	memSampledAt time.Time
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
	n := h.activeCountLocked()
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
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"reaped": id})
}

type capacityView struct {
	MachineTotalMB   int     `json:"machineTotalMB"`
	DemoBudgetMB     int     `json:"demoBudgetMB"`
	PerSessionMB     int     `json:"perSessionMB"`
	RunningSessions  int     `json:"runningSessions"`
	MaxSessions      int     `json:"maxSessions"`
	ReservedMB       int     `json:"reservedMB"`
	MeasuredActualMB int     `json:"measuredActualMB"`
	MeasuredAgeSec   int     `json:"measuredAgeSec"`
	HeadroomSessions int     `json:"headroomSessions"`
	BudgetUsedPct    float64 `json:"budgetUsedPct"`
}

func (h *host) capacityView() capacityView {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.activeCountLocked()
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
	age := 0
	if !h.memSampledAt.IsZero() {
		age = int(time.Since(h.memSampledAt).Seconds())
	}
	return capacityView{
		MachineTotalMB: h.machineMB, DemoBudgetMB: h.cfg.demoBudgetMB, PerSessionMB: h.cfg.sessionMB,
		RunningSessions: n, MaxSessions: h.cfg.maxSessions, ReservedMB: reserved,
		MeasuredActualMB: h.memActualMB, MeasuredAgeSec: age, HeadroomSessions: headroom,
		BudgetUsedPct: pct(reserved, h.cfg.demoBudgetMB),
	}
}

func (h *host) capacity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.capacityView())
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go h.reaperLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", h.auth(h.createSession))
	mux.HandleFunc("GET /v1/sessions", h.auth(h.listSessions))
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.auth(h.deleteSession))
	mux.HandleFunc("GET /v1/capacity", h.auth(h.capacity))
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
