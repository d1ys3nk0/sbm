package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

func TestSessionStopsOwnedChildAndClearsRuntime(t *testing.T) {
	c := testController(t)
	policy(t, c)
	path, err := c.sourcePath("office")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, inputs.Snapshot{URL: "https://example.org", Nodes: []map[string]any{{"type": "vless", "tag": "node", "server": "example.org"}}}); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(c.root, "test-sing-box")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"$1\" = run ]; then exec /bin/sleep 60; fi\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.bin = binary
	c.runCommand = func(_ context.Context, _, executable string, _ ...string) result {
		if executable == "/usr/bin/pgrep" {
			return result{code: 1}
		}
		return result{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.runSession(ctx, "office", func(state string) {
			if state == "running" {
				started <- struct{}{}
			}
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("session did not stop")
	}
	if _, err := os.Lstat(c.path("active.json")); !os.IsNotExist(err) {
		t.Fatal("runtime generation remained active")
	}
}

func TestStopCancelsRefreshAndDoesNotRestartSession(t *testing.T) {
	c := testController(t)
	policy(t, c)
	path, err := c.sourcePath("office")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, inputs.Snapshot{URL: "https://example.org", Nodes: []map[string]any{{"type": "vless", "tag": "node", "server": "example.org"}}}); err != nil {
		t.Fatal(err)
	}
	c.runCommand = func(_ context.Context, _, executable string, _ ...string) result {
		if executable == "/usr/bin/pgrep" {
			return result{code: 1}
		}
		return result{}
	}
	refresh := make(chan struct{})
	c.fetchContext = func(ctx context.Context, _, _ string) (inputs.Snapshot, error) {
		close(refresh)
		<-ctx.Done()
		return inputs.Snapshot{}, ctx.Err()
	}
	d := &daemon{ctx: context.Background(), root: c.root, controller: c, status: Status{State: "stopped"}, run: func(ctx context.Context, profile sessionProfile, state func(string)) error {
		return c.runSession(ctx, profile.Name, state)
	}}
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refresh:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not begin")
	}
	if _, _, err := d.handle(Request{Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	if desired, err := readIntent(c.root); err != nil || desired != "" {
		t.Fatalf("desired = %q, %v", desired, err)
	}
	if _, err := os.Lstat(c.path("active.json")); !os.IsNotExist(err) {
		t.Fatal("cancelled refresh published an active configuration")
	}
}
