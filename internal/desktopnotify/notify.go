// Package desktopnotify delivers best-effort alerts to the macOS console user.
package desktopnotify

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const script = `on run argv
    display notification (item 2 of argv) with title (item 1 of argv)
end run`

const deliveryTimeout = 3 * time.Second

type message struct {
	uid  int
	body string
}

type Notifier struct {
	queue      chan message
	done       chan struct{}
	cancel     context.CancelFunc
	consoleUID func() int
}

// New starts an independent delivery worker. Network operations never wait for it.
func New(title string) *Notifier {
	return newNotifier(consoleUID, func(ctx context.Context, uid int, body string) {
		deliver(ctx, uid, title, body)
	})
}

func newNotifier(user func() int, send func(context.Context, int, string)) *Notifier {
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{
		queue:      make(chan message, 8),
		done:       make(chan struct{}),
		cancel:     cancel,
		consoleUID: user,
	}
	go func() {
		defer close(n.done)
		for m := range n.queue {
			if ctx.Err() != nil {
				return
			}
			if n.consoleUID() != m.uid {
				continue
			}
			delivery, finish := context.WithTimeout(ctx, deliveryTimeout)
			send(delivery, m.uid, m.body)
			finish()
		}
	}()
	return n
}

// Notify drops new alerts when there is no console user or the queue is full.
func (n *Notifier) Notify(body string) {
	uid := n.consoleUID()
	if uid <= 0 {
		return
	}
	select {
	case n.queue <- message{uid: uid, body: body}:
	default:
	}
}

// Close allows a foreground process to deliver its final alert before exiting.
// The caller has already stopped all producers.
func (n *Notifier) Close() {
	close(n.queue)
	defer n.cancel()
	timer := time.NewTimer(deliveryTimeout)
	defer timer.Stop()
	select {
	case <-n.done:
	case <-timer.C:
	}
}

func consoleUID() int {
	if runtime.GOOS != "darwin" {
		return -1
	}
	info, err := os.Stat("/dev/console")
	if err != nil {
		return -1
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func deliver(ctx context.Context, uid int, title, body string) {
	id := strconv.Itoa(uid)
	if command(ctx, "/bin/launchctl", "print", "gui/"+id).Run() != nil || consoleUID() != uid {
		return
	}
	// asuser enters the GUI bootstrap context without dropping root credentials.
	// sudo drops them before invoking the script.
	_ = command(ctx, "/bin/launchctl", "asuser", id, "/usr/bin/sudo", "-n", "-H",
		"-u", "#"+id, "--", "/usr/bin/osascript", "-e", script, title, body).Run()
}

func command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	return cmd
}
