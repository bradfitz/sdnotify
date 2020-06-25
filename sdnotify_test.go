package sdnotify_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/sdnotify"
)

func TestNotifierNotExist(t *testing.T) {
	if s := os.Getenv(sdnotify.Socket); s != "" {
		t.Skipf("skipping, notify socket set to %q", s)
	}

	n, err := sdnotify.New()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected is not exist, but got: %v", err)
	}

	// None of these operations should error or panic even though the Notifier
	// is nil.
	if err := n.Notify("noop"); err != nil {
		t.Fatalf("failed to noop notify: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("failed to noop close: %v", err)
	}
}

func TestNotifierEcho(t *testing.T) {
	tests := []struct {
		name string
		ss   []string
	}{
		{
			name: "ready",
			ss:   []string{sdnotify.Ready},
		},
		{
			name: "status stopping",
			ss: []string{
				sdnotify.Statusf("stopping in %s", 5*time.Second),
				sdnotify.Stopping,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Open a local listener which will receive messages from the
			// Notifier.
			pc, err := net.ListenPacket("unixgram", "")
			if err != nil {
				t.Fatalf("failed to listen: %v", err)
			}
			defer pc.Close()

			if err := pc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("failed to set deadline: %v", err)
			}

			// Echo back any received messages.
			strC := make(chan string)
			go func() {
				b := make([]byte, 128)
				n, _, err := pc.ReadFrom(b)
				if err != nil {
					panicf("failed to read: %v", err)
				}

				strC <- string(b[:n])
			}()

			// Send a notification to the client and expect the same back after
			// splitting on newlines added automatically by Notify.
			n, err := sdnotify.Open(pc.LocalAddr().String())
			if err != nil {
				t.Fatalf("failed to open: %v", err)
			}
			defer n.Close()

			if err := n.Notify(tt.ss...); err != nil {
				t.Fatalf("failed to notify: %v", err)
			}

			if diff := cmp.Diff(tt.ss, strings.Split(<-strC, "\n")); diff != "" {
				t.Fatalf("unexpected notification (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNotifierIntegration(t *testing.T) {
	// Use a test binary in a fixed position and skip if unavailable.
	const bin = "./sdnotifytest"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("skipping, cannot stat: %v", err)
	}

	// Find a file suitable for listening on a socket and clean it up
	// immediately and after test execution.
	f, err := ioutil.TempFile("", "sdnotifytest")
	if err != nil {
		t.Fatalf("failed to create temporary file: %v", err)
	}
	_ = f.Close()

	remove := func() {
		if err := os.RemoveAll(f.Name()); err != nil {
			t.Fatalf("failed to remove temporary file: %v", err)
		}
	}
	remove()
	defer remove()

	pc, err := net.ListenPacket("unixgram", f.Name())
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer pc.Close()

	if err := pc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("failed to set deadline: %v", err)
	}

	// Batch up all received notification messages and send them back to the
	// main goroutine when the command finishes.
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()

	notifC := make(chan string)
	go func() {
		defer wg.Done()

		var buf bytes.Buffer
		b := make([]byte, 128)
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				if strings.Contains(err.Error(), "use of closed") {
					break
				}

				panicf("failed to read: %v", err)
			}

			_, _ = buf.Write(b[:n])
		}

		notifC <- buf.String()
	}()

	// Now that we've created a unixgram listener, invoke the test command with
	// NOTIFY_SOCKET set in its environment.
	cmd := exec.Command(bin)
	cmd.Env = []string{sdnotify.Socket + "=" + f.Name()}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to run command: %v\nout:\n%s", err, string(b))
	}
	_ = pc.Close()

	// Only messages sent in the same batch are newline delimited.
	const want = `STATUS=waiting 0STATUS=waiting 1STATUS=waiting 2READY=1
STATUS=done
STOPPING=1`

	if diff := cmp.Diff(want, <-notifC); diff != "" {
		t.Fatalf("unexpected notifications (-want +got):\n%s", diff)
	}
}

func panicf(format string, a ...interface{}) {
	panic(fmt.Sprintf(format, a...))
}
