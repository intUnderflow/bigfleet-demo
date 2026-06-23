package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// machineTotalMB returns physical RAM in MB (darwin via sysctl, linux via /proc/meminfo),
// or 0 if it can't tell — the budget math degrades gracefully to reservation-only.
func machineTotalMB() int {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if b, e := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); e == nil {
				return int(b / 1024 / 1024)
			}
		}
	case "linux":
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, ln := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(ln, "MemTotal:") {
					if f := strings.Fields(ln); len(f) >= 2 {
						if kb, e := strconv.Atoi(f[1]); e == nil {
							return kb / 1024
						}
					}
				}
			}
		}
	}
	return 0
}

// measureDemoMB is the MEASURED actual footprint of the live sessions: host-process RSS
// (the shard/operator/providers/backend per session) + the kwok docker containers. It's a
// best-effort cross-check on the reservation math; 0 means "couldn't measure" and the
// caller falls back to reservation-only admission.
func measureDemoMB(repo string, ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	return hostRSSMB(repo, ids) + dockerMB(ids)
}

// hostRSSMB sums RSS of every pid recorded in run/sessions/<id>/pids.
func hostRSSMB(repo string, ids []string) int {
	var pids []string
	for _, id := range ids {
		b, err := os.ReadFile(filepath.Join(repo, "run", "sessions", id, "pids"))
		if err != nil {
			continue
		}
		pids = append(pids, strings.Fields(string(b))...)
	}
	if len(pids) == 0 {
		return 0
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strings.Join(pids, ",")).Output()
	if err != nil {
		return 0
	}
	totalKB := 0
	for _, ln := range strings.Fields(string(out)) {
		if kb, e := strconv.Atoi(ln); e == nil {
			totalKB += kb
		}
	}
	return totalKB / 1024
}

// dockerMB sums the memory of this session's kwok containers (named kwok-<id>-cluster-*).
func dockerMB(ids []string) int {
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return 0
	}
	idset := map[string]bool{}
	for _, id := range ids {
		idset[id] = true
	}
	var names []string
	for _, n := range strings.Fields(string(out)) {
		rest, ok := strings.CutPrefix(n, "kwok-")
		if !ok {
			continue
		}
		if i := strings.Index(rest, "-cluster"); i > 0 && idset[rest[:i]] {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return 0
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{.MemUsage}}"}, names...)
	so, err := exec.Command("docker", args...).Output()
	if err != nil {
		return 0
	}
	total := 0.0
	for _, ln := range strings.Split(strings.TrimSpace(string(so)), "\n") {
		if ln == "" {
			continue
		}
		total += parseMemMB(strings.SplitN(ln, "/", 2)[0]) // "123MiB / 7GiB" -> used
	}
	return int(total)
}

// parseMemMB converts a docker MemUsage figure like "123.4MiB" or "1.2GiB" to MB.
func parseMemMB(s string) float64 {
	s = strings.TrimSpace(s)
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "GiB"):
		mult, s = 1024, strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "MiB"):
		mult, s = 1, strings.TrimSuffix(s, "MiB")
	case strings.HasSuffix(s, "KiB"):
		mult, s = 1.0/1024, strings.TrimSuffix(s, "KiB")
	case strings.HasSuffix(s, "GB"):
		mult, s = 1000, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "kB"):
		mult, s = 1.0/1024, strings.TrimSuffix(s, "kB")
	case strings.HasSuffix(s, "B"):
		mult, s = 1.0/(1024*1024), strings.TrimSuffix(s, "B")
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v * mult
}
