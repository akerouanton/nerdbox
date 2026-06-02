/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package task

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

type streamCreator interface {
	StartStream(ctx context.Context, streamID string) (net.Conn, error)
}

// generateStreamID creates a pseudo-random stream ID with the given prefix.
// The format is "{prefix}-{nanosecond}-{random}" to minimize collisions.
func generateStreamID(prefix string) string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), base64.RawURLEncoding.EncodeToString(b[:]))
}

// ioFSMState represents the lifecycle state of an ioForwarder.
type ioFSMState int

const (
	// ioStateForwarding: copy goroutines are running; streams are open and
	// must not be closed.
	ioStateForwarding ioFSMState = iota

	// ioStateDrained: all copy goroutines have returned; it is now safe to
	// close the underlying stream connections.
	ioStateDrained

	// ioStateClosed: streams have been closed; terminal state.
	ioStateClosed
)

func (s ioFSMState) String() string {
	switch s {
	case ioStateForwarding:
		return "Forwarding"
	case ioStateDrained:
		return "Drained"
	case ioStateClosed:
		return "Closed"
	default:
		return fmt.Sprintf("ioFSMState(%d)", int(s))
	}
}

// ioForwarder manages the host-side IO streams that bridge host FIFOs/files
// to the VM vsock streams.  It is a small explicit state machine with three
// states:
//
//	Forwarding --> Drained --> Closed
//	     |                     ^
//	     +-- ForceShutdown() --+
//	         (streams closed before full drain)
//
// Forwarding: copy goroutines are running; streams must remain open.
// Drained:    copy goroutines finished; streams may now be closed.
// Closed:     streams closed; terminal state.
//
// The only valid operation after construction is Shutdown, which drives the
// forwarder from Forwarding through Drained to Closed in order, enforcing
// the invariant that we never close a stream that a goroutine is still
// reading from.
type ioForwarder struct {
	mu      sync.Mutex
	state   ioFSMState
	streams [3]io.ReadWriteCloser // vsock connections; [0]=stdin [1]=stdout [2]=stderr
	drained chan struct{}         // closed by copy goroutines when all output is drained
}

// Shutdown drives the forwarder to Closed, waiting for copy goroutines to
// drain first.  It is safe to call from multiple goroutines; all concurrent
// callers block until the first call completes the shutdown, then all return.
// Subsequent calls after shutdown is complete return nil immediately.
//
// If ctx carries no deadline, a 30-second timeout is applied so a wedged
// guest cannot pin cleanup indefinitely.  ctx.Done() is only reached in
// exceptional cases (VM crash, vminitd hang, kernel wedge); in normal
// operation the drained channel closes on its own when the container process
// exits and propagates EOF through the vsock connections.
func (f *ioForwarder) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch f.state {
	case ioStateForwarding:
		// Normal path: wait for drain, then close.
	case ioStateClosed:
		// Already shut down; idempotent.
		return nil
	default:
		return fmt.Errorf("ioForwarder.Shutdown: unexpected state %s", f.state)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	select {
	case <-f.drained:
		// Goroutines finished naturally; transition Forwarding → Drained → Closed.
		f.state = ioStateDrained
	case <-ctx.Done():
		// Forceful shutdown: transition Forwarding → Closed, skipping Drained.
	}

	// Transition to Closed: close all stream connections.
	for i, c := range f.streams {
		if c != nil && (i != 2 || c != f.streams[1]) {
			c.Close()
		}
	}
	f.state = ioStateClosed

	return nil
}

// ForceShutdown closes the stream connections immediately without waiting for
// copy goroutines to drain.  Use this when cleanup must be immediate and data
// loss is acceptable.  For graceful shutdown, use Shutdown.
func (f *ioForwarder) ForceShutdown() error {
	// Generate a canceled context to signal to Shutdown to skip the drain phase.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return f.Shutdown(ctx)
}

