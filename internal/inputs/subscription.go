package inputs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const subscriptionLimit = 8 << 20

type Snapshot struct {
	URL    string            `json:"url"`
	Nodes  []map[string]any  `json:"nodes"`
	Labels map[string]string `json:"labels"`
}
type entry struct {
	outbound map[string]any
	label    string
}

func FetchSubscription(source, singBox string) (Snapshot, error) {
	snapshot := Snapshot{URL: source, Labels: map[string]string{}}
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return snapshot, errors.New("Subscription URL must use HTTPS without user credentials")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || r.URL.User != nil || len(via) >= 10 {
			return errors.New("invalid redirect")
		}
		return nil
	}}
	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return snapshot, errors.New("Subscription download or validation failed")
	}
	req.Header.Set("User-Agent", "sing-box")
	resp, err := client.Do(req)
	if err != nil {
		return snapshot, errors.New("Subscription download or validation failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return snapshot, fmt.Errorf("Subscription download or validation failed (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, subscriptionLimit+1))
	if err != nil || len(data) > subscriptionLimit || !utf8.Valid(data) {
		return snapshot, errors.New("Subscription download or validation failed")
	}
	entries, err := parseSubscription(string(data))
	if err != nil {
		return snapshot, errors.New("Subscription download or validation failed")
	}
	for _, item := range entries {
		valid, err := validateNode(item.outbound, singBox)
		if err != nil {
			return snapshot, errors.New("Subscription download or validation failed")
		}
		if valid {
			snapshot.Nodes = append(snapshot.Nodes, item.outbound)
			snapshot.Labels[item.outbound["tag"].(string)] = item.label
		}
	}
	if len(snapshot.Nodes) == 0 {
		return snapshot, errors.New("Subscription contains no usable nodes")
	}
	return snapshot, nil
}

func validateNode(node map[string]any, binary string) (bool, error) {
	dir, err := os.MkdirTemp("", "sbm-subscribe-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	data, err := json.Marshal(map[string]any{"outbounds": []any{node}})
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, "check.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = exec.CommandContext(ctx, binary, "check", "-c", path).Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, err
}

func decode64(s string) (string, error) {
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		data, err := enc.DecodeString(s)
		if err == nil && utf8.Valid(data) {
			return string(data), nil
		}
	}
	return "", errors.New("invalid base64")
}
func readable(s string) string {
	out := make([]rune, 0, 200)
	for _, r := range s {
		if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Co, r) && !unicode.Is(unicode.Cs, r) {
			out = append(out, r)
			if len(out) == 200 {
				break
			}
		}
	}
	return string(out)
}
func pythonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 32 || r >= 127 {
				if r <= 0xffff {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					a, c := utf16.EncodeRune(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, a, c)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
func canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return pythonString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return string(x)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = canonical(e)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, pythonString(k)+":"+canonical(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
func makeEntry(node map[string]any, label string) entry {
	sum := sha256.Sum256([]byte(canonical(node)))
	node["tag"] = "sub-" + hex.EncodeToString(sum[:])
	return entry{node, readable(label)}
}
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		return v != "0" && v != "0.0"
	case []any:
		return len(v) != 0
	case map[string]any:
		return len(v) != 0
	default:
		return true
	}
}
func hasLocalFile(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if strings.HasSuffix(k, "_path") && truthy(item) {
				return true
			}
			if hasLocalFile(item) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if hasLocalFile(item) {
				return true
			}
		}
	}
	return false
}
func parseSubscription(raw string) ([]entry, error) {
	raw = strings.TrimLeft(strings.TrimSpace(raw), "\ufeff")
	var entries []entry
	seen := map[string]bool{}
	appendEntry := func(e entry) {
		tag := e.outbound["tag"].(string)
		if !seen[tag] {
			seen[tag] = true
			entries = append(entries, e)
		}
	}
	if strings.HasPrefix(raw, "{") {
		var doc map[string]any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&doc) != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, errors.New("invalid json")
		}
		nodes, ok := doc["outbounds"].([]any)
		if !ok {
			return nil, errors.New("missing outbounds")
		}
		allowed := map[string]bool{"shadowsocks": true, "vless": true, "vmess": true, "trojan": true, "hysteria": true, "hysteria2": true, "tuic": true, "socks": true, "http": true, "anytls": true, "naive": true, "ssh": true}
		for _, item := range nodes {
			node, ok := item.(map[string]any)
			if !ok {
				continue
			}
			server, ok := node["server"].(string)
			if !ok {
				continue
			}
			typ, _ := node["type"].(string)
			if !allowed[typ] || truthy(node["detour"]) || hasLocalFile(node) {
				continue
			}
			label, ok := node["tag"].(string)
			if !ok {
				label = server
			}
			delete(node, "tag")
			appendEntry(makeEntry(node, label))
		}
		return entries, nil
	}
	if !strings.Contains(raw, "://") {
		var err error
		raw, err = decode64(strings.Join(strings.Fields(raw), ""))
		if err != nil {
			return nil, err
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		item, err := parseURI(strings.TrimSpace(line))
		if err == nil {
			appendEntry(item)
		}
	}
	return entries, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

func normalizeUUID(value string) (string, bool) {
	value = strings.TrimPrefix(value, "urn:")
	value = strings.TrimPrefix(value, "uuid:")
	value = strings.Trim(value, "{}")
	value = strings.ReplaceAll(value, "-", "")
	if !uuidPattern.MatchString(value) {
		return "", false
	}
	value = strings.ToLower(value)
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:], true
}

