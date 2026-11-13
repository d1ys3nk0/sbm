package inputs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const geoIPLimit = 10 << 20

type geoState struct {
	LastAttempt float64 `json:"last_attempt"`
	Failed      bool    `json:"failed,omitempty"`
	LastSuccess float64 `json:"last_success,omitempty"`
	Source      string  `json:"source,omitempty"`
	Hash        string  `json:"hash,omitempty"`
	ETag        string  `json:"etag,omitempty"`
	Modified    string  `json:"modified,omitempty"`
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
func writeAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".sbm-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func saveState(path string, state geoState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type geoActivation struct {
	PreviousExists bool   `json:"previous_exists"`
	PreviousHash   string `json:"previous_hash"`
	CandidateHash  string `json:"candidate_hash"`
}

type geoOptions struct {
	ctx      context.Context
	policy   map[string]any
	now      func() time.Time
	client   *http.Client
	validate func([]byte) error
	activate func() error
}

func UpdateGeoIP(root, singBox string, scheduled bool) error {
	return UpdateGeoIPWithActivation(root, singBox, scheduled, nil)
}

// UpdateGeoIPWithActivation commits success only after the session owner confirms
// activation and its configured connectivity checks. The same callback reapplies
// the old file on rollback; nil is reserved for an inactive session.
func UpdateGeoIPWithActivation(root, singBox string, scheduled bool, activate func() error) error {
	return UpdateGeoIPForPolicy(root, singBox, scheduled, nil, activate)
}

func UpdateGeoIPForPolicy(root, singBox string, scheduled bool, policy map[string]any, activate func() error) error {
	return UpdateGeoIPForPolicyContext(context.Background(), root, singBox, scheduled, policy, activate)
}

func UpdateGeoIPForPolicyContext(ctx context.Context, root, singBox string, scheduled bool, policy map[string]any, activate func() error) error {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	return updateGeoIP(root, scheduled, geoOptions{
		now:    time.Now,
		ctx:    ctx,
		policy: policy,
		client: &http.Client{Timeout: 40 * time.Second, Transport: transport, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != "https" || r.URL.User != nil || len(via) >= 10 {
				return errors.New("invalid redirect")
			}
			return nil
		}},
		validate: func(data []byte) error { return validateGeoIPContext(ctx, data, root, singBox) },
		activate: activate,
	})
}

func durableRemove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func clearGeoActivation(root string) error {
	if err := durableRemove(filepath.Join(root, "geoip-activation.json")); err != nil {
		return err
	}
	return durableRemove(filepath.Join(root, "geoip-previous.srs"))
}

