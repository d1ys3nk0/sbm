package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (c *controller) launch(args ...string) result {
	return c.run(15*time.Second, "", "/bin/launchctl", args...)
}

func (c *controller) reconcile(generation string, restart bool) error {
	status := c.launch("print", job)
	if status.code != 0 && status.code != 113 {
		return errors.New("Could not inspect the SBM launchd job")
	}
	if generation == "" {
		if status.code == 0 && c.launch("bootout", job).code != 0 {
			return errors.New("Could not unload the SBM launchd job")
		}
		if err := c.tailscale(false); err != nil {
			return err
		}
	} else if status.code != 0 {
		processes := c.run(15*time.Second, "", "/usr/bin/pgrep", "-x", "sing-box")
		if processes.code == 0 {
			return errors.New("Stop the other sing-box service before connecting SBM")
		}
		if processes.code != 1 {
			return errors.New("Could not inspect running sing-box processes")
		}
		if err := c.tailscale(true); err != nil {
			return err
		}
		if c.launch("bootstrap", "system", c.path("sbm.plist")).code != 0 {
			return errors.New("Could not register the SBM launchd job")
		}
	} else if restart && c.launch("kickstart", "-k", job).code != 0 {
		return errors.New("Could not restart the SBM launchd job")
	}
	var applied *string
	err := readJSON(c.path("applied.json"), &applied)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if (generation == "" && applied == nil) || (applied != nil && *applied == generation) {
		return nil
	}
	var desired any
	if generation != "" {
		desired = generation
	}
	return writeJSON(c.path("applied.json"), desired)
}

type tailState struct {
	Exit string `json:"exit"`
	LAN  bool   `json:"lan"`
}

type tailPrefs struct {
	ExitNodeIP             string
	ExitNodeID             string
	ExitNodeAllowLANAccess bool
}

func (c *controller) tailscale(enabled bool) error {
	var executable string
	if err := readJSON(c.path("tailscale-cli.json"), &executable); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("Invalid Tailscale CLI configuration")
	}
	if executable == "" {
		return nil
	}
	if !filepath.IsAbs(executable) {
		return errors.New("Tailscale CLI path must be absolute")
	}
	statePath := c.path("tailscale.json")
	var state tailState
	stateErr := readJSON(statePath, &state)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return errors.New("Invalid Tailscale exit-node state")
	}
	tail := func(args ...string) (string, error) {
		all := append([]string{"TAILSCALE_BE_CLI=1", executable}, args...)
		result := c.run(4*time.Second, "", "/usr/bin/env", all...)
		if result.code != 0 {
			return "", errors.New("Could not reconcile the Tailscale exit node")
		}
		return result.stdout, nil
	}
	if !enabled {
		if stateErr == nil {
			if _, err := tail("set", "--exit-node="+state.Exit, fmt.Sprintf("--exit-node-allow-lan-access=%t", state.LAN)); err != nil {
				return err
			}
			if err := os.Remove(statePath); err != nil {
				return err
			}
			return syncDir(c.root)
		}
		return nil
	}
	prefsData, err := tail("debug", "prefs")
	var prefs *tailPrefs
	if err != nil || json.Unmarshal([]byte(prefsData), &prefs) != nil || prefs == nil {
		if c.physicalInterface("1.1.1.1") != "" && c.physicalInterface("8.8.8.8") != "" {
			return nil
		}
		return errors.New("Tailscale preferences unavailable and the public route is not physical")
	}
	if prefs.ExitNodeIP == "" && prefs.ExitNodeID == "" {
		return nil
	}
	if errors.Is(stateErr, os.ErrNotExist) {
		selected := prefs.ExitNodeIP
		if selected == "" {
			statusData, err := tail("status", "--json")
			if err != nil {
				return err
			}
			var status struct {
				ExitNodeStatus struct{ TailscaleIPs []string }
			}
			if json.Unmarshal([]byte(statusData), &status) != nil || len(status.ExitNodeStatus.TailscaleIPs) == 0 {
				return errors.New("Could not resolve the current Tailscale exit node")
			}
			selected = strings.Split(status.ExitNodeStatus.TailscaleIPs[0], "/")[0]
		}
		if err := writeJSON(statePath, tailState{Exit: selected, LAN: prefs.ExitNodeAllowLANAccess}); err != nil {
			return err
		}
	}
	if _, err := tail("set", "--exit-node="); err != nil {
		return err
	}
	prefsData, err = tail("debug", "prefs")
	prefs = nil
	if err != nil || json.Unmarshal([]byte(prefsData), &prefs) != nil || prefs == nil || prefs.ExitNodeIP != "" || prefs.ExitNodeID != "" {
		return errors.New("Tailscale exit node did not clear")
	}
	return nil
}

func (c *controller) physicalInterface(destination string) string {
	result := c.run(15*time.Second, "", "/sbin/route", "-n", "get", "-inet", destination)
	match := interfacePattern.FindStringSubmatch(result.stdout)
	if result.code != 0 || len(match) != 2 {
		return ""
	}
	return match[1]
}