func (s *service) forwardIO(ctx context.Context, ss streamCreator, idPrefix string, sio stdio.Stdio) (_ stdio.Stdio, _ *ioForwarder, retErr error) {
	pio := sio
	if pio.IsNull() {
		return pio, nil, nil
	}
	u, err := url.Parse(pio.Stdout)
	if err != nil {
		return stdio.Stdio{}, nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	var streams [3]io.ReadWriteCloser
	switch u.Scheme {
	case "stream":
		// Pass through
		return pio, nil, nil
	case "fifo", "pipe":
		pio, streams, err = createStreams(ctx, ss, idPrefix, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	case "file":
		filePath := u.Path
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return stdio.Stdio{}, nil, err
		}
		var f *os.File
		f, err = os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
		f.Close()
		pio.Stdout = filePath
		pio.Stderr = filePath
		pio, streams, err = createStreams(ctx, ss, idPrefix, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	default:
		// TODO: Support "binary"
		return stdio.Stdio{}, nil, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}
	if err != nil {
		return stdio.Stdio{}, nil, err
	}

	fwd := &ioForwarder{
		state:   ioStateForwarding,
		streams: streams,
		drained: make(chan struct{}),
	}

	defer func() {
		if err != nil {
			if err := fwd.ForceShutdown(); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"error":    err,
					"orig_err": retErr,
				}).Error("failed to force shutdown io forwarder")
			}
		}
	}()

	if err = copyStreams(ctx, streams, sio.Stdin, sio.Stdout, sio.Stderr, fwd.drained); err != nil {
		return stdio.Stdio{}, nil, err
	}
	return pio, fwd, nil
}

func createStreams(ctx context.Context, ss streamCreator, idPrefix string, io stdio.Stdio) (_ stdio.Stdio, conns [3]io.ReadWriteCloser, err error) {
	defer func() {
		if err != nil {
			for i, c := range conns {
				if c != nil && (i != 2 || c != conns[1]) {
					c.Close()
				}
			}
		}
	}()
	if io.Stdin != "" {
		sid := generateStreamID(idPrefix + "-stdin")
		conn, err := ss.StartStream(ctx, sid)
		if err != nil {
			return io, conns, fmt.Errorf("failed to start stdin stream: %w", err)
		}
		io.Stdin = fmt.Sprintf("stream://%s", sid)
		conns[0] = conn
	}

	stdout := io.Stdout
	if stdout != "" {
		sid := generateStreamID(idPrefix + "-stdout")
		conn, err := ss.StartStream(ctx, sid)
		if err != nil {
			return io, conns, fmt.Errorf("failed to start stdout stream: %w", err)
		}
		io.Stdout = fmt.Sprintf("stream://%s", sid)
		conns[1] = conn
	}

	if io.Stderr != "" {
		if io.Stderr == stdout {
			io.Stderr = io.Stdout
			conns[2] = conns[1]
		} else {
			sid := generateStreamID(idPrefix + "-stderr")
			conn, err := ss.StartStream(ctx, sid)
			if err != nil {
				return io, conns, fmt.Errorf("failed to start stderr stream: %w", err)
			}
			io.Stderr = fmt.Sprintf("stream://%s", sid)
			conns[2] = conn
		}
	}
	return io, conns, nil
}

var bufPool = sync.Pool{
	New: func() interface{} {
		// setting to 4096 to align with PIPE_BUF
		// http://man7.org/linux/man-pages/man7/pipe.7.html
		buffer := make([]byte, 4096)
		return &buffer
	},
}

// countingWriteCloser masks io.Closer() until close has been invoked a certain number of times.
type countingWriteCloser struct {
	io.WriteCloser
	count atomic.Int64
}

func newCountingWriteCloser(c io.WriteCloser, count int64) *countingWriteCloser {
	cwc := &countingWriteCloser{
		c,
		atomic.Int64{},
	}
	cwc.bumpCount(count)
	return cwc
}

func (c *countingWriteCloser) bumpCount(delta int64) int64 {
	return c.count.Add(delta)
}

func (c *countingWriteCloser) Close() error {
	if c.bumpCount(-1) > 0 {
		return nil
	}
	return c.WriteCloser.Close()
}