func restoreGeoActivation(root string, activation geoActivation, activate func() error) error {
	destination := filepath.Join(root, "ru.srs")
	current, err := os.ReadFile(destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && digest(current) != activation.CandidateHash && digest(current) != activation.PreviousHash {
		return errors.New("GeoIP recovery found an unexpected destination; recovery journal retained")
	}
	if activation.PreviousExists {
		data, err := os.ReadFile(filepath.Join(root, "geoip-previous.srs"))
		if err != nil || digest(data) != activation.PreviousHash {
			return errors.New("GeoIP rollback backup is unavailable or invalid; recovery journal retained")
		}
		if err = writeAtomic(destination, data); err != nil {
			return err
		}
	} else if err = durableRemove(destination); err != nil {
		return err
	}
	if activate != nil {
		if err = activate(); err != nil {
			return errors.New("GeoIP rollback activation failed; recovery journal retained")
		}
	}
	return clearGeoActivation(root)
}

func recoverGeoActivation(root string, activate func() error) error {
	var activation geoActivation
	data, err := os.ReadFile(filepath.Join(root, "geoip-activation.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &activation) != nil || len(activation.CandidateHash) != 64 || activation.PreviousExists && len(activation.PreviousHash) != 64 {
		return errors.New("invalid GeoIP recovery journal")
	}
	return restoreGeoActivation(root, activation, activate)
}

func updateGeoIP(root string, scheduled bool, options geoOptions) error {
	var err error
	policy := options.policy
	if policy == nil {
		if err = readJSON(filepath.Join(root, "policy.json"), &policy); err != nil {
			return err
		}
	}
	needed := false
	route, _ := policy["route"].(map[string]any)
	ruleSets, _ := route["rule_set"].([]any)
	for _, value := range ruleSets {
		rule, _ := value.(map[string]any)
		if rule["tag"] == "geoip-ru" {
			needed = true
		}
	}
	if !needed {
		if _, err := os.Lstat(filepath.Join(root, "geoip-activation.json")); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
	}
	parent := options.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	lock, err := os.OpenFile(filepath.Join(root, "geoip.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err = recoverGeoActivation(root, options.activate); err != nil {
		return err
	}
	if !needed {
		return nil
	}
	statePath := filepath.Join(root, "geoip.json")
	var state geoState
	if err = readJSON(statePath, &state); err != nil {
		return err
	}
	now := float64(options.now().Unix())
	destination := filepath.Join(root, "ru.srs")
	previous, err := os.ReadFile(destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exists := err == nil
	failed := state.Failed || state.LastAttempt > state.LastSuccess
	if scheduled {
		if failed && now-state.LastAttempt < 3600 {
			return nil
		}
		if !failed && exists && now-state.LastSuccess < 7*86400 {
			return nil
		}
	}
	state.LastAttempt = now
	state.Failed = true
	if err = saveState(statePath, state); err != nil {
		return err
	}
	var source string
	if err = readJSON(filepath.Join(root, "geoip-url.json"), &source); err != nil {
		return err
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("GeoIP download or validation failed")
	}
	conditional := exists && state.Source == source && state.Hash == digest(previous)
	var data []byte
	var status int
	var etag, modified string
	for attempt := 0; attempt < 3; attempt++ {
		request, e := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if e != nil {
			break
		}
		if conditional {
			if state.ETag != "" {
				request.Header.Set("If-None-Match", state.ETag)
			}
			if state.Modified != "" {
				request.Header.Set("If-Modified-Since", state.Modified)
			}
		}
		response, e := options.client.Do(request)
		if e == nil {
			status = response.StatusCode
			data, e = io.ReadAll(io.LimitReader(response.Body, geoIPLimit+1))
			response.Body.Close()
			etag, modified = response.Header.Get("ETag"), response.Header.Get("Last-Modified")
			if e == nil && len(data) <= geoIPLimit && (status == 200 || status == 304 && conditional && (state.ETag != "" || state.Modified != "")) {
				break
			}
		}
		status = 0
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return errors.New("GeoIP download or validation failed")
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
	}
	if status == 0 {
		return errors.New("GeoIP download or validation failed")
	}
	if status == 304 {
		data = previous
		if etag == "" {
			etag = state.ETag
		}
		if modified == "" {
			modified = state.Modified
		}
	}
	if len(data) == 0 || options.validate(data) != nil {
		return errors.New("GeoIP download or validation failed")
	}
	if !bytes.Equal(data, previous) {
		activation := geoActivation{PreviousExists: exists, CandidateHash: digest(data)}
		if exists {
			activation.PreviousHash = digest(previous)
			if err = writeAtomic(filepath.Join(root, "geoip-previous.srs"), previous); err != nil {
				return err
			}
		}
		encoded, e := json.Marshal(activation)
		if e != nil {
			return e
		}
		if err = writeAtomic(filepath.Join(root, "geoip-activation.json"), encoded); err != nil {
			return err
		}
		if err = writeAtomic(destination, data); err != nil {
			return err
		}
		if options.activate != nil {
			if err = options.activate(); err != nil {
				if rollback := restoreGeoActivation(root, activation, options.activate); rollback != nil {
					return rollback
				}
				return errors.New("GeoIP activation failed; previous rules restored")
			}
		}
	}
	state = geoState{LastAttempt: now, LastSuccess: now, Source: source, Hash: digest(data), ETag: etag, Modified: modified}
	if err = saveState(statePath, state); err != nil {
		return err
	}
	return clearGeoActivation(root)
}
func validateGeoIP(data []byte, root, binary string) error {
	return validateGeoIPContext(context.Background(), data, root, binary)
}

func validateGeoIPContext(parent context.Context, data []byte, root, binary string) error {
	dir, err := os.MkdirTemp(root, "geoip-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "candidate.srs")
	output := filepath.Join(dir, "rules.json")
	if err = os.WriteFile(input, data, 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if err = exec.CommandContext(ctx, binary, "rule-set", "decompile", input, "--output", output).Run(); err != nil {
		return err
	}
	var rules struct {
		Version int `json:"version"`
		Rules   []struct {
			CIDRs []string `json:"ip_cidr"`
		} `json:"rules"`
	}
	if err = readJSON(output, &rules); err != nil {
		return err
	}
	ipv4, ipv6 := false, false
	for _, r := range rules.Rules {
		for _, cidr := range r.CIDRs {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				return err
			}
			if ip.To4() != nil {
				ipv4 = true
			} else {
				ipv6 = true
			}
		}
	}
	if rules.Version < 1 || !ipv4 || !ipv6 {
		return errors.New("invalid geoip rules")
	}
	return nil
}