func parseURI(raw string) (entry, error) {
	bad := func() (entry, error) { return entry{}, errors.New("invalid connection") }
	u, err := url.Parse(raw)
	if err != nil {
		return bad()
	}
	proto := strings.ToLower(u.Scheme)
	if proto != "vless" && proto != "trojan" && proto != "ss" && proto != "hysteria2" && proto != "hy2" {
		return bad()
	}
	options, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return bad()
	}
	known := strings.Fields("type security sni peer allowInsecure insecure fp alpn pbk sid flow encryption serviceName mode host path headerType packetEncoding plugin spx cs")
	if proto == "hy2" || proto == "hysteria2" {
		known = strings.Fields("sni insecure obfs obfs-password")
	}
	valid := map[string]bool{}
	for _, k := range known {
		valid[k] = true
	}
	for k, v := range options {
		if !valid[k] || len(v) != 1 {
			return bad()
		}
	}
	get := func(k string) string { return options.Get(k) }
	def := func(k, v string) string {
		if _, ok := options[k]; ok {
			return get(k)
		}
		return v
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if get(k) != "" {
				return true
			}
		}
		return false
	}
	var node map[string]any
	endpoint := u
	switch proto {
	case "ss":
		authority := strings.TrimRight(strings.SplitN(strings.SplitN(raw[5:], "#", 2)[0], "?", 2)[0], "/")
		if !strings.Contains(authority, "@") {
			authority, err = decode64(authority)
			if err != nil {
				return bad()
			}
		}
		pos := strings.LastIndex(authority, "@")
		if pos < 0 {
			return bad()
		}
		cred, e := url.PathUnescape(authority[:pos])
		if e != nil {
			return bad()
		}
		if !strings.Contains(cred, ":") {
			cred, err = decode64(cred)
			if err != nil {
				return bad()
			}
		}
		method, password, ok := strings.Cut(cred, ":")
		if !ok || method == "" || password == "" {
			return bad()
		}
		endpoint, err = url.Parse("ss://" + authority[pos+1:])
		if err != nil {
			return bad()
		}
		node = map[string]any{"type": "shadowsocks", "method": method, "password": password}
		if get("plugin") != "" {
			p, opts, _ := strings.Cut(get("plugin"), ";")
			node["plugin"], node["plugin_opts"] = p, opts
		}
		for k := range options {
			if k != "plugin" {
				return bad()
			}
		}
	case "hy2", "hysteria2":
		if u.Path != "" && u.Path != "/" {
			return bad()
		}
		cred := ""
		if u.User != nil {
			cred = u.User.Username()
			if p, ok := u.User.Password(); ok {
				cred += ":" + p
			}
		}
		tls := map[string]any{"enabled": true}
		if get("sni") != "" {
			tls["server_name"] = get("sni")
		}
		if insecure := def("insecure", "0"); insecure != "0" && insecure != "1" {
			return bad()
		}
		if get("insecure") == "1" {
			tls["insecure"] = true
		}
		node = map[string]any{"type": "hysteria2", "password": cred, "tls": tls}
		if _, ok := options["obfs"]; ok {
			if get("obfs") != "salamander" || get("obfs-password") == "" {
				return bad()
			}
			node["obfs"] = map[string]any{"type": "salamander", "password": get("obfs-password")}
		} else if _, ok := options["obfs-password"]; ok {
			return bad()
		}
	default:
		if u.User == nil {
			return bad()
		}
		cred := u.User.Username()
		if _, ok := u.User.Password(); ok || cred == "" {
			return bad()
		}
		if proto == "vless" {
			uuid, valid := normalizeUUID(cred)
			if !valid || def("encryption", "none") != "none" {
				return bad()
			}
			node = map[string]any{"type": "vless", "uuid": uuid}
			if get("flow") != "" {
				node["flow"] = get("flow")
			}
			if get("packetEncoding") != "" {
				node["packet_encoding"] = get("packetEncoding")
			}
		} else {
			node = map[string]any{"type": "trojan", "password": cred}
		}
	}
	host := strings.ToLower(endpoint.Hostname())
	port := endpoint.Port()
	if port == "" && (proto == "hy2" || proto == "hysteria2") {
		port = "443"
	}
	pn, err := strconv.Atoi(port)
	if host == "" || err != nil || pn < 1 || pn > 65535 {
		return bad()
	}
	node["server"], node["server_port"] = host, pn
	if proto == "vless" || proto == "trojan" {
		security := def("security", "none")
		if proto == "trojan" {
			security = def("security", "tls")
		}
		if security != "none" && security != "tls" && security != "reality" {
			return bad()
		}
		if security == "none" && (proto == "trojan" || has("sni", "peer", "allowInsecure", "insecure", "fp", "alpn", "pbk", "sid", "spx", "cs")) {
			return bad()
		}
		if security == "tls" && has("pbk", "sid", "spx") {
			return bad()
		}
		if security != "none" {
			tls := map[string]any{"enabled": true}
			server := get("sni")
			if server == "" {
				server = get("peer")
			}
			if server != "" {
				tls["server_name"] = server
			}
			ins := def("allowInsecure", def("insecure", "0"))
			if ins != "0" && ins != "1" && ins != "true" && ins != "false" {
				return bad()
			}
			if ins == "1" || ins == "true" {
				tls["insecure"] = true
			}
			if get("fp") != "" {
				tls["utls"] = map[string]any{"enabled": true, "fingerprint": get("fp")}
			}
			if get("alpn") != "" {
				values := strings.Split(get("alpn"), ",")
				arr := make([]any, len(values))
				for i, s := range values {
					arr[i] = s
				}
				tls["alpn"] = arr
			}
			if get("cs") != "" {
				values := strings.Split(get("cs"), ":")
				arr := make([]any, len(values))
				for i, s := range values {
					if s == "" {
						return bad()
					}
					arr[i] = s
				}
				tls["cipher_suites"] = arr
			}
			if security == "reality" {
				if get("pbk") == "" || (get("spx") != "" && get("spx") != "/") {
					return bad()
				}
				tls["reality"] = map[string]any{"enabled": true, "public_key": get("pbk"), "short_id": get("sid")}
			}
			node["tls"] = tls
		}
		if def("headerType", "none") != "none" {
			return bad()
		}
		switch def("type", "tcp") {
		case "ws":
			transport := map[string]any{"type": "ws", "path": def("path", "/")}
			if get("host") != "" {
				transport["headers"] = map[string]any{"Host": get("host")}
			}
			node["transport"] = transport
		case "grpc":
			if def("mode", "gun") != "gun" && get("mode") != "" {
				return bad()
			}
			node["transport"] = map[string]any{"type": "grpc", "service_name": get("serviceName")}
		case "tcp":
			if has("host", "path", "serviceName", "mode") {
				return bad()
			}
		default:
			return bad()
		}
	}
	label := u.Fragment
	if label == "" {
		label = host
	}
	return makeEntry(node, label), nil
}
