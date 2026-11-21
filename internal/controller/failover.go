package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"time"
)

// Bound the work per watchdog run. NextNode resumes the search on the next tick.
const candidatesPerCheck = 2

func (c *controller) proxyAPI(secret, group string, selection []byte) result {
	input := fmt.Sprintf("header = \"Authorization: Bearer %s\"\n", secret)
	args := []string{"-q", "--config", "-", "-fsS", "--max-time", "1", "--noproxy", "*"}
	if selection != nil {
		args = append(args, "--request", "PUT", "--header", "Content-Type: application/json", "--data", string(selection))
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/proxies", apiPort)
	if group != "" {
		endpoint += "/" + url.PathEscape(group)
	}
	return c.run(time.Second, input, "/usr/bin/curl", append(args, endpoint)...)
}

func (c *controller) selectNode(secret, group, node string) error {
	selection, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: node})
	if err != nil {
		return err
	}
	if c.proxyAPI(secret, group, selection).code != 0 {
		return errors.New("Could not change the SBM proxy selection")
	}
	return nil
}

func (c *controller) watchNodes(generation string, proxy proxyConfig) (string, error) {
	var selected string
	// Keep a stop or connect from replacing the generation between testing and
	// promoting a node. Each invocation probes only a bounded number of nodes.
	err := c.locked(func() error {
		current, err := c.active()
		if err != nil || current != generation {
			return err
		}
		var health healthState
		if err := readJSON(c.path("health.json"), &health); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if health.Generation != generation || health.NextNode < 0 || health.NextNode >= len(proxy.Nodes) {
			health = healthState{Generation: generation}
		}

		selected, err = c.findHealthyNode(proxy, &health)
		health.Healthy = selected != ""
		health.CheckedAt = float64(c.now().UnixNano()) / float64(time.Second)
		return errors.Join(err, writeJSON(c.path("health.json"), health))
	})
	return selected, err
}

func (c *controller) findHealthyNode(proxy proxyConfig, health *healthState) (string, error) {
	response := c.proxyAPI(proxy.Secret, proxy.Traffic, nil)
	var group struct{ Now string }
	if response.code != 0 || json.Unmarshal([]byte(response.stdout), &group) != nil || group.Now == "" {
		return "", errors.New("Could not read the active SBM proxy selection")
	}

	activeIndex := slices.Index(proxy.Nodes, group.Now)
	if activeIndex >= 0 {
		healthy, err := c.checkNode(proxy.Secret, group.Now)
		if err != nil {
			return "", err
		}
		if healthy {
			return group.Now, nil
		}
		if err := c.selectNode(proxy.Secret, proxy.Traffic, unavailableNode); err != nil {
			return "", err
		}
		health.NextNode = (activeIndex + 1) % len(proxy.Nodes)
	}

	for range min(candidatesPerCheck, len(proxy.Nodes)) {
		index := health.NextNode
		if index == activeIndex {
			break
		}
		node := proxy.Nodes[index]
		healthy, err := c.checkNode(proxy.Secret, node)
		if err != nil {
			return "", err
		}
		if healthy {
			if err := c.selectNode(proxy.Secret, proxy.Traffic, node); err != nil {
				return "", err
			}
			return node, nil
		}
		health.NextNode = (index + 1) % len(proxy.Nodes)
	}
	return "", nil
}

func (c *controller) checkNode(secret, node string) (bool, error) {
	if err := c.selectNode(secret, probeSelector, node); err != nil {
		return false, err
	}
	return c.bodyProbe(secret), nil
}
