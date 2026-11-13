package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLiveCustomNamedChildRefusesRecoveryAcrossRestart(t *testing.T) {
	c := testController(t)
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if err := writeJSON(c.path("session-recovery.json"), sessionRecovery{PID: child.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("office.1.json", c.path("active.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("tailscale.json"), tailState{Exit: "100.64.0.1"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		d := &daemon{ctx: context.Background(), root: c.root, controller: c, status: Status{State: "stopped"}, load: func(string) (sessionProfile, error) { return sessionProfile{}, nil }}
		if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err == nil {
			t.Fatal("start accepted an unconfirmed child")
		}
		if _, _, err := d.handle(Request{Action: "stop"}); err == nil {
			t.Fatal("stop cleared recovery for a live child")
		}
		for _, name := range []string{"session-recovery.json", "active.json", "tailscale.json"} {
			if _, err := os.Lstat(c.path(name)); err != nil {
				t.Fatalf("lost %s: %v", name, err)
			}
		}
	}
}

func TestAbsentChildRestoresTailscaleBeforeRemovingJournal(t *testing.T) {
	c := testController(t)
	if err := writeJSON(c.path("session-recovery.json"), sessionRecovery{PID: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("office.1.json", c.path("active.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.path("tailscale.json"), tailState{Exit: "100.64.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.recoverStopped(); err == nil {
		t.Fatal("accepted unavailable Tailscale recovery")
	}
	if _, err := os.Lstat(c.path("session-recovery.json")); err != nil {
		t.Fatal("recovery journal was lost")
	}
	if err := writeJSON(c.path("tailscale-cli.json"), "/usr/local/bin/tailscale"); err != nil {
		t.Fatal(err)
	}
	if err := c.recoverStopped(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session-recovery.json", "active.json", "tailscale.json"} {
		if _, err := os.Lstat(c.path(name)); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed", name)
		}
	}
}

func TestResumeBackgroundRecoversAbsentChildAndPreservesLiveIntent(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			c := testController(t)
			pid := 1 << 30
			if live {
				pid = os.Getpid()
			}
			if err := writeJSON(c.path("session-recovery.json"), sessionRecovery{PID: pid}); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("office.1.json", c.path("active.json")); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(c.path("tailscale.json"), tailState{Exit: "100.64.0.1"}); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(c.path("tailscale-cli.json"), "/usr/local/bin/tailscale"); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(c.path("desired.json"), intent{Profile: "office"}); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			d := &daemon{ctx: context.Background(), root: c.root, controller: c, status: Status{State: "stopped", SavedProfile: "office"}, load: func(string) (sessionProfile, error) { return sessionProfile{}, nil }, run: func(ctx context.Context, _ sessionProfile, _ func(string)) error {
				close(started)
				<-ctx.Done()
				return nil
			}}
			err := d.resumeBackground("office")
			if live {
				if err == nil {
					t.Fatal("started with live saved PID")
				}
				for _, name := range []string{"session-recovery.json", "active.json", "tailscale.json"} {
					if _, err := os.Lstat(c.path(name)); err != nil {
						t.Fatalf("lost %s: %v", name, err)
					}
				}
				select {
				case <-started:
					t.Fatal("activated despite unresolved recovery")
				default:
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("background intent was not resumed")
				}
				if err := d.stopWorker(); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"session-recovery.json", "active.json", "tailscale.json"} {
					if _, err := os.Lstat(c.path(name)); !os.IsNotExist(err) {
						t.Fatalf("did not recover %s", name)
					}
				}
			}
			if saved, err := readIntent(c.root); err != nil || saved != "office" {
				t.Fatalf("lost intent: %q %v", saved, err)
			}
		})
	}
}
