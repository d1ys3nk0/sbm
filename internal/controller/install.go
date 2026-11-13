package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

const serviceLabel = "io.github.d1ys3nk0.sbm"

type installation struct {
	packageDir, dataDir, command, plist, source, singbox string
	uid                                                  int
	service                                              func(context.Context, string, string) error
}

type installRecord struct {
	Label    string   `json:"label"`
	Binaries []string `json:"binaries"`
	Services []string `json:"services"`
}

func systemInstallation() (installation, error) {
	source, err := os.Executable()
	c := &controller{root: "/var/lib/sbm", bin: defaultBinary()}
	if err != nil {
		return installation{}, err
	}
	if err := c.configureBinary(); err != nil {
		return installation{}, err
	}
	return installation{packageDir: "/opt/sbm", dataDir: "/var/lib/sbm", command: "/usr/local/bin/sbm", plist: "/Library/LaunchDaemons/" + serviceLabel + ".plist", source: source, singbox: c.bin, uid: 0, service: launchService}, nil
}

func launchService(ctx context.Context, action, plist string) error {
	if action == "inspect" || action == "stop" {
		if err := exec.CommandContext(ctx, "/bin/launchctl", "print", "system/"+serviceLabel).Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && (exit.ExitCode() == 113 || exit.ExitCode() == 3) {
				if action == "inspect" {
					return fs.ErrNotExist
				}
				return nil
			}
			return errors.New("cannot inspect installed SBM service")
		}
		if action == "inspect" {
			return nil
		}
		if err := exec.CommandContext(ctx, "/bin/launchctl", "bootout", "system/"+serviceLabel).Run(); err != nil {
			return errors.New("cannot stop installed SBM service")
		}
		return nil
	}
	if err := exec.CommandContext(ctx, "/bin/launchctl", "enable", "system/"+serviceLabel).Run(); err != nil {
		return errors.New("cannot enable SBM service")
	}
	if err := exec.CommandContext(ctx, "/bin/launchctl", "bootstrap", "system", plist).Run(); err != nil {
		return errors.New("cannot register SBM service")
	}
	return nil
}

// Install packages the current executable; a new service has no connection intent.
func Install(ctx context.Context, dryRun bool) error {
	_, err := InstallResult(ctx, dryRun)
	return err
}

func InstallResult(ctx context.Context, dryRun bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	i, err := systemInstallation()
	if err != nil {
		return "", err
	}
	return i.installResult(ctx, dryRun)
}

