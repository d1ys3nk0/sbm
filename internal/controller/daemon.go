package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Request struct {
	Action  string `json:"action"`
	Profile string `json:"profile,omitempty"`
}

type Status struct {
	State        string `json:"state"`
	Profile      string `json:"profile,omitempty"`
	SavedProfile string `json:"saved_profile,omitempty"`
	Error        string `json:"error,omitempty"`
	Health       string `json:"-"`
}

func (s Status) MarshalJSON() ([]byte, error) {
	state := "stopped"
	switch s.State {
	case "waiting-network", "connecting":
		state = "starting"
	case "connected", "running":
		state = "running"
	case "reconnecting", "profile-unavailable":
		state = "recovering"
	case "failed", "authentication-required":
		state = "failed"
	case "stopping":
		state = "stopping"
	}
	mode := ""
	if s.SavedProfile != "" {
		mode = "background"
	} else if s.Profile != "" && state != "stopped" {
		mode = "foreground"
	}
	message := s.Error
	if message == "" && s.State == "authentication-required" {
		message = "authentication required"
	} else if message == "" && s.State == "failed" {
		message = "session failed; check private logs"
	}
	health := s.Health
	if health == "" {
		health = "unknown"
	}
	return json.Marshal(struct {
		State          string            `json:"state"`
		Mode           string            `json:"mode"`
		Profile        string            `json:"profile"`
		DesiredProfile string            `json:"desired_profile"`
		Health         string            `json:"health"`
		Error          string            `json:"error"`
		Details        map[string]string `json:"details"`
	}{state, mode, s.Profile, s.SavedProfile, health, message, map[string]string{"stage": s.State}})
}

type sessionProfile struct{ Name string }

type runner func(context.Context, sessionProfile, func(string)) error

type daemon struct {
	ctx        context.Context
	root       string
	controller *controller
	run        runner
	load       func(string) (sessionProfile, error)
	control    sync.Mutex
	mu         sync.Mutex
	status     Status
	cancel     context.CancelFunc
	done       chan struct{}
	session    *os.File
	foreground bool
}

func (d *daemon) loadProfile(name string) (sessionProfile, error) {
	if d.load != nil {
		return d.load(name)
	}
	path, err := d.controller.sourcePath(name)
	if err != nil {
		return sessionProfile{}, err
	}
	var profile savedProfile
	if err := readJSON(path, &profile); err != nil || len(profile.Nodes) == 0 {
		return sessionProfile{}, errors.New("Unknown or invalid profile")
	}
	return sessionProfile{Name: name}, nil
}

func (d *daemon) snapshot() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *daemon) setState(state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if state == "healthy" || state == "unhealthy" {
		d.status.Health = state
		return
	}
	d.status.State = state
	if state != "running" {
		d.status.Health = "unknown"
	}
	if state == "connecting" {
		d.status.Error = ""
	}
}

func (d *daemon) stopWorker() error {
	if d.cancel != nil {
		d.cancel()
		<-d.done
		d.cancel = nil
		d.mu.Lock()
		failed := d.status.Error == "network recovery failed; check private logs"
		d.mu.Unlock()
		if failed {
			return errors.New("network recovery failed; check private logs")
		}
	}
	d.mu.Lock()
	d.status.Profile = ""
	d.status.State = "stopped"
	d.status.Error = ""
	d.mu.Unlock()
	return nil
}

func (d *daemon) launch(name string, persistent bool) <-chan struct{} {
	ctx, cancel := context.WithCancel(d.ctx)
	done := make(chan struct{})
	d.cancel, d.done = cancel, done
	d.mu.Lock()
	d.status.Profile = name
	d.status.State = "waiting-network"
	d.mu.Unlock()
	session := d.session
	d.session = nil
	go func() {
		defer close(done)
		if session != nil {
			defer session.Close()
		}
		delay := 5 * time.Second
		for ctx.Err() == nil {
			profile, err := d.loadProfile(name)
			if err != nil {
				d.setState("profile-unavailable")
				if !persistent || !pause(ctx, delay) {
					return
				}
				delay = min(delay*2, 5*time.Minute)
				continue
			}
			err = d.run(ctx, profile, func(state string) {
				if state == "connected" {
					delay = 5 * time.Second
				}
				d.setState(state)
			})
			if ctx.Err() != nil {
				if errors.Is(err, errSessionCleanup) {
					d.mu.Lock()
					d.status.State = "failed"
					d.status.Error = "network recovery failed; check private logs"
					d.mu.Unlock()
					fmt.Fprintln(os.Stderr, "sbm:", err)
				} else {
					d.setState("stopped")
				}
				return
			}
			if errors.Is(err, errSessionCleanup) {
				d.mu.Lock()
				d.status.State = "failed"
				d.status.Error = "network recovery failed; check private logs"
				d.mu.Unlock()
				return
			}
			if err == nil {
				d.setState("stopped")
			} else {
				d.mu.Lock()
				d.status.State = "failed"
				if errors.Is(err, errSessionCleanup) {
					d.status.Error = "network recovery failed; check private logs"
				}
				d.mu.Unlock()
				fmt.Fprintln(os.Stderr, "sbm:", err)
			}
			if (!persistent && !errors.Is(err, errUplinkChanged)) || !pause(ctx, delay) {
				return
			}
			delay = min(delay*2, 5*time.Minute)
		}
	}()
	return done
}

