package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

func testController(t *testing.T) *controller {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "subscriptions"), 0700); err != nil {
		t.Fatal(err)
	}
	return &controller{root: root, bin: "/test/sing-box", out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		now:        func() time.Time { return time.Unix(1000, 0) },
		runCommand: func(_ context.Context, _, _ string, _ ...string) result { return result{} },
		fetch:      func(string, string) (inputs.Snapshot, error) { return inputs.Snapshot{}, errors.New("offline") },
	}
}

func enable(t *testing.T, c *controller) string {
	t.Helper()
	generation := "main.1.json"
	if err := writeJSON(c.path(generation), map[string]any{"experimental": map[string]any{"clash_api": map[string]any{"secret": "test-secret"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(generation, c.path("active.json")); err != nil {
		t.Fatal(err)
	}
	return generation
}

func policy(t *testing.T, c *controller) {
	t.Helper()
	value := map[string]any{"outbounds": []any{map[string]any{"type": "selector", "tag": "PROXY"}, map[string]any{"type": "direct", "tag": "direct"}}}
	if err := writeJSON(c.path("policy.json"), value); err != nil {
		t.Fatal(err)
	}
}

func TestPublishPreservesActiveOnRejectedCandidate(t *testing.T) {
	c := testController(t)
	before := enable(t, c)
	c.runCommand = func(_ context.Context, _, _ string, _ ...string) result { return result{code: 1} }
	if _, err := c.publish("other", map[string]any{}, false); err == nil {
		t.Fatal("expected validation failure")
	}
	after, err := c.active()
	if err != nil || after != before {
		t.Fatalf("active generation = %q, %v", after, err)
	}
}

func TestPublishMakesValidatedGenerationActive(t *testing.T) {
	c := testController(t)
	enable(t, c)
	generation, err := c.publish("other", map[string]any{"outbounds": []any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	active, err := c.active()
	if err != nil || active != generation {
		t.Fatalf("active generation = %q, %v", active, err)
	}
	var config map[string]any
	if err := readJSON(c.path("active.json"), &config); err != nil || config["outbounds"] == nil {
		t.Fatalf("invalid active config: %v", err)
	}
}

func TestConnectUsesCachedSubscriptionOnFetchFailure(t *testing.T) {
	c := testController(t)
	policy(t, c)
	snapshot := inputs.Snapshot{URL: "https://example.test/private", Nodes: []map[string]any{{"type": "vless", "tag": "node"}}}
	if err := writeJSON(c.path("subscriptions/main.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" && args[0] == "print" {
			return result{code: 113}
		}
		if executable == "/usr/bin/pgrep" {
			return result{code: 1}
		}
		return result{}
	}
	if err := c.connect("main"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.errOut.(*bytes.Buffer).String(), "last saved") {
		t.Fatal("fallback was not reported")
	}
	generation, err := c.active()
	if err != nil || generation == "" {
		t.Fatalf("no active generation: %v", err)
	}
	var active map[string]any
	if err := readJSON(c.path(generation), &active); err != nil {
		t.Fatal(err)
	}
	if len(array(active["outbounds"])) != 6 {
		t.Fatalf("unexpected outbounds: %v", active["outbounds"])
	}
}

func TestConnectRejectsConcurrentSubscriptionChange(t *testing.T) {
	c := testController(t)
	snapshot := inputs.Snapshot{URL: "https://example.test/private", Nodes: []map[string]any{{"tag": "original"}}}
	path := c.path("subscriptions/main.json")
	if err := writeJSON(path, snapshot); err != nil {
		t.Fatal(err)
	}
	c.fetch = func(string, string) (inputs.Snapshot, error) {
		changed := snapshot
		changed.URL = "https://example.test/new"
		if err := writeJSON(path, changed); err != nil {
			t.Fatal(err)
		}
		return snapshot, nil
	}
	if err := c.connect("main"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected concurrent edit error, got %v", err)
	}
	if generation, err := c.active(); generation != "" || err != nil {
		t.Fatalf("active = %q, %v", generation, err)
	}
}

func TestWatchdogDoesNotRestartAfterManualStop(t *testing.T) {
	c := testController(t)
	generation := enable(t, c)
	if err := writeJSON(c.path("applied.json"), generation); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("health.json"), healthState{Generation: generation, Failures: 2}); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" {
			if args[0] == "kickstart" {
				restarts++
			}
			return result{}
		}
		if executable == "/usr/bin/curl" && strings.Contains(strings.Join(args, " "), "17987") {
			if err := os.Remove(c.path("active.json")); err != nil {
				t.Fatal(err)
			}
			return result{code: 1}
		}
		if executable == "/usr/bin/curl" {
			return result{stdout: "200"}
		}
		return result{}
	}
	if err := c.watchdog(); err != nil {
		t.Fatal(err)
	}
	if restarts != 0 {
		t.Fatalf("restarted %d times after stop", restarts)
	}
}

func TestWatchdogRestartThresholdAndCooldown(t *testing.T) {
	c := testController(t)
	generation := enable(t, c)
	if err := writeJSON(c.path("applied.json"), generation); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	restarts := 0
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" {
			if args[0] == "kickstart" {
				restarts++
			}
			return result{}
		}
		if executable == "/usr/bin/curl" {
			if strings.Contains(strings.Join(args, " "), "17987") {
				return result{code: 1}
			}
			return result{stdout: "200"}
		}
		return result{}
	}
	for i := 0; i < 6; i++ {
		if err := c.watchdog(); err != nil {
			t.Fatal(err)
		}
	}
	if restarts != 1 {
		t.Fatalf("got %d restarts, want one", restarts)
	}
	now = now.Add(301 * time.Second)
	if err := c.watchdog(); err != nil {
		t.Fatal(err)
	}
	if restarts != 2 {
		t.Fatalf("got %d restarts, want two after cooldown", restarts)
	}
}

func TestWatchdogFailsWhenBodyStalls(t *testing.T) {
	c := testController(t)
	generation := enable(t, c)
	if err := writeJSON(c.path("applied.json"), generation); err != nil {
		t.Fatal(err)
	}
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" {
			return result{}
		}
		if executable == "/usr/bin/curl" {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, bodyCheckURL), strings.Contains(joined, bodyFallbackURL):
				return result{code: 28}
			case strings.Contains(joined, "17987"):
				return result{stdout: "204"}
			default:
				return result{stdout: "200"}
			}
		}
		return result{}
	}
	if err := c.watchdog(); err != nil {
		t.Fatal(err)
	}
	var health healthState
	if err := readJSON(c.path("health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if health.Healthy || health.Failures != 1 {
		t.Fatalf("health = %+v", health)
	}
}

