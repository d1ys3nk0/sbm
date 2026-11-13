package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func lockFile(ctx context.Context, path string, wait bool) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("unsafe lock file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !wait || (err != syscall.EWOULDBLOCK && err != syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		if !pause(ctx, 25*time.Millisecond) {
			file.Close()
			return nil, ctx.Err()
		}
	}
}

// LockControl serializes commands with session startup and background recovery.
func LockControl(ctx context.Context, root string) (*os.File, error) {
	if err := CheckRoot(root); err != nil {
		return nil, err
	}
	return lockFile(ctx, filepath.Join(root, "control.lock"), true)
}

func lockSession(ctx context.Context, root string) (*os.File, error) {
	file, err := lockFile(ctx, filepath.Join(root, "session.lock"), false)
	if err != nil {
		return nil, fmt.Errorf("another SBM session is active")
	}
	return file, nil
}

func ownedByRoot(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func CheckRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByRoot(info) {
		return fmt.Errorf("state directory must be root-owned with mode 0700")
	}
	return nil
}

type intent struct {
	Profile string `json:"profile"`
}

func readIntent(root string) (string, error) {
	var state intent
	err := readJSON(filepath.Join(root, "desired.json"), &state)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil || (state.Profile != "" && !namePattern.MatchString(state.Profile)) {
		return "", fmt.Errorf("invalid saved connection state")
	}
	return state.Profile, nil
}

func OfflineStatus(root string) (Status, error) {
	profile, err := readIntent(root)
	return Status{State: "stopped", SavedProfile: profile}, err
}

// StopOffline clears saved intent only when no session owns the state.
func StopOffline(ctx context.Context, root string) (Status, error) {
	lock, err := lockSession(ctx, root)
	if err != nil {
		return Status{}, err
	}
	defer lock.Close()
	if err := writeJSON(filepath.Join(root, "desired.json"), intent{}); err != nil {
		return Status{}, fmt.Errorf("cannot save connection state")
	}
	return Status{State: "stopped"}, nil
}