func (d *daemon) handle(req Request) (Status, <-chan struct{}, error) {
	if req.Action == "status" {
		return d.snapshot(), nil, nil
	}
	if req.Action == "update-geoip" {
		return d.updateGeoIP()
	}
	d.control.Lock()
	defer d.control.Unlock()
	if d.ctx.Err() != nil {
		return Status{}, nil, d.ctx.Err()
	}
	current := d.snapshot()

	if d.foreground && (req.Action == "start" || req.Action == "toggle" && (current.Profile == "" || current.State == "stopped" || current.State == "failed" || current.State == "authentication-required")) {
		return Status{}, nil, fmt.Errorf("foreground session is ending; retry the command")
	}
	switch req.Action {
	case "toggle":
		if !namePattern.MatchString(req.Profile) {
			return Status{}, nil, fmt.Errorf("invalid profile name")
		}
		if current.SavedProfile != "" || (current.Profile != "" && current.State != "stopped" && current.State != "failed" && current.State != "authentication-required") {
			if current.SavedProfile != "" && current.SavedProfile != req.Profile || current.Profile != "" && current.Profile != req.Profile {
				return Status{}, nil, fmt.Errorf("stop the existing session before changing profiles")
			}
			req.Action = "stop"
		} else {
			req.Action = "start"
		}
	case "start", "stop", "run":
	default:
		return Status{}, nil, fmt.Errorf("unsupported command")
	}
	if req.Action != "stop" {
		if _, err := d.loadProfile(req.Profile); err != nil {
			return Status{}, nil, err
		}
	}
	if req.Action == "run" && (current.SavedProfile != "" || (current.Profile != "" && current.State != "stopped" && current.State != "failed" && current.State != "authentication-required")) {
		return Status{}, nil, fmt.Errorf("stop the existing session before foreground run")
	}
	if req.Action == "start" {
		if current.SavedProfile != "" && current.SavedProfile != req.Profile || current.Profile != "" && current.Profile != req.Profile && current.State != "stopped" {
			return Status{}, nil, fmt.Errorf("stop the existing session before changing profiles")
		}
		if current.SavedProfile == req.Profile && current.State != "failed" && current.State != "authentication-required" && current.State != "stopped" {
			return current, nil, nil
		}
		if current.SavedProfile == "" && current.Profile != "" && current.State != "stopped" && current.State != "failed" && current.State != "authentication-required" {
			return Status{}, nil, fmt.Errorf("stop the foreground session before starting in the background")
		}
	}
	if req.Action == "start" || req.Action == "run" {
		// A finished worker can still have a cancel function until it is reaped.
		if err := d.stopWorker(); err != nil {
			return Status{}, nil, err
		}
		session, err := lockSession(d.ctx, d.root)
		if err != nil {
			return Status{}, nil, err
		}
		d.session = session
	}
	if req.Action != "run" {
		saved := ""
		if req.Action == "start" {
			saved = req.Profile
		}
		if err := writeJSON(filepath.Join(d.root, "desired.json"), intent{Profile: saved}); err != nil {
			if d.session != nil {
				d.session.Close()
				d.session = nil
			}
			return Status{}, nil, fmt.Errorf("cannot save connection state")
		}
		d.mu.Lock()
		d.status.SavedProfile = saved
		d.mu.Unlock()
	}
	if err := d.stopWorker(); err != nil {
		if d.session != nil {
			d.session.Close()
			d.session = nil
		}
		return Status{}, nil, err
	}
	if req.Action == "stop" {
		return d.snapshot(), nil, nil
	}
	done := d.launch(req.Profile, req.Action == "start")
	return d.snapshot(), done, nil
}

func (d *daemon) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&req); err != nil {
		return
	}
	if req.Action == "run" {
		_ = json.NewEncoder(conn).Encode(Status{Error: "run must execute in the foreground"})
		return
	}
	conn.SetReadDeadline(time.Time{})
	status, _, err := d.handle(req)
	if err != nil {
		status.Error = err.Error()
	}
	_ = json.NewEncoder(conn).Encode(status)
}

