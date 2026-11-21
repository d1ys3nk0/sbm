package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestForegroundSocketAndExclusiveSession(t *testing.T) {
	for _, externalStop := range []bool{true, false} {
		t.Run(fmt.Sprintf("external-stop-%t", externalStop), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "sbm-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			var alerts []string
			d := &daemon{
				ctx: ctx, root: root, foreground: true, status: Status{State: "stopped"},
				notify: func(body string) { alerts = append(alerts, body) },
				load:   func(string) (sessionProfile, error) { return sessionProfile{}, nil },
				run: func(ctx context.Context, _ sessionProfile, state func(string), probe func(verifiedNode)) error {
					state("running")
					probe(verifiedNode{"first", "North"})
					probe(verifiedNode{"second", "West"})
					probe(verifiedNode{})
					probe(verifiedNode{})
					probe(verifiedNode{"second", "West"})
					probe(verifiedNode{})
					probe(verifiedNode{"third", "East"})
					close(started)
					<-ctx.Done()
					return ctx.Err()
				},
			}
			ready := make(chan error, 1)
			done := make(chan error, 1)
			go func() { done <- foregroundSession(ctx, d, "office", ready) }()
			if err := <-ready; err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("session did not start")
			}
			other := &daemon{ctx: ctx, root: root, status: Status{State: "stopped"}, load: d.load, run: d.run}
			if _, _, err := other.handle(Request{Action: "start", Profile: "other"}); err == nil {
				t.Fatal("background session acquired an owned foreground session")
			}
			if saved, err := readIntent(root); err != nil || saved != "" {
				t.Fatalf("conflicting start changed desired state: %q, %v", saved, err)
			}
			if externalStop {
				conn, err := net.Dial("unix", filepath.Join(root, "foreground.sock"))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.NewEncoder(conn).Encode(Request{Action: "stop"}); err != nil {
					t.Fatal(err)
				}
				var response map[string]any
				if err := json.NewDecoder(conn).Decode(&response); err != nil {
					t.Fatal(err)
				}
				conn.Close()
				if response["state"] != "stopped" || response["error"] != "" {
					t.Fatalf("stop response = %#v", response)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if externalStop && err != nil || !externalStop && !errors.Is(err, context.Canceled) {
					t.Fatalf("foreground result = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("foreground did not exit")
			}
			want := []string{
				"Connected · office · North",
				"Node switched · office · North → West",
				"Connection lost · office · West",
				"Connection recovered · office · West",
				"Connection lost · office · West",
				"Connection recovered · office · West → East",
				"Disconnected · office · East",
			}
			if !slices.Equal(alerts, want) {
				t.Fatalf("alerts = %q, want %q", alerts, want)
			}
			if saved, err := readIntent(root); err != nil || saved != "" {
				t.Fatalf("foreground persisted intent: %q, %v", saved, err)
			}
		})
	}
}

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("session did not transition")
	}
}

