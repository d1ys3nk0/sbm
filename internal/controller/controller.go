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
	defaultRoot     = "/Library/Application Support/apexplane-sbm"
	job             = "system/com.apexplane.sbm"
	checkURL        = "https://www.gstatic.com/generate_204"
	bodyCheckURL    = "https://speed.cloudflare.com/__down?bytes=32768"
	bodyCheckResult = "200 32768"
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
	root   string
	bin    string
	out    io.Writer
	errOut io.Writer
	run    func(time.Duration, string, string, ...string) result
	fetch  func(string, string) (inputs.Snapshot, error)
	now    func() time.Time
}

func defaultBinary() string {
	if runtime.GOARCH == "amd64" {
		return "/usr/local/bin/sing-box"
	}
	return "/opt/homebrew/bin/sing-box"
}

func command(timeout time.Duration, stdin, executable string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
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

func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("Usage: sbm subscribe|subscriptions|check|connect|stop|status|watchdog|update-geoip|run")
	}
	want := map[string]int{"subscribe": 3, "subscriptions": 1, "check": 2, "connect": 2, "stop": 1, "status": 1, "watchdog": 1, "update-geoip": 1, "run": 1}
	n, ok := want[args[0]]
	if !ok || len(args) != n {
		return errors.New("Invalid SBM command or arguments")
	}
	if os.Geteuid() != 0 {
		return errors.New("Run SBM with sudo")
	}
	syscall.Umask(0077)
	c := &controller{root: defaultRoot, bin: defaultBinary(), out: stdout, errOut: stderr, run: command, fetch: inputs.FetchSubscription, now: time.Now}
	if err := c.configureBinary(); err != nil {
		return err
	}
	switch args[0] {
	case "run":
		return syscall.Exec(c.bin, []string{c.bin, "run", "-c", filepath.Join(c.root, "active.json")}, os.Environ())
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
	case "subscriptions":
		return c.subscriptions()
	case "check":
		return c.check(args[1])
	case "connect":
		return c.connect(args[1])
	case "stop":
		return c.stop()
	case "status":
		return c.status()
	case "watchdog":
		if err := c.watchdog(); err != nil {
			return err
		}
		return inputs.UpdateGeoIP(c.root, c.bin, true)
	case "update-geoip":
		return inputs.UpdateGeoIP(c.root, c.bin, false)
	}
	return nil
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
	data, err := os.ReadFile(path)
	if err != nil {
		return err
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
	return c.path(filepath.Join("subscriptions", name+".json")), nil
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
	snapshot, err := c.fetch(url, c.bin)
	if err != nil {
		return err
	}
	return c.locked(func() error { return writeJSON(path, snapshot) })
}

func (c *controller) subscriptions() error {
	paths, err := filepath.Glob(c.path("subscriptions/*.json"))
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
		var snapshot inputs.Snapshot
		if err := readJSON(path, &snapshot); err != nil {
			return errors.New("Unknown or invalid subscription")
		}
		config, err := c.build(snapshot.Nodes)
		if err != nil {
			return err
		}
		if _, err := c.publish(name, config, true); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "Validated %d nodes with the system policy\n", len(snapshot.Nodes))
		return nil
	})
}

func (c *controller) connect(name string) error {
	path, err := c.sourcePath(name)
	if err != nil {
		return err
	}
	var old inputs.Snapshot
	if err := readJSON(path, &old); err != nil {
		return errors.New("Unknown or invalid subscription")
	}
	snapshot, err := c.fetch(old.URL, c.bin)
	refreshed := err == nil
	if !refreshed {
		snapshot = old
		fmt.Fprintln(c.errOut, "Using the last saved subscription")
	}
	return c.locked(func() error {
		var current inputs.Snapshot
		if readJSON(path, &current) != nil || !reflect.DeepEqual(current, old) {
			return errors.New("Subscription changed during refresh. Retry connect")
		}
		previous, err := c.active()
		if err != nil {
			return err
		}
		buildAndPublish := func(nodes []map[string]any) (string, error) {
			config, err := c.build(nodes)
			if err != nil {
				return "", err
			}
			return c.publish(name, config, false)
		}
		generation, err := buildAndPublish(snapshot.Nodes)
		if err != nil && refreshed {
			snapshot = old
			generation, err = buildAndPublish(old.Nodes)
			fmt.Fprintln(c.errOut, "Updated nodes failed validation. Using the last saved subscription")
		}
		if err != nil {
			return err
		}
		if err := writeJSON(path, snapshot); err != nil {
			return err
		}
		if err := c.reconcile(generation, previous != generation); err != nil {
			return err
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