func TestWatchdogHealthyRequiresFullBodyDownload(t *testing.T) {
	c := testController(t)
	generation := enable(t, c)
	if err := writeJSON(c.path("applied.json"), generation); err != nil {
		t.Fatal(err)
	}
	var bodyURL, bodyFormat string
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" {
			return result{}
		}
		if executable == "/usr/bin/curl" {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, bodyCheckURL):
				bodyURL = args[len(args)-1]
				for i, arg := range args {
					if arg == "-w" && i+1 < len(args) {
						bodyFormat = args[i+1]
					}
				}
				return result{stdout: "200 32768"}
			case strings.Contains(joined, "17987"):
				return result{stdout: "204"}
			default:
				return result{stdout: "200"}
			}
		}
		return result{}
	}
	if err := c.watchdog(); err != nil {
		t.Fatal(err)
	}
	var health healthState
	if err := readJSON(c.path("health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if !health.Healthy || health.Failures != 0 {
		t.Fatalf("health = %+v", health)
	}
	if bodyURL != bodyCheckURL || bodyFormat != "%{http_code} %{size_download}" {
		t.Fatalf("body probe args: url=%q format=%q", bodyURL, bodyFormat)
	}
}

func TestWatchdogCorroboratesBodyFailure(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fallback result
		healthy  bool
		restarts int
	}{
		{name: "fallback succeeds", fallback: result{stdout: "206 32768"}, healthy: true},
		{name: "fallback stalls", fallback: result{code: 28, stdout: "206 5516"}, restarts: 1},
		{name: "fallback returns a short body", fallback: result{stdout: "206 4096"}, restarts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testController(t)
			generation := enable(t, c)
			if err := writeJSON(c.path("applied.json"), generation); err != nil {
				t.Fatal(err)
			}
			c.now = func() time.Time { return time.Unix(1000, 0) }
			restarts, fallbackCalls := 0, 0
			c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
				if executable == "/bin/launchctl" && args[0] == "kickstart" {
					restarts++
				}
				if executable != "/usr/bin/curl" {
					return result{}
				}
				switch args[len(args)-1] {
				case bodyCheckURL:
					return result{code: 28, stdout: "200 5516"}
				case checkURL:
					return result{stdout: "204"}
				case uplinkURL:
					return result{stdout: "200"}
				case bodyFallbackURL:
					fallbackCalls++
					joined := strings.Join(args, " ")
					for _, option := range []string{"--proxy http://127.0.0.1:17987", "--range 0-32767", "--max-filesize 32768"} {
						if !strings.Contains(joined, option) {
							t.Errorf("fallback probe is missing %q", option)
						}
					}
					return tt.fallback
				default:
					t.Errorf("unexpected curl target %q", args[len(args)-1])
					return result{}
				}
			}
			for i := 0; i < 3; i++ {
				if err := c.watchdog(); err != nil {
					t.Fatal(err)
				}
			}
			if restarts != tt.restarts || fallbackCalls != 3 {
				t.Fatalf("got %d restarts and %d fallback probes, want %d and 3", restarts, fallbackCalls, tt.restarts)
			}
			var health healthState
			if err := readJSON(c.path("health.json"), &health); err != nil {
				t.Fatal(err)
			}
			if health.Healthy != tt.healthy || health.Failures != 0 {
				t.Fatalf("health = %+v, want healthy=%t with no pending failures", health, tt.healthy)
			}
		})
	}
}

