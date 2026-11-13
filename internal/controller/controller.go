package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/d1ys3nk0/sbm/internal/inputs"
)

const (
	defaultRoot     = "/var/lib/sbm"
	job             = "system/io.github.d1ys3nk0.sbm"
	checkURL        = "https://www.gstatic.com/generate_204"
	bodyCheckURL    = "https://speed.cloudflare.com/__down?bytes=32768"
	bodyFallbackURL = "https://lil1-rbx.speedtest.network.ovh.net/files/1Mb.dat"
	uplinkURL       = "https://www.apple.com/library/test/success.html"
	probePort       = 17987
	directPort      = 17988
	apiPort         = 17986
)

var (
	namePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	generationPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*\.[0-9]+\.json$`)
	interfacePattern  = regexp.MustCompile(`(?m)^\s*interface:\s*(en[0-9]+)\s*$`)
	runningPattern    = regexp.MustCompile(`(?m)^\s*state = running$`)
)

type result struct {
	stdout string
	code   int
}

type controller struct {
	root         string
	bin          string
	out          io.Writer
	errOut       io.Writer
	runCommand   func(context.Context, string, string, ...string) result
	fetch        func(string, string) (inputs.Snapshot, error)
	fetchContext func(context.Context, string, string) (inputs.Snapshot, error)
	now          func() time.Time
	geoipUpdates chan chan error
	geoipUpdate  func(context.Context, bool, map[string]any, func() error) error
}

func defaultBinary() string {
	if runtime.GOARCH == "amd64" {
		return "/usr/local/bin/sing-box"
	}
	return "/opt/homebrew/bin/sing-box"
}

func (c *controller) run(timeout time.Duration, stdin, executable string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.runCommand(ctx, stdin, executable, args...)
}

func command(ctx context.Context, stdin, executable string, args ...string) result {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return result{stdout.String(), exit.ExitCode()}
		}
		return result{stdout.String(), -1}
	}
	return result{stdout.String(), 0}
}

var ErrUsage = errors.New("invalid command or arguments")

func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return MainContext(context.Background(), args, stdin, stdout, stderr)
}

func MainContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(stdout, "Usage: sbm <command> [arguments]")
		fmt.Fprintln(stdout, "Commands: install, uninstall, subscribe, import, profiles, check, start, run, stop, toggle, status, update-geoip")
		return nil
	}
	if len(args) == 0 {
		return ErrUsage
	}
	want := map[string]int{"subscribe": 3, "import": 3, "profiles": 1, "check": 2, "start": 2, "run": 2, "toggle": 2, "stop": 1, "status": 1, "daemon": 1, "update-geoip": 1, "install": 1, "uninstall": 1}
	n, ok := want[args[0]]
	if args[0] == "status" && len(args) == 2 && args[1] == "--json" {
		n = 2
	}
	if args[0] == "install" && len(args) == 2 && args[1] == "--dry-run" {
		n = 2
	}
	if !ok || len(args) != n {
		return ErrUsage
	}
	if (args[0] == "start" || args[0] == "run" || args[0] == "toggle" || args[0] == "check" || args[0] == "subscribe" || args[0] == "import") && !namePattern.MatchString(args[1]) {
		return ErrUsage
	}
	if args[0] == "import" && args[2] != "-" {
		return ErrUsage
	}
	if os.Geteuid() != 0 {
		return errors.New("Run SBM with sudo")
	}
	syscall.Umask(0077)
	c := &controller{root: defaultRoot, bin: defaultBinary(), out: stdout, errOut: stderr, runCommand: command, fetch: inputs.FetchSubscription, geoipUpdates: make(chan chan error), fetchContext: func(ctx context.Context, source, bin string) (inputs.Snapshot, error) {
		return inputs.FetchSubscriptionIn(ctx, source, bin, defaultRoot)
	}, now: time.Now}
	if err := c.configureBinary(); err != nil {
		return err
	}
	switch args[0] {
	case "subscribe":
		url := args[2]
		if url == "-" {
			line, err := bufio.NewReader(stdin).ReadString('\n')
			if err != nil && err != io.EOF {
				return errors.New("Could not read subscription URL")
			}
			url = strings.TrimSpace(line)
		}
		return c.subscribe(args[1], url)
	case "import":
		return c.importProfile(args[1], stdin)
	case "profiles":
		return c.profiles()
	case "check":
		return c.check(args[1])
	case "daemon":
		return serveDaemon(ctx, c)
	case "run":
		control, err := LockControl(ctx, c.root)
		if err != nil {
			return err
		}
		ready, done := make(chan error, 1), make(chan error, 1)
		go func() { done <- foreground(ctx, c, args[1], ready) }()
		err = <-ready
		control.Close()
		if err != nil {
			return err
		}
		return <-done
	case "start", "stop", "toggle", "status":
		request := Request{Action: args[0]}
		if len(args) > 1 && args[0] != "status" {
			request.Profile = args[1]
		}
		status, err := c.request(ctx, request)
		if err != nil {
			return err
		}
		if args[0] == "status" && len(args) == 2 {
			return json.NewEncoder(stdout).Encode(status)
		}
		fmt.Fprintf(stdout, "State: %s\nMode: %s\nProfile: %s\nDesired profile: %s\nHealth: %s\n", status.State, status.Mode, status.Profile, status.DesiredProfile, status.Health)
		if status.Error != "" {
			fmt.Fprintf(stdout, "Error: %s\n", status.Error)
		}
		return nil
	case "update-geoip":
		_, err := c.request(ctx, Request{Action: "update-geoip"})
		return err
	case "install":
		result, err := InstallResult(ctx, len(args) == 2)
		if err == nil {
			fmt.Fprintln(stdout, result)
		}
		return err
	case "uninstall":
		if _, err := os.Lstat(c.root); err == nil {
			if _, err := c.request(ctx, Request{Action: "stop"}); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return Uninstall(ctx)
	}
	return ErrUsage
}

func (c *controller) path(name string) string { return filepath.Join(c.root, name) }

func (c *controller) configureBinary() error {
	path := c.path("sing-box-bin.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Could not inspect sing-box path configuration")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("sing-box path configuration must be a root-owned, non-writable regular file")
	}
	var bin string
	if err := readJSON(path, &bin); err != nil || !filepath.IsAbs(bin) {
		return errors.New("sing-box path configuration must contain an absolute path")
	}
	c.bin = bin
	return nil
}

func readJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("Unsafe state file %s", filepath.Base(path))
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return fmt.Errorf("State file too large: %s", filepath.Base(path))
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("Invalid state file %s", filepath.Base(path))
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".sbm-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (c *controller) locked(fn func() error) error {
	f, err := os.OpenFile(c.path("operation.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("Could not open SBM operation lock")
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return errors.New("Could not acquire SBM operation lock")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (c *controller) sourcePath(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", errors.New("Use a lowercase subscription name, up to 64 characters")
	}
	return c.path(filepath.Join("profiles", name+".json")), nil
}

func (c *controller) active() (string, error) {
	generation, err := os.Readlink(c.path("active.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !generationPattern.MatchString(generation) {
		return "", errors.New("Invalid active SBM generation")
	}
	return generation, nil
}

func (c *controller) subscribe(name, url string) error {
	path, err := c.sourcePath(name)
	if err != nil {
		return err
	}
	checkExisting := func() (bool, error) {
		var profile savedProfile
		err := readJSON(path, &profile)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, errors.New("Could not read existing profile")
		}
		if profile.Policy == nil && profile.URL == url && len(profile.Nodes) != 0 {
			return true, nil
		}
		return false, errors.New("A different profile already exists under that name")
	}
	var exists bool
	if err := c.locked(func() error { var e error; exists, e = checkExisting(); return e }); err != nil {
		return err
	}
	if exists {
		return nil
	}
	var snapshot inputs.Snapshot
	if c.fetchContext != nil {
		snapshot, err = c.fetchContext(context.Background(), url, c.bin)
	} else {
		snapshot, err = c.fetch(url, c.bin)
	}
	if err != nil {
		return err
	}
	return c.locked(func() error {
		exists, err := checkExisting()
		if err != nil || exists {
			return err
		}
		return writeJSON(path, snapshot)
	})
}

func (c *controller) profiles() error {
	paths, err := filepath.Glob(c.path("profiles/*.json"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		fmt.Fprintln(c.out, strings.TrimSuffix(filepath.Base(path), ".json"))
	}
	return nil
}

func (c *controller) check(name string) error {
	path, err := c.sourcePath(name)
	if err != nil {
		return err
	}
	return c.locked(func() error {
		var profile savedProfile
		if err := readJSON(path, &profile); err != nil {
			return errors.New("Unknown or invalid profile")
		}
		config, err := c.buildProfile(profile)
		if err != nil {
			return err
		}
		if _, err := c.publish(name, config, true); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "Validated %d nodes with the system policy\n", len(profile.Nodes))
		return nil
	})
}

func (c *controller) connect(name string) error {
	return c.prepare(name, true)
}

func (c *controller) prepare(name string, launch bool) error {
	return c.prepareContext(context.Background(), name, launch)
}

func (c *controller) prepareContext(ctx context.Context, name string, launch bool) error {
	path, err := c.sourcePath(name)
	if err != nil {
		return err
	}
	var old savedProfile
	if err := readJSON(path, &old); err != nil {
		return errors.New("Unknown or invalid profile")
	}
	snapshot := old
	refreshed := false
	if old.Policy == nil {
		var updated inputs.Snapshot
		var fetchErr error
		if c.fetchContext != nil {
			updated, fetchErr = c.fetchContext(ctx, old.URL, c.bin)
		} else {
			updated, fetchErr = c.fetch(old.URL, c.bin)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		refreshed = fetchErr == nil
		if refreshed {
			snapshot.Snapshot = updated
		} else {
			fmt.Fprintln(c.errOut, "Using the last saved subscription")
		}
	}
	return c.locked(func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var current savedProfile
		if readJSON(path, &current) != nil || !reflect.DeepEqual(current, old) {
			return errors.New("Subscription changed during refresh. Retry connect")
		}
		previous, err := c.active()
		if err != nil {
			return err
		}
		buildAndPublish := func(profile savedProfile) (string, error) {
			config, err := c.buildProfile(profile)
			if err != nil {
				return "", err
			}
			return c.publish(name, config, false)
		}
		generation, err := buildAndPublish(snapshot)
		if err != nil && refreshed {
			snapshot = old
			generation, err = buildAndPublish(old)
			fmt.Fprintln(c.errOut, "Updated nodes failed validation. Using the last saved subscription")
		}
		if err != nil {
			return err
		}
		if err := writeJSON(path, snapshot); err != nil {
			return err
		}
		if launch {
			if err := c.reconcile(generation, previous != generation); err != nil {
				return err
			}
		}
		paths, _ := filepath.Glob(c.path("*.json"))
		for _, path := range paths {
			base := filepath.Base(path)
			if generationPattern.MatchString(base) && base != generation {
				_ = os.Remove(path)
			}
		}
		return nil
	})
}

func (c *controller) stop() error {
	return c.locked(func() error {
		if err := os.Remove(c.path("active.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncDir(c.root); err != nil {
			return err
		}
		return c.reconcile("", false)
	})
}

func (c *controller) publish(name string, config map[string]any, checkOnly bool) (string, error) {
	dir, err := os.MkdirTemp(c.root, "candidate-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	candidate := filepath.Join(dir, "config.json")
	if err := writeJSON(candidate, config); err != nil {
		return "", err
	}
	if c.run(15*time.Second, "", c.bin, "check", "-c", candidate).code != 0 {
		return "", errors.New("sing-box rejected the generated configuration")
	}
	if checkOnly {
		return "", nil
	}
	generation := name + "." + strconv.FormatInt(c.now().UnixNano(), 10) + ".json"
	if err := os.Rename(candidate, c.path(generation)); err != nil {
		return "", err
	}
	link := filepath.Join(dir, "active")
	if err := os.Symlink(generation, link); err != nil {
		return "", err
	}
	if err := os.Rename(link, c.path("active.json")); err != nil {
		return "", err
	}
	return generation, syncDir(c.root)
}
