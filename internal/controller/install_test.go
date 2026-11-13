package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func installerFixture(t *testing.T) (installation, *[]string) {
	t.Helper()
	root := t.TempDir()
	write := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte("test executable\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("source", 0755)
	write("singbox", 0755)
	var calls []string
	return installation{packageDir: filepath.Join(root, "opt", "sbm"), dataDir: filepath.Join(root, "var", "lib", "sbm"), command: filepath.Join(root, "usr", "local", "bin", "sbm"), plist: filepath.Join(root, "Library", "LaunchDaemons", serviceLabel+".plist"), source: filepath.Join(root, "source"), singbox: filepath.Join(root, "singbox"), uid: os.Geteuid(), service: func(ctx context.Context, action, path string) error { calls = append(calls, action); return ctx.Err() }}, &calls
}

func TestInstallDryRunIsNonMutating(t *testing.T) {
	i, calls := installerFixture(t)
	if err := i.install(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{i.packageDir, i.dataDir, i.command, i.plist} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview created %s: %v", path, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatal("preview touched launchd")
	}
}

func TestInstallRepeatUpgradeAndUninstallPreserveData(t *testing.T) {
	previousMask := syscall.Umask(0077)
	defer syscall.Umask(previousMask)
	i, calls := installerFixture(t)
	ctx := context.Background()
	for range 2 {
		if err := i.install(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{i.command} {
		target, err := os.Readlink(path)
		if err != nil || target != i.binary() {
			t.Fatalf("wrong command link: %s %v", target, err)
		}
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{i.binary(), 0755}, {i.plist, 0644}, {i.recordPath(), 0600}, {i.dataDir, 0700}, {filepath.Join(i.dataDir, "profiles"), 0700}, {filepath.Join(i.dataDir, "service.log"), 0600}} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("wrong permissions: %s %v", item.path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(i.dataDir, "desired.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fresh installation saved intent")
	}
	if err := os.WriteFile(i.source, []byte("upgraded executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := i.install(ctx, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(i.binary())
	if err != nil || string(data) != "upgraded executable" {
		t.Fatal("upgrade did not install executable")
	}
	secret := filepath.Join(i.dataDir, "profiles", "private")
	if err := os.WriteFile(secret, []byte("private profile"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := i.uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if err := i.uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{i.packageDir, i.command, i.plist} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uninstall retained %s", path)
		}
	}
	if info, err := os.Stat(i.recordPath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private installation ownership state was not preserved")
	}
	if filepath.Dir(i.recordPath()) != i.dataDir {
		t.Fatal("installation state escaped private data directory")
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "private profile" {
		t.Fatal("uninstall removed private data")
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start", "stop", "start", "stop", "start", "stop"}) {
		t.Fatalf("unexpected service requests: %v", *calls)
	}
}

func TestInstallRefusesConflictsAndMissingDependenciesWithoutMutation(t *testing.T) {
	for _, kind := range []string{"command", "plist", "dependency"} {
		t.Run(kind, func(t *testing.T) {
			i, calls := installerFixture(t)
			path := map[string]string{"command": i.command, "plist": i.plist, "dependency": i.singbox}[kind]
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if kind == "dependency" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("unrelated resource"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := i.install(context.Background(), false); err == nil {
				t.Fatal("accepted conflict")
			}
			if len(*calls) != 0 {
				t.Fatal("modified service on preflight failure")
			}
			if kind != "dependency" {
				if data, err := os.ReadFile(path); err != nil || string(data) != "unrelated resource" {
					t.Fatal("changed unrelated destination")
				}
			}
		})
	}
}

func TestInstallRefusesActiveIntentAndSession(t *testing.T) {
	for _, kind := range []string{"intent", "session", "control"} {
		t.Run(kind, func(t *testing.T) {
			i, calls := installerFixture(t)
			if err := os.MkdirAll(i.dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "intent" {
				if err := os.WriteFile(filepath.Join(i.dataDir, "desired.json"), []byte(`{"profile":"office"}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				file, err := os.OpenFile(filepath.Join(i.dataDir, kind+".lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			for _, dry := range []bool{true, false} {
				if err := i.install(context.Background(), dry); err == nil {
					t.Fatal("installation accepted active session")
				}
			}
			if len(*calls) != 0 {
				t.Fatal("interrupted active service")
			}
		})
	}
}

func TestInstallCanResumeAfterServiceFailure(t *testing.T) {
	i, _ := installerFixture(t)
	ctx := context.Background()
	i.service = func(_ context.Context, action, _ string) error {
		if action == "start" {
			return errors.New("injected launchd failure")
		}
		return nil
	}
	if err := i.install(ctx, false); err == nil {
		t.Fatal("lost registration failure")
	}
	i.service = func(context.Context, string, string) error { return nil }
	if err := i.install(ctx, false); err != nil {
		t.Fatalf("cannot resume interrupted installation: %v", err)
	}
	if err := os.WriteFile(i.binary(), []byte("unrelated binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := i.install(ctx, false); err == nil {
		t.Fatal("overwrote tampered destination")
	}
	if err := i.uninstall(ctx); err == nil {
		t.Fatal("removed unowned destination")
	}
}

func TestInstallRejectsSymlinkedPrivateDirectory(t *testing.T) {
	i, _ := installerFixture(t)
	if err := os.MkdirAll(filepath.Dir(i.dataDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), i.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := i.install(context.Background(), false); err == nil {
		t.Fatal("accepted symlinked private directory")
	}
}

func TestInstallResultSkipsIdenticalRegisteredService(t *testing.T) {
	i, calls := installerFixture(t)
	ctx := context.Background()
	if result, err := i.installResult(ctx, true); err != nil || result != "would install" {
		t.Fatalf("unexpected preview %s: %v", result, err)
	}
	if result, err := i.installResult(ctx, false); err != nil || result != "installed" {
		t.Fatalf("unexpected install %s: %v", result, err)
	}
	before := len(*calls)
	for _, dry := range []bool{true, false} {
		if result, err := i.installResult(ctx, dry); err != nil || result != "unchanged" {
			t.Fatalf("unexpected repeat %s: %v", result, err)
		}
	}
	if !reflect.DeepEqual((*calls)[before:], []string{"inspect", "inspect"}) {
		t.Fatal("identical install restarted service")
	}
}
