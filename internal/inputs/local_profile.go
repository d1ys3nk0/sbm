package inputs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const localProfileLimit = 8 << 20

type LocalProfile struct {
	Policy map[string]any   `json:"policy"`
	Nodes  []map[string]any `json:"nodes"`
}

// ConvertLocalProfile accepts only configurations whose proxy group can be
// replaced by SBM's managed selectors without silently discarding policy.
func ConvertLocalProfile(source io.Reader) (LocalProfile, error) {
	var result LocalProfile
	invalid := errors.New("Unsupported or ambiguous local sing-box configuration")
	data, err := io.ReadAll(io.LimitReader(source, localProfileLimit+1))
	if err != nil || len(data) > localProfileLimit || !utf8.Valid(data) {
		return result, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeUnique(decoder, 0)
	if err != nil {
		return result, invalid
	}
	if _, err = decoder.Token(); err != io.EOF {
		return result, invalid
	}
	policy, ok := value.(map[string]any)
	if !ok {
		return result, invalid
	}
	if _, exists := policy["proxy_binding"]; exists {
		return result, invalid
	}
	if exp, exists := policy["experimental"]; exists {
		m, ok := exp.(map[string]any)
		if !ok {
			return result, invalid
		}
		if _, exists := m["clash_api"]; exists {
			return result, invalid
		}
	}
	if route, exists := policy["route"]; exists {
		m, ok := route.(map[string]any)
		if !ok {
			return result, invalid
		}
		if v, exists := m["auto_detect_interface"]; exists {
			if _, ok := v.(bool); !ok {
				return result, invalid
			}
		}
		if sets, exists := m["rule_set"]; exists {
			a, ok := sets.([]any)
			if !ok {
				return result, invalid
			}
			geoipSeen := false
			for _, v := range a {
				m, ok := v.(map[string]any)
				if !ok {
					return result, invalid
				}
				if m["tag"] == "geoip-ru" {
					path, pathOK := m["path"].(string)
					if geoipSeen || len(m) != 4 || m["type"] != "local" || m["format"] != "binary" || !pathOK || !filepath.IsAbs(path) {
						return result, invalid
					}
					geoipSeen = true
				}
			}
		}
	}
	if inbounds, exists := policy["inbounds"]; exists {
		a, ok := inbounds.([]any)
		if !ok {
			return result, invalid
		}
		for _, v := range a {
			m, ok := v.(map[string]any)
			if !ok || m["tag"] == "sbm-probe" || m["tag"] == "sbm-uplink" {
				return result, invalid
			}
		}
	}
	outbounds, ok := policy["outbounds"].([]any)
	if !ok {
		return result, invalid
	}
	var members []any
	var group map[string]any
	kept := make([]any, 0, len(outbounds))
	nodes := make(map[string]map[string]any)
	used := map[string]bool{"sbm-direct": true, "sbm-probe-selector": true, "sbm-unavailable": true}
	for _, value := range outbounds {
		outbound, ok := value.(map[string]any)
		if !ok {
			return result, invalid
		}
		tag, tagOK := outbound["tag"].(string)
		typ, typeOK := outbound["type"].(string)
		if !tagOK || tag == "" || !typeOK || used[tag] {
			return result, invalid
		}
		used[tag] = true
		switch typ {
		case "selector":
			if group != nil || len(outbound) != 3 {
				return result, invalid
			}
			members, ok = outbound["outbounds"].([]any)
			if !ok || len(members) == 0 {
				return result, invalid
			}
			group = map[string]any{"type": typ, "tag": tag}
			kept = append(kept, group)
		case "direct", "block", "dns":
			kept = append(kept, outbound)
		case "urltest":
			return result, invalid
		default:
			server, ok := outbound["server"].(string)
			if !ok || strings.TrimSpace(server) == "" {
				return result, invalid
			}
			nodes[tag] = outbound
		}
	}
	if group == nil || len(nodes) != len(members) {
		return result, invalid
	}
	for _, member := range members {
		tag, ok := member.(string)
		if !ok || nodes[tag] == nil {
			return result, invalid
		}
		result.Nodes = append(result.Nodes, nodes[tag])
		delete(nodes, tag)
	}
	policy["outbounds"] = kept
	result.Policy = policy
	return result, nil
}

func decodeUnique(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate object key")
			}
			value, err := decodeUnique(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid object")
		}
		return object, nil
	case '[':
		var values []any
		for decoder.More() {
			value, err := decodeUnique(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid array")
		}
		return values, nil
	default:
		return nil, errors.New("invalid JSON")
	}
}