func TestSavedProfileSurvivesDaemonRestartAndStopCancels(t *testing.T) {
	root := t.TempDir()
	newDaemon := func(started, cancelled chan struct{}) *daemon {
		saved, err := readIntent(root)
		if err != nil {
			t.Fatal(err)
		}
		d := &daemon{
			ctx: context.Background(), root: root, status: Status{State: "stopped", SavedProfile: saved},
			load: func(name string) (sessionProfile, error) {
				if name != "office" {
					return sessionProfile{}, errors.New("unexpected profile")
				}
				return sessionProfile{}, nil
			},
			run: func(ctx context.Context, _ sessionProfile, state func(string), _ func(verifiedNode)) error {
				state("connecting")
				close(started)
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			},
		}
		if saved != "" {
			d.launch(saved, true)
		}
		return d
	}
	started, cancelled := make(chan struct{}), make(chan struct{})
	first := newDaemon(started, cancelled)
	if _, _, err := first.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	first.stopWorker()
	wait(t, cancelled)

	started, cancelled = make(chan struct{}), make(chan struct{})
	second := newDaemon(started, cancelled)
	wait(t, started)
	if got := second.snapshot(); got.SavedProfile != "office" || got.State != "connecting" {
		t.Fatalf("restored status = %+v", got)
	}
	if _, _, err := second.handle(Request{Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	wait(t, cancelled)
	if saved, err := readIntent(root); err != nil || saved != "" {
		t.Fatalf("saved state after stop = %q, %v", saved, err)
	}
}

func TestStartAndToggleRequireStoppingBeforeProfileChange(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{}, 2)
	d := &daemon{
		ctx: context.Background(), root: root, status: Status{State: "stopped"},
		load: func(string) (sessionProfile, error) { return sessionProfile{}, nil },
		run: func(ctx context.Context, _ sessionProfile, state func(string), _ func(verifiedNode)) error {
			state("connecting")
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("repeated start restarted the session")
	default:
	}
	for _, action := range []string{"start", "toggle"} {
		if _, _, err := d.handle(Request{Action: action, Profile: "other"}); err == nil {
			t.Fatalf("%s accepted a different profile", action)
		}
		if got := d.snapshot(); got.SavedProfile != "office" || got.Profile != "office" {
			t.Fatalf("%s changed the existing session: %+v", action, got)
		}
	}
	if _, _, err := d.handle(Request{Action: "stop"}); err != nil {
		t.Fatal(err)
	}
}

func TestStopCancelsInProgressGeoIPUpdate(t *testing.T) {
	root := t.TempDir()
	c := &controller{geoipUpdates: make(chan chan error)}
	started := make(chan struct{})
	updating := make(chan struct{})
	d := &daemon{ctx: context.Background(), root: root, controller: c, status: Status{State: "stopped"},
		load: func(string) (sessionProfile, error) { return sessionProfile{}, nil },
		run: func(ctx context.Context, _ sessionProfile, state func(string), _ func(verifiedNode)) error {
			state("running")
			close(started)
			select {
			case reply := <-c.geoipUpdates:
				close(updating)
				<-ctx.Done()
				reply <- ctx.Err()
				return nil
			case <-ctx.Done():
				return nil
			}
		},
	}
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	updateDone := make(chan struct{})
	go func() { d.handle(Request{Action: "update-geoip"}); close(updateDone) }()
	wait(t, updating)
	stopDone := make(chan error, 1)
	go func() { _, _, err := d.handle(Request{Action: "stop"}); stopDone <- err }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop waited on update")
	}
	wait(t, updateDone)
}

func TestIdleGeoIPDoesNotBlockStopAndCancelsOnShutdown(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	c := &controller{root: root, geoipUpdate: func(ctx context.Context, _ bool, _ map[string]any, _ func() error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	d := &daemon{ctx: ctx, root: root, controller: c, status: Status{State: "stopped"}}
	updateDone := make(chan error, 1)
	go func() { _, _, err := d.handle(Request{Action: "update-geoip"}); updateDone <- err }()
	wait(t, started)
	stopDone := make(chan error, 1)
	go func() { _, _, err := d.handle(Request{Action: "stop"}); stopDone <- err }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle update blocked stop")
	}
	cancel()
	select {
	case err := <-updateDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("update result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel idle update")
	}
}

func TestBackgroundNotificationsAcrossAutomaticRetry(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		alerts := make(chan string, 16)
		attempts := 0
		d := &daemon{
			ctx: ctx, root: root, status: Status{State: "stopped"},
			load:   func(string) (sessionProfile, error) { return sessionProfile{}, nil },
			notify: func(body string) { alerts <- body },
			run: func(ctx context.Context, _ sessionProfile, state func(string), probe func(verifiedNode)) error {
				attempts++
				if attempts == 1 {
					probe(verifiedNode{"north", "North"})
					return errors.New("unexpected exit")
				}
				if attempts == 2 {
					return errors.New("still unavailable")
				}
				probe(verifiedNode{"west", "West"})
				<-ctx.Done()
				return ctx.Err()
			},
		}
		expect := func(want string) {
			t.Helper()
			select {
			case got := <-alerts:
				if got != want {
					t.Fatalf("alert = %q, want %q", got, want)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("missing alert %q", want)
			}
		}
		if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
			t.Fatal(err)
		}
		expect("Connected · office · North")
		expect("Connection lost · office · North")
		expect("Connection recovered · office · North → West")
		if _, _, err := d.handle(Request{Action: "stop"}); err != nil {
			t.Fatal(err)
		}
		expect("Disconnected · office · West")

		select {
		case got := <-alerts:
			t.Fatalf("unexpected alert %q", got)
		default:
		}
	})
}
