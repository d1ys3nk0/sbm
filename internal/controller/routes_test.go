package controller

import (
	"context"
	"testing"
)

func TestConfiguredRouteChecks(t *testing.T) {
	c := testController(t)
	if err := c.checkRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("route-checks.json"), []routeCheck{{Name: "direct", URL: "https://example.org"}, {Name: "tailnet", URL: "http://example.internal/health", ExpectedStatus: 204}}); err != nil {
		t.Fatal(err)
	}
	count := 0
	c.runCommand = func(_ context.Context, _, _ string, _ ...string) result {
		count++
		if count == 1 {
			return result{stdout: "200"}
		}
		return result{stdout: "204"}
	}
	if err := c.checkRoutes(context.Background()); err != nil || count != 2 {
		t.Fatalf("checks: %v, count %d", err, count)
	}
	c.runCommand = func(_ context.Context, _, _ string, _ ...string) result { return result{code: 1} }
	if err := c.checkRoutes(context.Background()); err == nil {
		t.Fatal("failed route accepted")
	}
}

func TestConfiguredRouteChecksRejectCredentialsAndDuplicateNames(t *testing.T) {
	c := testController(t)
	for _, checks := range [][]routeCheck{{{Name: "direct", URL: "https://user:password@example.org"}}, {{Name: "same", URL: "https://example.org"}, {Name: "same", URL: "https://example.org"}}} {
		if err := writeJSON(c.path("route-checks.json"), checks); err != nil {
			t.Fatal(err)
		}
		if err := c.checkRoutes(context.Background()); err == nil {
			t.Fatal("accepted invalid checks")
		}
	}
}
