package controller

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

func TestImportProfileValidatesAndPreservesSourceOnRejection(t *testing.T) {
	c := testController(t)
	source := `{"route":{"final":"proxy"},"outbounds":[{"type":"selector","tag":"proxy","outbounds":["node"]},{"type":"vless","tag":"node","server":"example.org","uuid":"private"}]}`
	if err := c.importProfile("office", strings.NewReader(source)); err != nil {
		t.Fatal(err)
	}
	path, err := c.sourcePath("office")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.check("office"); err != nil {
		t.Fatal(err)
	}
	afterCheck, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, afterCheck) {
		t.Fatal("check changed saved profile")
	}
	if err := c.importProfile("office", strings.NewReader(`{"outbounds":[]}`)); err == nil {
		t.Fatal("invalid import succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected import changed saved profile")
	}
	var profile savedProfile
	if err := readJSON(path, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Policy["route"].(map[string]any)["final"] != "proxy" || profile.Nodes[0]["uuid"] != "private" {
		t.Fatal(errors.New("saved profile lost settings"))
	}
}

func TestImportAndSubscriptionRepeatWithoutOverwriting(t *testing.T) {
	c := testController(t)
	source := `{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"}]}`
	if err := c.importProfile("local", strings.NewReader(source)); err != nil {
		t.Fatal(err)
	}
	if err := c.importProfile("local", strings.NewReader(source)); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(source, `"server":"x"`, `"server":"y"`, 1)
	if err := c.importProfile("local", strings.NewReader(changed)); err == nil {
		t.Fatal("different local profile replaced existing profile")
	}
	path, err := c.sourcePath("remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, inputs.Snapshot{URL: "https://example.org/source", Nodes: []map[string]any{{"tag": "n"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.subscribe("remote", "https://example.org/source"); err != nil {
		t.Fatal(err)
	}
	if err := c.subscribe("remote", "https://example.org/other"); err == nil {
		t.Fatal("different subscription replaced existing profile")
	}
}
