package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type healthState struct {
	Generation  string  `json:"generation"`
	Failures    int     `json:"failures"`
	Healthy     bool    `json:"healthy"`
	LastRestart float64 `json:"last_restart"`
	CheckedAt   float64 `json:"checked_at"`
	NextNode    int     `json:"next_node,omitempty"`
}

func (c *controller) probe(port int, secret, url, expected string) bool {
	input := fmt.Sprintf("proxy-user = \"sbm:%s\"\n", secret)
	result := c.run(15*time.Second, input, "/usr/bin/curl", "--config", "-", "-fsS", "--max-time", "6",
		"--noproxy", "", "--proxy", fmt.Sprintf("http://127.0.0.1:%d", port),
		"-o", "/dev/null", "-w", "%{http_code}", url)
	return result.code == 0 && result.stdout == expected
}

// bodyProbe gives both providers the same deadline and drains both results
// before the caller can change the probe route.
func (c *controller) bodyProbe(secret string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	results := make(chan bool, 2)
	go func() {
		results <- c.downloadProbe(ctx, secret, bodyCheckURL, "200 32768")
	}()
	go func() {
		results <- c.downloadProbe(ctx, secret, bodyFallbackURL, "206 32768", "--range", "0-32767")
	}()

	healthy := false
	for range 2 {
		if <-results {
			healthy = true
			cancel()
		}
	}
	return healthy
}

func (c *controller) downloadProbe(ctx context.Context, secret, url, expected string, options ...string) bool {
	input := fmt.Sprintf("proxy-user = \"sbm:%s\"\n", secret)
	args := []string{"-q", "--config", "-", "-fsS", "--max-time", "3",
		"--noproxy", "", "--proxy", fmt.Sprintf("http://127.0.0.1:%d", probePort),
		"--max-filesize", "32768", "-o", "/dev/null", "-w", "%{http_code} %{size_download}"}
	args = append(args, options...)
	result := c.runCommand(ctx, input, "/usr/bin/curl", append(args, url)...)
	return result.code == 0 && result.stdout == expected && ctx.Err() == nil
}

func (c *controller) uplinkProbe(secret string) bool {
	if c.probe(directPort, secret, uplinkURL, "200") {
		return true
	}
	iface := c.physicalInterface("default")
	if iface == "" {
		return false
	}
	result := c.run(15*time.Second, "", "/usr/bin/curl", "-fsS", "--max-time", "6", "--noproxy", "*",
		"--interface", iface, "-o", "/dev/null", "-w", "%{http_code}", uplinkURL)
	return result.code == 0 && result.stdout == "200"
}

func (c *controller) watchdog() error {
	var generation string
	var proxy proxyConfig
	err := c.locked(func() error {
		var err error
		generation, err = c.active()
		if err != nil {
			return err
		}
		var applied *string
		if err := readJSON(c.path("applied.json"), &applied); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		restart := (applied == nil && generation != "") || (applied != nil && *applied != generation)
		if err := c.reconcile(generation, restart); err != nil {
			return err
		}
		if generation != "" {
			proxy, err = c.proxyConfig(generation)
		}
		return err
	})
	if err != nil || generation == "" {
		return err
	}
	if len(proxy.Nodes) > 0 {
		return c.watchNodes(generation, proxy)
	}
	return c.watchLegacy(generation, proxy.Secret)
}

func (c *controller) watchLegacy(generation, secret string) error {
	healthy := c.probe(probePort, secret, checkURL, "204") && c.bodyProbe(secret)
	uplink := healthy || c.uplinkProbe(secret)
	return c.locked(func() error {
		current, err := c.active()
		if err != nil {
			return err
		}
		if current != generation {
			return nil
		}
		var health healthState
		if err := readJSON(c.path("health.json"), &health); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if health.Generation != generation {
			health.Failures = 0
		}
		if uplink && !healthy {
			health.Failures++
		} else {
			health.Failures = 0
		}
		now := float64(c.now().UnixNano()) / float64(time.Second)
		health.Generation, health.Healthy, health.CheckedAt = generation, healthy, now
		if health.Failures >= 3 && now-health.LastRestart >= 300 {
			health.LastRestart = now
			if err := writeJSON(c.path("health.json"), health); err != nil {
				return err
			}
			if err := c.reconcile(generation, true); err != nil {
				return err
			}
			health.Failures = 0
		}
		return writeJSON(c.path("health.json"), health)
	})
}

func (c *controller) status() error {
	var generation, selectedName string
	var proxy proxyConfig
	var health healthState
	var labels map[string]string
	var running bool
	err := c.locked(func() error {
		var err error
		generation, err = c.active()
		if err != nil {
			return err
		}
		if generation != "" {
			selectedName = strings.SplitN(generation, ".", 2)[0]
			path, err := c.sourcePath(selectedName)
			if err != nil {
				return err
			}
			var snapshot struct {
				Labels map[string]string `json:"labels"`
			}
			if err := readJSON(path, &snapshot); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			labels = snapshot.Labels
			proxy, err = c.proxyConfig(generation)
			if err != nil {
				return err
			}
		}
		status := c.launch("print", job)
		if status.code != 0 && status.code != 113 {
			return errors.New("Could not inspect the SBM launchd job")
		}
		running = status.code == 0 && runningPattern.MatchString(status.stdout)
		if err := readJSON(c.path("health.json"), &health); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if selectedName == "" {
		selectedName = "off"
	}
	process := "stopped"
	if running {
		process = "running"
	}
	lastProbe := "unknown"
	if running && generation != "" && health.Generation == generation {
		lastProbe = "failed"
		if health.Healthy {
			lastProbe = "healthy"
		}
	}
	fmt.Fprintf(c.out, "Desired: %s\nProcess: %s\nLast probe: %s\n", selectedName, process, lastProbe)
	if running && proxy.Secret != "" {
		result := c.proxyAPI(proxy.Secret, "", nil)
		if result.code == 0 {
			var response struct {
				Proxies map[string]struct {
					Type string
					Now  string
				}
			}
			if json.Unmarshal([]byte(result.stdout), &response) != nil {
				return errors.New("Could not read SBM proxy status")
			}
			for tag, group := range response.Proxies {
				if (proxy.Traffic != "" && tag == proxy.Traffic) ||
					(proxy.Traffic == "" && strings.EqualFold(group.Type, "urltest")) {
					selected := group.Now
					switch selected {
					case "":
						selected = "pending"
					case unavailableNode:
						selected = "no healthy node"
					default:
						if label := labels[selected]; label != "" {
							selected = label
						}
					}
					fmt.Fprintf(c.out, "Selected: %s\n", selected)
				}
			}
		}
	}
	return nil
}
