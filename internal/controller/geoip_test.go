package controller

import (
	"context"
	"testing"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

func TestIdleGeoIPUsesLocalProfilePolicy(t *testing.T) {
	c := testController(t)
	profile := savedProfile{Snapshot: inputs.Snapshot{Nodes: []map[string]any{{"tag": "node"}}}, Policy: map[string]any{"route": map[string]any{"rule_set": []any{map[string]any{"tag": "geoip-ru"}}}}}
	path, err := c.sourcePath("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, profile); err != nil {
		t.Fatal(err)
	}
	called := false
	c.geoipUpdate = func(_ context.Context, scheduled bool, policy map[string]any, activate func() error) error {
		called = true
		if !scheduled || activate != nil || policy == nil {
			t.Fatal("idle update did not receive the local policy")
		}
		route, ok := policy["route"].(map[string]any)
		if !ok || route["rule_set"] == nil {
			t.Fatal("local GeoIP rule set was not selected")
		}
		return nil
	}
	if err := c.updateIdleGeoIP(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("GeoIP updater was not called")
	}
}
