package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// parseServeFlags parses `demohost serve` flags into a config, plus the key-file path and
// the dev-no-auth toggle (kept out of config because they're resolved into config.key).
func parseServeFlags(args []string) (config, string, bool) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c config
	var keyFile string
	var devNoAuth bool
	fs.StringVar(&c.addr, "addr", ":8080", "host API listen address")
	fs.StringVar(&c.repo, "repo", ".", "path to the bigfleet-demo repo (where hack/demo-*.sh live)")
	fs.StringVar(&c.advertise, "advertise-host", "localhost", "host part used in returned session URLs")
	fs.StringVar(&keyFile, "key-file", "secrets/demohost.key", "file holding the coordinator secret key")
	fs.StringVar(&c.key, "key", "", "coordinator secret key inline (prefer --key-file or $DEMOHOST_KEY)")
	fs.BoolVar(&devNoAuth, "dev-no-auth", false, "DISABLE the key requirement — local testing only")
	fs.IntVar(&c.portBase, "port-base", 21000, "first session port-block base")
	fs.IntVar(&c.stride, "port-stride", 16, "ports reserved per session")
	fs.IntVar(&c.maxSessions, "max-sessions", 8, "hard cap on concurrent sessions (backstop)")
	fs.IntVar(&c.demoBudgetMB, "demo-memory-mb", 16384, "TOTAL memory reserved to demo sessions (MB) — admission never exceeds this")
	fs.IntVar(&c.sessionMB, "session-memory-mb", 2048, "memory reserved per session (MB); measured ~1.9 GB/session on the dev Mac")
	fs.DurationVar(&c.ttl, "session-ttl", time.Hour, "hard session lifetime")
	fs.DurationVar(&c.idle, "idle-timeout", 5*time.Minute, "reap a session this long after its tab stops heart-beating")
	fs.BoolVar(&c.dashboards, "dashboards", false, "run per-cluster k8s dashboards in each session (heavier)")
	_ = fs.Parse(args)
	if abs, err := filepath.Abs(c.repo); err == nil {
		c.repo = abs
	}
	return c, keyFile, devNoAuth
}

// resolveKey picks the coordinator key: an explicit value wins, then $DEMOHOST_KEY (what CI
// injects from the GitHub Actions secret), then the key file (the local default). Whitespace
// is trimmed so a stray newline in a file/secret never corrupts the comparison.
func resolveKey(inline, keyFile string) string {
	if v := strings.TrimSpace(inline); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("DEMOHOST_KEY")); v != "" {
		return v
	}
	if keyFile != "" {
		if b, err := os.ReadFile(keyFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