func TestReconcileFailsClosedOnProcessInspectionFailure(t *testing.T) {
	c := testController(t)
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" && args[0] == "print" {
			return result{code: 113}
		}
		if executable == "/usr/bin/pgrep" {
			return result{code: 3}
		}
		t.Fatalf("unexpected command: %s %v", executable, args)
		return result{}
	}
	if err := c.reconcile("main.1.json", false); err == nil {
		t.Fatal("expected process inspection error")
	}
}

func TestTailscaleRestoresSavedExitNode(t *testing.T) {
	c := testController(t)
	if err := writeJSON(c.path("tailscale-cli.json"), "/test/tailscale"); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("tailscale.json"), tailState{Exit: "100.64.0.1", LAN: true}); err != nil {
		t.Fatal(err)
	}
	var called string
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		called = executable + " " + strings.Join(args, " ")
		return result{}
	}
	if err := c.tailscale(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(called, "--exit-node=100.64.0.1 --exit-node-allow-lan-access=true") {
		t.Fatalf("unexpected command %q", called)
	}
	if _, err := os.Stat(c.path("tailscale.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state retained: %v", err)
	}
}

func TestConnectFallsBackWhenRefreshedConfigIsRejected(t *testing.T) {
	c := testController(t)
	policy(t, c)
	old := inputs.Snapshot{URL: "https://example.test/private", Nodes: []map[string]any{{"type": "vless", "tag": "old"}}}
	if err := writeJSON(c.path("subscriptions/main.json"), old); err != nil {
		t.Fatal(err)
	}
	c.fetch = func(string, string) (inputs.Snapshot, error) {
		return inputs.Snapshot{URL: old.URL, Nodes: []map[string]any{{"type": "vless", "tag": "new"}}}, nil
	}
	checks := 0
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == c.bin {
			checks++
			if checks == 1 {
				return result{code: 1}
			}
		}
		if executable == "/bin/launchctl" && args[0] == "print" {
			return result{code: 113}
		}
		if executable == "/usr/bin/pgrep" {
			return result{code: 1}
		}
		return result{}
	}
	if err := c.connect("main"); err != nil {
		t.Fatal(err)
	}
	if checks != 2 {
		t.Fatalf("validation count = %d", checks)
	}
	var stored inputs.Snapshot
	if err := readJSON(c.path("subscriptions/main.json"), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Nodes[0]["tag"] != "old" {
		t.Fatalf("saved unvalidated nodes: %v", stored.Nodes)
	}
	if !strings.Contains(c.errOut.(*bytes.Buffer).String(), "last saved") {
		t.Fatal("fallback was not reported")
	}
}

func TestTailscaleNullPrefsCannotDiscardRecoveryState(t *testing.T) {
	c := testController(t)
	if err := writeJSON(c.path("tailscale-cli.json"), "/test/tailscale"); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("tailscale.json"), tailState{Exit: "100.64.0.1", LAN: true}); err != nil {
		t.Fatal(err)
	}
	c.runCommand = func(_ context.Context, _, executable string, args ...string) result {
		if executable == "/usr/bin/env" {
			return result{stdout: "null"}
		}
		return result{code: 1}
	}
	if err := c.tailscale(true); err == nil {
		t.Fatal("expected invalid preferences failure")
	}
	if _, err := os.Stat(c.path("tailscale.json")); err != nil {
		t.Fatalf("lost recovery state: %v", err)
	}
}

