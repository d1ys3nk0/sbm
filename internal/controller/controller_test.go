package controller

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		now:   func() time.Time { return time.Unix(1000, 0) },
		run:   func(_ time.Duration, _, _ string, _ ...string) result { return result{} },
		fetch: func(string, string) (inputs.Snapshot, error) { return inputs.Snapshot{}, errors.New("offline") },
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
	c.run = func(_ time.Duration, _, _ string, _ ...string) result { return result{code: 1} }
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
	if len(array(active["outbounds"])) != 4 {
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" {
			return result{}
		}
		if executable == "/usr/bin/curl" {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, bodyCheckURL):
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
				return result{stdout: bodyCheckResult}
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

func TestWatchdogRestartsWhenBodyStalls(t *testing.T) {
	c := testController(t)
	generation := enable(t, c)
	if err := writeJSON(c.path("applied.json"), generation); err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Unix(1000, 0) }
	restarts := 0
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
		if executable == "/bin/launchctl" {
			if args[0] == "kickstart" {
				restarts++
			}
			return result{}
		}
		if executable == "/usr/bin/curl" {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, bodyCheckURL):
				return result{code: 28}
			case strings.Contains(joined, "17987"):
				return result{stdout: "204"}
			default:
				return result{stdout: "200"}
			}
		}
		return result{}
	}
	for i := 0; i < 3; i++ {
		if err := c.watchdog(); err != nil {
			t.Fatal(err)
		}
	}
	if restarts != 1 {
		t.Fatalf("got %d restarts, want one", restarts)
	}
	var health healthState
	if err := readJSON(c.path("health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if health.Healthy {
		t.Fatal("stalled body reported healthy")
	}
}

func TestReconcileFailsClosedOnProcessInspectionFailure(t *testing.T) {
	c := testController(t)
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
	c.run = func(_ time.Duration, _, executable string, args ...string) result {
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
