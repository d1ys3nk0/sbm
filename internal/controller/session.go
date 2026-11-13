package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var errSessionCleanup = errors.New("session cleanup failed")
var errUplinkChanged = errors.New("physical uplink changed")

// runSession owns its child directly; no persisted PID is used to signal it.
func (c *controller) runSession(ctx context.Context, name string, state func(string)) (retErr error) {
	state("connecting")
	if err := c.pendingSession(); err != nil {
		return errors.Join(errSessionCleanup, err)
	}
	processes := c.run(15*time.Second, "", "/usr/bin/pgrep", "-x", "sing-box")
	if processes.code != 1 {
		return errors.New("Another sing-box process is running or cannot be inspected")
	}
	path, err := c.sourcePath(name)
	if err != nil {
		return err
	}
	var profile savedProfile
	if err := readJSON(path, &profile); err != nil {
		return errors.New("Unknown or invalid profile")
	}
	if err := c.updateGeoIP(ctx, true, profile.Policy, nil); err != nil {
		if _, journalErr := os.Lstat(c.path("geoip-activation.json")); journalErr == nil {
			return errors.Join(errSessionCleanup, err)
		}
		fmt.Fprintln(c.errOut, "SBM GeoIP startup check failed")
	}
	if err := c.prepareContext(ctx, name, false); err != nil {
		return err
	}
	var cmd *exec.Cmd
	defer func() {
		if cmd != nil {
			err := writeJSON(c.path("session-recovery.json"), sessionRecovery{PID: cmd.Process.Pid})
			retErr = errors.Join(retErr, errSessionCleanup, errors.New("sing-box termination not confirmed"), err)
			return
		}
		if err := c.tailscale(false); err != nil {
			retErr = errors.Join(retErr, errSessionCleanup, err)
		}
		if retErr != nil && errors.Is(retErr, errSessionCleanup) {
			return
		}
		if err := c.locked(func() error {
			if err := os.Remove(c.path("active.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Remove(c.path("session-recovery.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return syncDir(c.root)
		}); err != nil {
			retErr = errors.Join(retErr, errSessionCleanup, err)
		}
	}()
	if err := c.tailscale(true); err != nil {
		return err
	}
	generation, err := c.active()
	if err != nil {
		return err
	}
	proxy, err := c.proxyConfig(generation)
	if err != nil {
		return err
	}
	uplink, err := c.configuredUplink()
	if err != nil {
		return err
	}
	if uplink.Interface != "" {
		var generated struct {
			Outbounds []struct {
				Type   string `json:"type"`
				Server string `json:"server"`
				Bind   string `json:"bind_interface"`
			} `json:"outbounds"`
		}
		if err := readJSON(c.path(generation), &generated); err != nil {
			return err
		}
		for _, outbound := range generated.Outbounds {
			if (outbound.Type == "direct" || outbound.Server != "") && outbound.Bind != uplink.Interface {
				return errUplinkChanged
			}
		}
	}
	logPath := c.path("sing-box.log")
	if info, err := os.Lstat(logPath); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return errors.New("Unsafe private sing-box log")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("Could not inspect private sing-box log")
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("Could not open private sing-box log")
	}
	defer log.Close()
	var exited chan error
	start := func() error {
		if err := os.Remove(c.path("session-recovery.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncDir(c.root); err != nil {
			return err
		}
		cmd = exec.Command(c.bin, "run", "-c", c.path(generation))
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			cmd = nil
			return errors.New("Could not start sing-box")
		}
		exited = make(chan error, 1)
		process, result := cmd, exited
		go func() { result <- process.Wait() }()
		if err := writeJSON(c.path("session-recovery.json"), sessionRecovery{PID: process.Process.Pid}); err != nil {
			return errors.New("Could not persist session recovery record")
		}
		return nil
	}
	stop := func() error {
		if cmd == nil {
			return nil
		}
		if err := stopOwnedChild(cmd.Process, exited, 5*time.Second, 5*time.Second); err != nil {
			return err
		}
		cmd = nil
		return nil
	}
	defer func() {
		if err := stop(); err != nil {
			retErr = errors.Join(retErr, errSessionCleanup, err)
		}
	}()
	if err := start(); err != nil {
		return err
	}
	activate := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		state("connecting")
		if err := stop(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := start(); err != nil {
			return err
		}
		state("running")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if err := c.watchNodes(generation, proxy); err == nil {
				var health healthState
				if readJSON(c.path("health.json"), &health) == nil && health.Generation == generation && health.Healthy && c.checkRoutes(ctx) == nil {
					state("healthy")
					return nil
				}
			}
			if !pause(ctx, time.Second) {
				break
			}
		}
		state("unhealthy")
		return errors.New("Could not verify GeoIP activation")
	}
	update := func(scheduled bool) error {
		return c.updateGeoIP(ctx, scheduled, profile.Policy, activate)
	}

	state("running")
	ticker := time.NewTimer(0)
	defer ticker.Stop()
	geoipTicker := time.NewTicker(time.Hour)
	defer geoipTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-exited:
			cmd = nil
			return fmt.Errorf("sing-box stopped unexpectedly: %v", err)
		case reply := <-c.geoipUpdates:
			err := update(false)
			reply <- err
			if err != nil {
				if _, journalErr := os.Lstat(c.path("geoip-activation.json")); journalErr == nil {
					return errors.Join(errSessionCleanup, err)
				}
			}
		case <-geoipTicker.C:
			if err := update(true); err != nil {
				fmt.Fprintln(c.errOut, "SBM GeoIP scheduled update failed")
				if _, journalErr := os.Lstat(c.path("geoip-activation.json")); journalErr == nil {
					return errors.Join(errSessionCleanup, err)
				}
			}
		case <-ticker.C:
			current, err := c.configuredUplink()
			if err != nil || current != uplink {
				return errUplinkChanged
			}
			if err := c.watchNodes(generation, proxy); err != nil {
				state("unhealthy")
				fmt.Fprintln(c.errOut, "SBM health check failed")
			} else {
				var health healthState
				if readJSON(c.path("health.json"), &health) == nil && health.Generation == generation && health.Healthy && c.checkRoutes(ctx) == nil {
					state("healthy")
				} else {
					state("unhealthy")
				}
			}
			ticker.Reset(30 * time.Second)
		}
	}
}