func serveDaemon(ctx context.Context, c *controller) error {
	root := c.root
	if err := CheckRoot(root); err != nil {
		return err
	}
	if err := CheckRoot(filepath.Join(root, "profiles")); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another SBM daemon owns this state: %w", err)
	}
	control, err := LockControl(ctx, root)
	if err != nil {
		return err
	}
	saved, err := readIntent(root)
	if err != nil {
		control.Close()
		return err
	}
	socket := filepath.Join(root, "control.sock")
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		control.Close()
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		control.Close()
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		control.Close()
		return err
	}
	d := daemon{ctx: ctx, root: root, controller: c, run: func(ctx context.Context, profile sessionProfile, state func(string)) error {
		return c.runSession(ctx, profile.Name, state)
	}, status: Status{State: "stopped", SavedProfile: saved}}
	if saved != "" {
		d.session, err = lockSession(ctx, root)
		if err != nil {
			control.Close()
			return err
		}
		d.launch(saved, true)
	} else if session, err := lockSession(ctx, root); err == nil {
		if err := c.tailscale(false); err != nil {
			fmt.Fprintln(os.Stderr, "sbm: could not clean stale network state:", err)
			d.status.State = "failed"
			d.status.Error = "network recovery failed; check private logs"
		}
		session.Close()
	}
	control.Close()
	go c.scheduleIdleGeoIP(ctx)
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			d.control.Lock()
			d.stopWorker()
			d.control.Unlock()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serve(conn)
	}
}

// Foreground runs a session in this process. The caller holds the control lock
// until ready receives a result, preventing commands from missing the socket.
func foreground(ctx context.Context, c *controller, name string, ready chan<- error) error {
	root := c.root
	if err := CheckRoot(root); err != nil {
		ready <- err
		return err
	}
	if err := CheckRoot(filepath.Join(root, "profiles")); err != nil {
		ready <- err
		return err
	}
	d := &daemon{ctx: ctx, root: root, controller: c, run: func(ctx context.Context, profile sessionProfile, state func(string)) error {
		return c.runSession(ctx, profile.Name, state)
	}, status: Status{State: "stopped"}, foreground: true}
	return foregroundSession(ctx, d, name, ready)
}

func foregroundSession(ctx context.Context, d *daemon, name string, ready chan<- error) error {
	root := d.root
	saved, err := readIntent(root)
	if err != nil {
		ready <- err
		return err
	}
	if saved != "" {
		err = fmt.Errorf("stop the background session before foreground run")
		ready <- err
		return err
	}
	_, done, err := d.handle(Request{Action: "run", Profile: name})
	if err != nil {
		ready <- err
		return err
	}
	socket := filepath.Join(root, "foreground.sock")
	if err = os.Remove(socket); err != nil && !os.IsNotExist(err) {
		d.stopWorker()
		ready <- err
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		d.stopWorker()
		ready <- err
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0600); err != nil {
		d.stopWorker()
		ready <- err
		return err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	ready <- nil
	select {
	case <-ctx.Done():
		d.control.Lock()
		d.stopWorker()
		d.control.Unlock()
		return ctx.Err()
	case <-done:
		switch d.snapshot().State {
		case "failed", "authentication-required", "profile-unavailable":
			return fmt.Errorf("foreground session failed; check private logs")
		}
		return nil
	}
}

func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *controller) scheduleIdleGeoIP(ctx context.Context) {
	attempt := func() {
		control, err := LockControl(ctx, c.root)
		if err != nil {
			return
		}
		session, err := lockSession(ctx, c.root)
		control.Close()
		if err != nil {
			return
		}
		defer session.Close()
		if err := c.updateGeoIP(ctx, true, nil, nil); err != nil && ctx.Err() == nil {
			fmt.Fprintln(c.errOut, "SBM GeoIP scheduled update failed")
		}
	}
	attempt()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt()
		}
	}
}

func (d *daemon) updateGeoIP() (Status, <-chan struct{}, error) {
	d.control.Lock()
	current := d.snapshot()
	active := d.cancel != nil && current.Profile != "" && current.State != "stopped" && current.State != "failed"
	done := d.done
	c := d.controller
	d.control.Unlock()
	if c == nil {
		return Status{}, nil, errors.New("Session update is unavailable")
	}
	if !active {
		session, err := lockSession(d.ctx, d.root)
		if err != nil {
			return Status{}, nil, err
		}
		defer session.Close()
		err = c.updateGeoIP(d.ctx, false, nil, nil)
		return d.snapshot(), nil, err
	}
	if c.geoipUpdates == nil {
		return Status{}, nil, errors.New("Session update is unavailable")
	}
	result := make(chan error, 1)
	select {
	case c.geoipUpdates <- result:
	case <-d.ctx.Done():
		return Status{}, nil, d.ctx.Err()
	case <-done:
		return Status{}, nil, errors.New("Session stopped during update")
	}
	select {
	case err := <-result:
		return d.snapshot(), nil, err
	case <-d.ctx.Done():
		return Status{}, nil, d.ctx.Err()
	case <-done:
		return Status{}, nil, errors.New("Session stopped during update")
	}
}
