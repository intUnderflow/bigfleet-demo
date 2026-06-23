package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// clientMain implements the create/ls/capacity/rm subcommands: thin authenticated calls to
// a running `demohost serve`. The key resolves the same way the daemon's does.
func clientMain(sub string, args []string) {
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	server := fs.String("server", envOr("DEMOHOST_SERVER", "http://localhost:8080"), "demohost serve address")
	key := fs.String("key", "", "coordinator key inline (prefer $DEMOHOST_KEY or --key-file)")
	keyFile := fs.String("key-file", "secrets/demohost.key", "file holding the coordinator key")
	_ = fs.Parse(args)

	k := resolveKey(*key, *keyFile)
	if k == "" {
		die("no key: set $DEMOHOST_KEY, --key, or --key-file (default secrets/demohost.key)")
	}

	switch sub {
	case "create":
		var s session
		req("POST", *server+"/v1/sessions", k, &s)
		fmt.Printf("session %s started\n  url:     %s\n  expires: %s (~%s from now)\n",
			s.ID, s.URL, s.ExpiresAt.Format(time.RFC3339), time.Until(s.ExpiresAt).Round(time.Minute))
	case "ls":
		var out struct {
			Sessions []session `json:"sessions"`
		}
		req("GET", *server+"/v1/sessions", k, &out)
		if len(out.Sessions) == 0 {
			fmt.Println("no live sessions")
			return
		}
		fmt.Printf("%-6s %-6s %-28s %-9s %s\n", "ID", "PORT", "URL", "STATE", "ENDS IN")
		for _, s := range out.Sessions {
			fmt.Printf("%-6s %-6d %-28s %-9s %s\n", s.ID, s.PortBase, s.URL, s.State,
				time.Until(s.ExpiresAt).Round(time.Second))
		}
	case "capacity":
		var v capacityView
		req("GET", *server+"/v1/capacity", k, &v)
		printCapacity(v)
	case "rm":
		id := fs.Arg(0)
		if id == "" {
			die("usage: demohost rm <id>")
		}
		var out map[string]string
		req("DELETE", *server+"/v1/sessions/"+id, k, &out)
		fmt.Println("reaped", id)
	}
}

func printCapacity(v capacityView) {
	fmt.Printf("machine RAM:         %6d MB\n", v.MachineTotalMB)
	fmt.Printf("demo budget:         %6d MB\n", v.DemoBudgetMB)
	fmt.Printf("per-session reserve: %6d MB\n", v.PerSessionMB)
	fmt.Printf("running sessions:    %6d / %d max\n", v.RunningSessions, v.MaxSessions)
	fmt.Printf("reserved:            %6d MB  (%.0f%% of budget)\n", v.ReservedMB, v.BudgetUsedPct)
	if v.MeasuredActualMB > 0 {
		fmt.Printf("measured actual:     %6d MB  (sampled %ds ago)\n", v.MeasuredActualMB, v.MeasuredAgeSec)
	} else {
		fmt.Printf("measured actual:          —  (not yet sampled)\n")
	}
	fmt.Printf("headroom:            %6d more session(s) fit\n", v.HeadroomSessions)
}

// req does an authenticated JSON request and decodes into out (nil to ignore). On a non-2xx
// it dies with the server's {"error"} message. The timeout exceeds the spawn budget so a
// `create` that takes a while to bring a stack up doesn't time out client-side.
func req(method, url, key string, out any) {
	r, _ := http.NewRequest(method, url, nil)
	r.Header.Set("X-Demo-Key", key)
	resp, err := (&http.Client{Timeout: spawnTimeout + 30*time.Second}).Do(r)
	if err != nil {
		die("request failed: " + err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			die(fmt.Sprintf("server %d: %s", resp.StatusCode, e.Error))
		}
		die(fmt.Sprintf("server %d: %s", resp.StatusCode, lastLines(string(body), 3)))
	}
	if out != nil {
		_ = json.Unmarshal(body, out)
	}
}
