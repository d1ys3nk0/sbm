package controller

import (
	"errors"
	"os"
	"syscall"
)

type sessionRecovery struct {
	PID int `json:"pid"`
}

func (c *controller) pendingSession() error {
	for _, name := range []string{"session-recovery.json", "active.json"} {
		if _, err := os.Lstat(c.path(name)); err == nil {
			return errors.New("A previous session requires recovery; run stop")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("Could not inspect session recovery")
		}
	}
	return nil
}

// A persisted PID is used only to refuse recovery while a process may exist.
func (c *controller) recoverStopped() error {
	path := c.path("session-recovery.json")
	var record sessionRecovery
	if err := readJSON(path, &record); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(c.path("active.json")); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("Session recovery record is missing; inspect private state")
	} else if err != nil || record.PID <= 1 {
		return errors.New("Invalid session recovery record; inspect private state")
	}
	if err := syscall.Kill(record.PID, 0); !errors.Is(err, syscall.ESRCH) {
		return errors.New("The previous session process may still exist; inspect it before retrying stop")
	}
	if _, err := os.Lstat(c.path("tailscale.json")); err == nil {
		var executable string
		if err := readJSON(c.path("tailscale-cli.json"), &executable); err != nil || executable == "" {
			return errors.New("Tailscale recovery configuration is unavailable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("Could not inspect Tailscale recovery state")
	}
	if err := c.tailscale(false); err != nil {
		return err
	}
	if err := os.Remove(c.path("active.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(c.root); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(c.root)
}
