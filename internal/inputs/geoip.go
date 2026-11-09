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
func UpdateGeoIP(root, singBox string, scheduled bool) error {
	var policy struct {
		Route struct {
			RuleSet []struct {
				Tag string `json:"tag"`
			} `json:"rule_set"`
		} `json:"route"`
	}
	if err := readJSON(filepath.Join(root, "policy.json"), &policy); err != nil {
		return err
	}
	needed := false
	for _, r := range policy.Route.RuleSet {
		if r.Tag == "geoip-ru" {
			needed = true
		}
	}
	if !needed {
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(root, "geoip.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	statePath := filepath.Join(root, "geoip.json")
	var state geoState
	if err = readJSON(statePath, &state); err != nil {
		return err
	}
	now := float64(time.Now().Unix())
	dest := filepath.Join(root, "ru.srs")
	previous, readErr := os.ReadFile(dest)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	exists := readErr == nil
	if scheduled && exists && now-state.LastSuccess < 7*86400 {
		return nil
	}
	if scheduled && now-state.LastAttempt < 3600 {
		return nil
	}
	state.LastAttempt = now
	if err = saveState(statePath, state); err != nil {
		return err
	}
	var source string
	if err = readJSON(filepath.Join(root, "geoip-url.json"), &source); err != nil {
		return err
	}
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("GeoIP download or validation failed")
	}
	conditional := exists && state.Source == source && state.Hash == digest(previous)
	client := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || r.URL.User != nil || len(via) >= 10 {
			return errors.New("invalid redirect")
		}
		return nil
	}}
	var data []byte
	var status int
	var etag, modified string
	for attempt := 0; attempt < 3; attempt++ {
		req, e := http.NewRequest(http.MethodGet, source, nil)
		if e != nil {
			break
		}
		if conditional {
			if state.ETag != "" {
				req.Header.Set("If-None-Match", state.ETag)
			}
			if state.Modified != "" {
				req.Header.Set("If-Modified-Since", state.Modified)
			}
		}
		resp, e := client.Do(req)
		if e == nil {
			status = resp.StatusCode
			data, e = io.ReadAll(io.LimitReader(resp.Body, geoIPLimit+1))
			resp.Body.Close()
			etag, modified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
			if e == nil && len(data) <= geoIPLimit && (status == 200 || status == 304 && conditional && (state.ETag != "" || state.Modified != "")) {
				break
			}
		}
		status = 0
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
	if status == 0 {
		return errors.New("GeoIP download or validation failed")
	}
	if status == 200 {
		if len(data) == 0 || validateGeoIP(data, root, singBox) != nil {
			return errors.New("GeoIP download or validation failed")
		}
		if !bytes.Equal(data, previous) {
			if err = writeAtomic(dest, data); err != nil {
				return err
			}
		}
		previous = data
	}
	state = geoState{LastAttempt: now, LastSuccess: now, Source: source, Hash: digest(previous), ETag: etag, Modified: modified}
	return saveState(statePath, state)
}
func validateGeoIP(data []byte, root, binary string) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
