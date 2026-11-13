package inputs

import (
	"reflect"
	"strings"
	"testing"
)

func TestConvertLocalProfilePreservesPolicyAndNodes(t *testing.T) {
	source := `{"log":{"level":"warn"},"route":{"final":"proxy"},"outbounds":[{"type":"selector","tag":"proxy","outbounds":["node"]},{"type":"direct","tag":"direct"},{"type":"vless","tag":"node","server":"example.org","server_port":443,"uuid":"secret"}]}`
	got, err := ConvertLocalProfile(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy["log"].(map[string]any)["level"] != "warn" || got.Policy["route"].(map[string]any)["final"] != "proxy" {
		t.Fatal("policy lost")
	}
	if len(got.Nodes) != 1 || got.Nodes[0]["uuid"] != "secret" || got.Nodes[0]["server"] != "example.org" {
		t.Fatal("node lost")
	}
	outbounds := got.Policy["outbounds"].([]any)
	if len(outbounds) != 2 || !reflect.DeepEqual(outbounds[0], map[string]any{"type": "selector", "tag": "proxy"}) {
		t.Fatalf("unexpected policy group: %#v", outbounds)
	}
}

func TestConvertLocalProfileRejectsAmbiguousOrOverwrittenSettings(t *testing.T) {
	cases := []string{
		`{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"],"default":"n"},{"type":"vless","tag":"n","server":"x"}]}`,
		`{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"},{"type":"vless","tag":"other","server":"y"}]}`,
		`{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"},{"type":"selector","tag":"other","outbounds":["n"]}]}`,
		`{"experimental":{"clash_api":{"secret":"x"}},"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"}]}`,
		`{"route":{"auto_detect_interface":"invalid"},"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"}]}`,
		`{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","tag":"other","server":"x"}]}`,
		`{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"}]} {}`,
		`{"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"direct","tag":"n"}]}`,
	}
	for _, source := range cases {
		if _, err := ConvertLocalProfile(strings.NewReader(source)); err == nil {
			t.Errorf("accepted unsupported input: %s", source)
		}
	}
}

func TestConvertLocalProfileRejectsTooDeepAndOversizedInput(t *testing.T) {
	for _, source := range []string{strings.Repeat("[", 130) + strings.Repeat("]", 130), strings.Repeat(" ", localProfileLimit+1)} {
		if _, err := ConvertLocalProfile(strings.NewReader(source)); err == nil {
			t.Fatal("accepted input exceeding a parser limit")
		}
	}
}

func TestConvertLocalProfilePreservesManagedGeoIPSource(t *testing.T) {
	source := `{"route":{"rule_set":[{"tag":"geoip-ru","type":"local","format":"binary","path":"/private/ru.srs"}]},"outbounds":[{"type":"selector","tag":"p","outbounds":["n"]},{"type":"vless","tag":"n","server":"x"}]}`
	profile, err := ConvertLocalProfile(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	route := profile.Policy["route"].(map[string]any)
	item := route["rule_set"].([]any)[0].(map[string]any)
	if item["path"] != "/private/ru.srs" {
		t.Fatal("source GeoIP path changed")
	}
	bad := strings.Replace(source, `"path":"/private/ru.srs"`, `"path":"/private/ru.srs","download_detour":"p"`, 1)
	if _, err := ConvertLocalProfile(strings.NewReader(bad)); err == nil {
		t.Fatal("accepted ambiguous GeoIP settings")
	}
}
