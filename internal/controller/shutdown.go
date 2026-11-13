package controller

import (
	"errors"
	"os"
	"syscall"
	"time"
)

type ownedChild interface {
	Signal(os.Signal) error
	Kill() error
}

func awaitChild(done <-chan error, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func stopOwnedChild(child ownedChild, done <-chan error, grace, forced time.Duration) error {
	select {
	case <-done:
		return nil
	default:
	}
	var signalErr error
	if err := child.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		signalErr = err
	} else if awaitChild(done, grace) {
		return nil
	}
	if err := child.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		signalErr = errors.Join(signalErr, err)
	}
	if awaitChild(done, forced) {
		return nil
	}
	return errors.Join(errors.New("sing-box child exit was not confirmed"), signalErr)
}