func TestReconcileDoesNotRewriteUnchangedAppliedState(t *testing.T) {
	c := testController(t)
	if err := c.reconcile("", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.path("applied.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle state was written: %v", err)
	}
}

func TestWatchdogSwitchesOnlyToVerifiedNodes(t *testing.T) {
	c := testController(t)
	policy(t, c)
	config, err := c.build([]map[string]any{
		{"type": "vless", "tag": "first"},
		{"type": "vless", "tag": "second"},
		{"type": "vless", "tag": "third"},
		{"type": "vless", "tag": "fourth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := c.publish("main", config, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("applied.json"), generation); err != nil {
		t.Fatal(err)
	}
	active, candidate := "first", ""
	healthyNode := "second"
	c.runCommand = func(ctx context.Context, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" && args[0] == "kickstart" {
			t.Fatal("node failure restarted sing-box")
		}
		if executable != "/usr/bin/curl" {
			return result{}
		}
		target := args[len(args)-1]
		if strings.Contains(target, "/proxies/") {
			if !strings.Contains(strings.Join(args, " "), "--request PUT") {
				return result{stdout: fmt.Sprintf(`{"now":%q}`, active)}
			}
			var node string
			for i, arg := range args {
				if arg == "--data" {
					var selection struct{ Name string }
					if err := json.Unmarshal([]byte(args[i+1]), &selection); err != nil {
						t.Fatal(err)
					}
					node = selection.Name
				}
			}
			if strings.HasSuffix(target, "/PROXY") {
				if node != "sbm-unavailable" && node != healthyNode {
					t.Fatalf("routed user traffic through unverified node %q", node)
				}
				active = node
			} else {
				candidate = node
			}
			return result{}
		}
		if target == bodyCheckURL || target == bodyFallbackURL {
			if _, ok := ctx.Deadline(); !ok || !strings.Contains(strings.Join(args, " "), "--max-time 3") {
				t.Error("body probe does not enforce three seconds")
			}
			if candidate != healthyNode {
				return result{code: 28}
			}
			if target == bodyFallbackURL {
				return result{stdout: "206 32768"}
			}
			return result{stdout: "200 32768"}
		}
		if target == checkURL {
			return result{stdout: "204"}
		}
		return result{stdout: "200"}
	}
	for _, step := range []struct {
		healthyNode string
		want        string
	}{
		{"second", "second"},
		{"second", "second"},
		{"", "sbm-unavailable"},
		{"third", "sbm-unavailable"},
		{"third", "third"},
	} {
		healthyNode = step.healthyNode
		if err := c.watchdog(); err != nil {
			t.Fatal(err)
		}
		if active != step.want {
			t.Fatalf("selected %q, want %q", active, step.want)
		}
	}
}

func TestBodyProbeUsesSharedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testController(t)
		started := make(chan time.Time, 2)
		c.runCommand = func(ctx context.Context, _, _ string, _ ...string) result {
			deadline, _ := ctx.Deadline()
			started <- deadline
			<-ctx.Done()
			return result{code: 28}
		}

		start := time.Now()
		if c.bodyProbe("test-secret") {
			t.Fatal("two stalled downloads passed")
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Errorf("body probe took %s, want 3s", elapsed)
		}
		for range 2 {
			if deadline := <-started; !deadline.Equal(start.Add(3 * time.Second)) {
				t.Errorf("download deadline = %v, want %v", deadline, start.Add(3*time.Second))
			}
		}
	})
}

func TestBodyProbeCancelsOtherDownload(t *testing.T) {
	for _, winner := range []struct{ url, output string }{
		{bodyCheckURL, "200 32768"},
		{bodyFallbackURL, "206 32768"},
	} {
		t.Run(winner.output, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := testController(t)
				otherStarted := make(chan struct{})
				otherDone := make(chan error, 1)
				c.runCommand = func(ctx context.Context, _, _ string, args ...string) result {
					if args[len(args)-1] == winner.url {
						select {
						case <-otherStarted:
							return result{stdout: winner.output}
						case <-ctx.Done():
							return result{code: 28}
						}
					}
					close(otherStarted)
					<-ctx.Done()
					time.Sleep(time.Millisecond)
					otherDone <- ctx.Err()
					return result{code: 28}
				}

				if !c.bodyProbe("test-secret") {
					t.Fatal("complete download did not pass")
				}
				select {
				case err := <-otherDone:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("second request ended with %v, want cancellation", err)
					}
				default:
					t.Error("body probe returned before the other download exited")
				}
			})
		})
	}
}