func (i installation) installResult(ctx context.Context, dryRun bool) (string, error) {
	if err := i.dependencies(); err != nil {
		return "", err
	}
	_, owned, err := i.inspect()
	if err != nil {
		return "", err
	}
	if owned {
		source, sourceErr := os.ReadFile(i.source)
		binary, binaryErr := os.ReadFile(i.binary())
		plist, plistErr := os.ReadFile(i.plist)
		complete := sourceErr == nil && binaryErr == nil && plistErr == nil && hashBytes(source) == hashBytes(binary) && hashBytes(plist) == hashBytes(i.serviceDefinition())
		for _, path := range []string{i.command, i.dataDir, filepath.Join(i.dataDir, "profiles"), filepath.Join(i.dataDir, "service.log")} {
			if _, err := os.Lstat(path); err != nil {
				complete = false
			}
		}
		if complete {
			if err := i.service(ctx, "inspect", i.plist); err == nil {
				return "unchanged", nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
		}
	}
	if err := i.install(ctx, dryRun); err != nil {
		return "", err
	}
	if dryRun {
		return "would install", nil
	}
	return "installed", nil
}

// Uninstall preserves all profiles, recovery records and other private data.
func Uninstall(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	i, err := systemInstallation()
	if err != nil {
		return err
	}
	return i.uninstall(ctx)
}

func hashBytes(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (i installation) binary() string     { return filepath.Join(i.packageDir, "bin", "sbm") }
func (i installation) recordPath() string { return filepath.Join(i.packageDir, "install.json") }

func (i installation) secure(path string, directory bool, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != i.uid || info.IsDir() != directory || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		return fmt.Errorf("unsafe installation resource: %s", path)
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	return nil
}

func (i installation) link(path, target string, owned bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	actual, err := os.Readlink(path)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !owned || err != nil || actual != target || !ok || int(stat.Uid) != i.uid {
		return fmt.Errorf("unrelated destination conflict: %s", path)
	}
	return nil
}

func (i installation) inspect() (installRecord, bool, error) {
	var record installRecord
	for _, dir := range []string{i.packageDir, filepath.Join(i.packageDir, "bin")} {
		if err := i.secure(dir, true, 0755); err != nil {
			return record, false, err
		}
	}
	if err := i.secure(i.dataDir, true, 0700); err != nil {
		return record, false, err
	}
	if err := i.secure(filepath.Join(i.dataDir, "profiles"), true, 0700); err != nil {
		return record, false, err
	}
	if err := i.secure(filepath.Join(i.dataDir, "service.log"), false, 0600); err != nil {
		return record, false, err
	}
	if err := i.secure(i.recordPath(), false, 0600); err != nil {
		return record, false, err
	}
	data, err := os.ReadFile(i.recordPath())
	owned := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return record, false, err
	}
	if owned && (json.Unmarshal(data, &record) != nil || record.Label != serviceLabel || len(record.Binaries) == 0 || len(record.Services) == 0) {
		return record, false, errors.New("invalid SBM installation ownership record")
	}
	for _, item := range []struct {
		path   string
		mode   fs.FileMode
		hashes []string
	}{{i.binary(), 0755, record.Binaries}, {i.plist, 0644, record.Services}} {
		if err := i.secure(item.path, false, item.mode); err != nil {
			return record, false, err
		}
		content, err := os.ReadFile(item.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return record, false, err
		}
		if !owned || !slices.Contains(item.hashes, hashBytes(content)) {
			return record, false, fmt.Errorf("unrelated destination conflict: %s", item.path)
		}
	}
	for _, item := range []struct{ path, target string }{{i.command, i.binary()}} {
		if err := i.link(item.path, item.target, owned); err != nil {
			return record, false, err
		}
	}
	for _, dir := range []struct {
		path  string
		names []string
	}{{i.packageDir, []string{"bin", "install.json"}}, {filepath.Join(i.packageDir, "bin"), []string{"sbm"}}} {
		entries, err := os.ReadDir(dir.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return record, false, err
		}
		for _, entry := range entries {
			if !slices.Contains(dir.names, entry.Name()) {
				return record, false, fmt.Errorf("unrelated package resource: %s", filepath.Join(dir.path, entry.Name()))
			}
		}
	}
	return record, owned, nil
}

func (i installation) idle() error {
	profile, err := readIntent(i.dataDir)
	if err != nil {
		return err
	}
	if profile != "" {
		return errors.New("stop the requested SBM session before changing installation")
	}
	return nil
}

func (i installation) locks() (func(), error) {
	var files []*os.File
	closeAll := func() {
		for n := len(files) - 1; n >= 0; n-- {
			files[n].Close()
		}
	}
	for _, name := range []string{"control.lock", "session.lock"} {
		path := filepath.Join(i.dataDir, name)
		if err := i.secure(path, false, 0600); err != nil {
			closeAll()
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			closeAll()
			return nil, err
		}
		files = append(files, file)
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			closeAll()
			return nil, errors.New("SBM operation or session is active; stop it before changing installation")
		}
	}
	return closeAll, nil
}

