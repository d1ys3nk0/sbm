package controller

import (
	"errors"
	"os"
	"testing"
	"time"
)

type fakeOwnedChild struct {
	termErr, killErr error
	onTerm, onKill   func()
	terms, kills     int
}

func (p *fakeOwnedChild) Signal(os.Signal) error {
	p.terms++
	if p.onTerm != nil {
		p.onTerm()
	}
	return p.termErr
}
func (p *fakeOwnedChild) Kill() error {
	p.kills++
	if p.onKill != nil {
		p.onKill()
	}
	return p.killErr
}

func TestStopOwnedChildBoundsWaitAfterKill(t *testing.T) {
	child := &fakeOwnedChild{}
	done := make(chan error)
	result := make(chan error, 1)
	go func() { result <- stopOwnedChild(child, done, time.Millisecond, 5*time.Millisecond) }()
	select {
	case err := <-result:
		if err == nil || child.terms != 1 || child.kills != 1 {
			t.Fatalf("err=%v terms=%d kills=%d", err, child.terms, child.kills)
		}
	case <-time.After(time.Second):
		t.Fatal("unconfirmed child blocked shutdown")
	}
}

func TestStopOwnedChildConfirmsExit(t *testing.T) {
	for _, forced := range []bool{false, true} {
		done := make(chan error)
		child := &fakeOwnedChild{}
		if forced {
			child.termErr = errors.New("term denied")
			child.onKill = func() { close(done) }
		} else {
			child.onTerm = func() { close(done) }
		}
		if err := stopOwnedChild(child, done, time.Millisecond, 5*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if (child.kills == 1) != forced {
			t.Fatalf("forced=%v kills=%d", forced, child.kills)
		}
	}
}
