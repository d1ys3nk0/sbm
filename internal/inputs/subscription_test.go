package inputs

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSubscriptionURIAndBase64(t *testing.T) {
	raw := "vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&sni=site.example&type=ws&path=%2Fweb#Hello\ntrojan://pw@example.org:443#Second\ninvalid://unsafe-secret\n"
	plain, err := parseSubscription(raw)
	if err != nil || len(plain) != 2 {
		t.Fatalf("plain: %v %v", len(plain), err)
	}
	encoded, err := parseSubscription(base64.RawURLEncoding.EncodeToString([]byte(raw)))
	if err != nil || len(encoded) != 2 {
		t.Fatalf("encoded: %v %v", len(encoded), err)
	}
	for i := range plain {
		if plain[i].outbound["tag"] != encoded[i].outbound["tag"] {
			t.Fatal("different tags")
		}
	}
	if plain[0].label != "Hello" || plain[1].label != "Second" {
		t.Fatalf("wrong labels: %#v", plain)
	}
}
func TestNodeTagMatchesPythonCanonicalJSON(t *testing.T) {
	// Calculated with the original Python serializer, including its ASCII escaping.
	node := map[string]any{"type": "vless", "uuid": "123e4567-e89b-12d3-a456-426614174000", "server": "example.com", "server_port": 443, "flow": "你好"}
	result := makeEntry(node, "node")
	const expected = "sub-a16fbfcc9c54fdba75cd4136d07b60f7839f15c8816c6a32477c782068c8155b"
	if got := result.outbound["tag"]; got != expected {
		t.Fatalf("got %v want %v", got, expected)
	}
}
func TestJSONFiltersUnsafeAndDeduplicates(t *testing.T) {
	raw := `{"outbounds":[{"type":"vless","server":"ok","tag":"a"},{"type":"vless","server":"ok","tag":"duplicate"},{"type":"vless","server":"bad","tls":{"certificate_path":"/tmp/secret"}},{"type":"vless","server":"bad","detour":"external"},{"type":"selector","tag":"group"}]}`
	entries, err := parseSubscription(raw)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v err %v", entries, err)
	}
	if entries[0].label != "a" {
		t.Fatal(entries[0].label)
	}
}
func TestFetchRejectsUnsafeURLsWithoutEchoingSecrets(t *testing.T) {
	for _, u := range []string{"http://example.com/private-secret", "https://secret@example.com/private-secret", "https://"} {
		_, err := FetchSubscription(u, "/missing")
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error %v", err)
		}
	}
}
func TestUpdateGeoIPWithoutPolicyIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := UpdateGeoIP(dir, "/missing", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "geoip.lock")); !os.IsNotExist(err) {
		t.Fatal("unexpected update")
	}
}
func TestGeoIPValidationRequiresBothAddressFamilies(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sing-box")
	contents := "#!/bin/sh\nprintf '%s' '{\"version\":1,\"rules\":[{\"ip_cidr\":[\"192.0.2.0/24\",\"2001:db8::/32\"]}]}' > \"$5\"\n"
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateGeoIP([]byte("sample"), dir, script); err != nil {
		t.Fatal(err)
	}
	contents = strings.ReplaceAll(contents, `,"2001:db8::/32"`, "")
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateGeoIP([]byte("sample"), dir, script); err == nil {
		t.Fatal("expected rejection")
	}
}
func TestSnapshotJSONCompatibility(t *testing.T) {
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(`{"url":"https://provider","nodes":[{"type":"vless","tag":"sub-a"}],"labels":{"sub-a":"label"}}`), &snapshot); err != nil || snapshot.Labels["sub-a"] != "label" {
		t.Fatalf("%+v %v", snapshot, err)
	}
}

func TestRejectedGeoIPSourcePreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(`{"route":{"rule_set":[{"tag":"geoip-ru"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "geoip-url.json"), []byte(`"http://example.com/secret"`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ru.srs")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	err := UpdateGeoIP(dir, "/missing", false)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing" {
		t.Fatalf("lost existing GeoIP: %q %v", data, err)
	}
}
func TestVLESSUUIDWithoutHyphens(t *testing.T) {
	e, err := parseURI("vless://123e4567e89b12d3a456426614174000@example.com:443")
	if err != nil || e.outbound["uuid"] != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestSubscriptionTransportProtocols(t *testing.T) {
	cases := []struct {
		uri, typ string
		fields   map[string]any
	}{
		{"vless://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee@one.example:443?security=reality&pbk=key&sid=abcd&fp=chrome&sni=front.example&flow=xtls-rprx-vision", "vless", map[string]any{"flow": "xtls-rprx-vision"}},
		{"trojan://secret@two.example:8443?type=ws&path=%2Fsocket&host=front.example&allowInsecure=1", "trojan", map[string]any{"password": "secret"}},
		{"hysteria2://user%3Apass%40word@[2001:db8::1]/#Hy", "hysteria2", map[string]any{"password": "user:pass@word", "server": "2001:db8::1", "server_port": 443}},
		{"hy2://user:pass%40word@[2001:db8::1]:443/#Alias", "hysteria2", map[string]any{"password": "user:pass@word", "server": "2001:db8::1", "server_port": 443}},
	}
	for _, tc := range cases {
		entry, err := parseURI(tc.uri)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.typ, err)
		}
		if entry.outbound["type"] != tc.typ {
			t.Fatal(entry.outbound)
		}
		for key, want := range tc.fields {
			if entry.outbound[key] != want {
				t.Fatalf("%s %s: %v", tc.typ, key, entry.outbound[key])
			}
		}
	}
	credential := "aes-256-gcm:synthetic-password"
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	one, err := parseURI("ss://" + encode(credential) + "@ss.example:443")
	if err != nil {
		t.Fatal(err)
	}
	two, err := parseURI("ss://" + encode(credential+"@ss.example:443"))
	if err != nil || one.outbound["tag"] != two.outbound["tag"] {
		t.Fatalf("shadowsocks identity %v", err)
	}
}

func TestFetchSubscriptionHTTPSValidatesNodes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "sing-box" {
			t.Errorf("user agent %q", r.UserAgent())
		}
		io.WriteString(w, "vless://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee@one.example:443#One\ntrojan://secret@two.example:443#Two")
	}))
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previous }()
	dir := t.TempDir()
	binary := filepath.Join(dir, "sing-box")
	script := "#!/bin/sh\nif /usr/bin/grep -q trojan \"$3\"; then exit 1; fi\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := FetchSubscription(server.URL+"/private?token=secret", binary)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Labels[result.Nodes[0]["tag"].(string)] != "One" {
		t.Fatalf("snapshot %+v", result)
	}
}
