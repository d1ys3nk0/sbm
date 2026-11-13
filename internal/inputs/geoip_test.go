package inputs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type geoRoundTrip func(*http.Request) (*http.Response, error)

func (f geoRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func geoFixture(t *testing.T) (string, geoOptions, *int) {
	t.Helper()
	root := t.TempDir()
	if err := writeAtomic(filepath.Join(root, "policy.json"), []byte(`{"route":{"rule_set":[{"tag":"geoip-ru"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(root, "geoip-url.json"), []byte(`"https://rules.example.invalid/ru.srs"`)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	options := geoOptions{now: func() time.Time { return time.Unix(2000000000, 0) }, validate: func(data []byte) error {
		if !strings.HasPrefix(string(data), "valid-") {
			return errors.New("invalid fixture")
		}
		return nil
	}, client: &http.Client{Transport: geoRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("valid-new")), Header: http.Header{"Etag": []string{"v2"}}}, nil
	})}}
	return root, options, &calls
}

func geoRead(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func geoWrite(t *testing.T, root, name string, data []byte) {
	t.Helper()
	if err := writeAtomic(filepath.Join(root, name), data); err != nil {
		t.Fatal(err)
	}
}
func geoAssertNoJournal(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"geoip-activation.json", "geoip-previous.srs"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retained %s: %v", name, err)
		}
	}
}

func TestGeoIPActivationCommitsSuccessAfterConfirmation(t *testing.T) {
	root, options, _ := geoFixture(t)
	geoWrite(t, root, "ru.srs", []byte("valid-old"))
	activations := 0
	options.activate = func() error {
		activations++
		if string(geoRead(t, root, "ru.srs")) != "valid-new" {
			t.Fatal("activated wrong candidate")
		}
		var state geoState
		if err := readJSON(filepath.Join(root, "geoip.json"), &state); err != nil {
			t.Fatal(err)
		}
		if !state.Failed || state.LastSuccess != 0 {
			t.Fatal("reported success before activation")
		}
		return nil
	}
	if err := updateGeoIP(root, false, options); err != nil {
		t.Fatal(err)
	}
	var state geoState
	if err := readJSON(filepath.Join(root, "geoip.json"), &state); err != nil {
		t.Fatal(err)
	}
	if state.LastSuccess != float64(options.now().Unix()) || state.Failed || activations != 1 {
		t.Fatal("missing confirmed success")
	}
	geoAssertNoJournal(t, root)
}

func TestGeoIPActivationFailureRestoresAndReactivatesPreviousRules(t *testing.T) {
	root, options, _ := geoFixture(t)
	geoWrite(t, root, "ru.srs", []byte("valid-old"))
	activations := 0
	options.activate = func() error {
		activations++
		if activations == 1 {
			return errors.New("injected route check failure")
		}
		if string(geoRead(t, root, "ru.srs")) != "valid-old" {
			t.Fatal("rollback did not restore rules")
		}
		return nil
	}
	if err := updateGeoIP(root, false, options); err == nil {
		t.Fatal("activation failure hidden")
	}
	if activations != 2 {
		t.Fatal("rollback was not activated")
	}
	if string(geoRead(t, root, "ru.srs")) != "valid-old" {
		t.Fatal("candidate retained")
	}
	var state geoState
	if err := readJSON(filepath.Join(root, "geoip.json"), &state); err != nil {
		t.Fatal(err)
	}
	if state.LastSuccess != 0 || !state.Failed {
		t.Fatal("failure advanced success cadence")
	}
	geoAssertNoJournal(t, root)
}

func TestGeoIPRollbackFailureKeepsJournalAndRecoversBeforeRetry(t *testing.T) {
	root, options, calls := geoFixture(t)
	geoWrite(t, root, "ru.srs", []byte("valid-old"))
	options.activate = func() error { return errors.New("unreachable") }
	if err := updateGeoIP(root, false, options); err == nil {
		t.Fatal("rollback failure hidden")
	}
	if string(geoRead(t, root, "geoip-previous.srs")) != "valid-old" {
		t.Fatal("rollback backup lost")
	}
	_ = geoRead(t, root, "geoip-activation.json")
	recovered := false
	options.activate = func() error { recovered = true; return nil }
	if err := updateGeoIP(root, true, options); err != nil {
		t.Fatal(err)
	}
	if !recovered || *calls != 1 {
		t.Fatal("did not recover before applying retry cadence")
	}
	geoAssertNoJournal(t, root)
}

func TestGeoIPRecoveryRefusesUnexpectedDestination(t *testing.T) {
	root, options, _ := geoFixture(t)
	geoWrite(t, root, "geoip-previous.srs", []byte("valid-old"))
	journal, _ := json.Marshal(geoActivation{PreviousExists: true, PreviousHash: digest([]byte("valid-old")), CandidateHash: digest([]byte("valid-new"))})
	geoWrite(t, root, "geoip-activation.json", journal)
	geoWrite(t, root, "ru.srs", []byte("externally replaced"))
	if err := updateGeoIP(root, false, options); err == nil {
		t.Fatal("overwrote unrelated rules")
	}
	if string(geoRead(t, root, "ru.srs")) != "externally replaced" {
		t.Fatal("changed unexpected destination")
	}
	_ = geoRead(t, root, "geoip-activation.json")
}

func TestGeoIPSevenDayCadenceAndHourlyFailureRetry(t *testing.T) {
	root, options, calls := geoFixture(t)
	if err := updateGeoIP(root, true, options); err != nil {
		t.Fatal(err)
	}
	now := options.now()
	options.now = func() time.Time { return now.Add(7*24*time.Hour - time.Second) }
	if err := updateGeoIP(root, true, options); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatal("retried successful check before seven days")
	}
	options.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	options.validate = func([]byte) error { return errors.New("invalid candidate") }
	if err := updateGeoIP(root, true, options); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	if *calls != 2 {
		t.Fatal("did not catch up at seven days")
	}
	options.now = func() time.Time { return now.Add(7*24*time.Hour + time.Hour - time.Second) }
	if err := updateGeoIP(root, true, options); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatal("retried failed check too soon")
	}
	options.now = func() time.Time { return now.Add(7*24*time.Hour + time.Hour) }
	options.validate = func([]byte) error { return nil }
	if err := updateGeoIP(root, true, options); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatal("did not retry failed check after one hour")
	}
}

func TestGeoIPExplicitLocalPolicyOverridesSharedPolicy(t *testing.T) {
	root, options, calls := geoFixture(t)
	geoWrite(t, root, "policy.json", []byte(`{}`))
	options.policy = map[string]any{"route": map[string]any{"rule_set": []any{map[string]any{"tag": "geoip-ru"}}}}
	if err := updateGeoIP(root, false, options); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatal("local GeoIP policy ignored")
	}
	options.policy = map[string]any{}
	if err := updateGeoIP(root, false, options); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatal("downloaded despite explicit policy without GeoIP")
	}
}

func TestGeoIPNotModifiedPreservesConditionalMetadata(t *testing.T) {
	root, options, _ := geoFixture(t)
	if err := updateGeoIP(root, false, options); err != nil {
		t.Fatal(err)
	}
	options.client = &http.Client{Transport: geoRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") != "v2" {
			t.Fatal("conditional validator lost")
		}
		return &http.Response{StatusCode: 304, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	for range 2 {
		if err := updateGeoIP(root, false, options); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGeoIPFirstActivationFailureRestoresAbsentFile(t *testing.T) {
	root, options, _ := geoFixture(t)
	calls := 0
	options.activate = func() error {
		calls++
		if calls == 1 {
			return errors.New("failed first activation")
		}
		if _, err := os.Stat(filepath.Join(root, "ru.srs")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("rollback did not restore absence")
		}
		return nil
	}
	if err := updateGeoIP(root, false, options); err == nil {
		t.Fatal("activation failure hidden")
	}
	if calls != 2 {
		t.Fatal("rollback not confirmed")
	}
	geoAssertNoJournal(t, root)
}