func (i installation) serviceDefinition() []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>daemon</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>Umask</key><integer>63</integer>
<key>StandardOutPath</key><string>%s/service.log</string>
<key>StandardErrorPath</key><string>%s/service.log</string>
</dict></plist>
`, serviceLabel, i.binary(), i.dataDir, i.dataDir))
}

func atomicInstall(path string, data []byte, mode fs.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".sbm-install-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = file.Chmod(mode)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func makeInstallDirectory(path string, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if err == nil {
		if path == "/var" && info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			stat, ok := info.Sys().(*syscall.Stat_t)
			if err == nil && target == "/private/var" && ok && stat.Uid == 0 {
				return nil
			}
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("installation parent is not a directory: %s", path)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = makeInstallDirectory(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err = os.Mkdir(path, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (i installation) dependencies() error {
	for _, dependency := range []struct {
		path       string
		executable bool
	}{{i.source, true}, {i.singbox, true}} {
		info, err := os.Stat(dependency.path)
		if err != nil || !info.Mode().IsRegular() || dependency.executable && info.Mode().Perm()&0111 == 0 {
			return errors.New("required executable or sing-box is unavailable")
		}
	}

	return nil
}

func (i installation) install(ctx context.Context, dryRun bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := i.dependencies(); err != nil {
		return err
	}
	record, _, err := i.inspect()
	if err != nil {
		return err
	}
	if err = i.idle(); err != nil {
		return err
	}
	if dryRun {
		// Opening existing locks without O_CREATE keeps previews non-mutating.
		for _, name := range []string{"control.lock", "session.lock"} {
			path := filepath.Join(i.dataDir, name)
			if err = i.secure(path, false, 0600); err != nil {
				return err
			}
			file, e := os.OpenFile(path, os.O_RDWR, 0)
			if errors.Is(e, fs.ErrNotExist) {
				continue
			}
			if e != nil {
				return e
			}
			e = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			file.Close()
			if e != nil {
				return errors.New("SBM operation or session is active")
			}
		}
		return nil
	}
	if err = makeInstallDirectory(i.dataDir, 0700); err != nil {
		return err
	}
	unlock, err := i.locks()
	if err != nil {
		return err
	}
	defer unlock()
	record, _, err = i.inspect()
	if err != nil {
		return err
	}
	if err = i.idle(); err != nil {
		return err
	}
	data, err := os.ReadFile(i.source)
	if err != nil {
		return err
	}
	plist := i.serviceDefinition()
	record.Label = serviceLabel
	if hash := hashBytes(data); !slices.Contains(record.Binaries, hash) {
		record.Binaries = append(record.Binaries, hash)
	}
	if hash := hashBytes(plist); !slices.Contains(record.Services, hash) {
		record.Services = append(record.Services, hash)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	for _, dir := range []struct {
		path string
		mode fs.FileMode
	}{{i.packageDir, 0755}, {filepath.Join(i.packageDir, "bin"), 0755}, {filepath.Dir(i.command), 0755}, {filepath.Dir(i.plist), 0755}, {filepath.Join(i.dataDir, "profiles"), 0700}} {
		if dir.path == filepath.Join(i.dataDir, "profiles") {
			if err = i.secure(dir.path, true, 0700); err != nil {
				return err
			}
		}
		if err = makeInstallDirectory(dir.path, dir.mode); err != nil {
			return err
		}
	}
	logPath := filepath.Join(i.dataDir, "service.log")
	if err = i.secure(logPath, false, 0600); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err = log.Close(); err != nil {
		return err
	}
	if err = atomicInstall(i.recordPath(), encoded, 0600); err != nil {
		return err
	}
	if err = i.service(ctx, "stop", i.plist); err != nil {
		return err
	}
	if err = atomicInstall(i.binary(), data, 0755); err != nil {
		return err
	}
	for _, path := range []string{i.command} {
		if _, err = os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			if err = os.Symlink(i.binary(), path); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if err = atomicInstall(i.plist, plist, 0644); err != nil {
		return err
	}
	return i.service(ctx, "start", i.plist)
}

func (i installation) uninstall(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, owned, err := i.inspect()
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if err = i.idle(); err != nil {
		return err
	}
	if err = makeInstallDirectory(i.dataDir, 0700); err != nil {
		return err
	}
	unlock, err := i.locks()
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err = i.inspect(); err != nil {
		return err
	}
	if err = i.idle(); err != nil {
		return err
	}
	if err = i.service(ctx, "stop", i.plist); err != nil {
		return err
	}
	for _, path := range []string{i.command, i.binary(), i.plist, i.recordPath(), filepath.Join(i.packageDir, "bin"), i.packageDir} {
		if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
