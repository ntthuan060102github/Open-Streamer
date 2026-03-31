// Package pull contains pull-mode Reader implementations.
// Each reader connects to a remote source and emits raw MPEG-TS chunks.
package pull

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const udpReadSize = 188 * 7 // 7 TS packets per read — typical UDP MTU alignment

// UDPReader reads raw MPEG-TS from a UDP unicast or multicast address.
// Expected URL format: udp://[group@]host:port
// Example:
//
//	udp://239.1.1.1:5000   — multicast group
//	udp://0.0.0.0:5000     — unicast, listen on all interfaces
type UDPReader struct {
	input domain.Input
	conn  net.PacketConn
	buf   []byte
}

// NewUDPReader constructs a UDPReader for the given input.
func NewUDPReader(input domain.Input) *UDPReader {
	return &UDPReader{
		input: input,
		buf:   make([]byte, udpReadSize),
	}
}

func (r *UDPReader) Open(ctx context.Context) error {
	u, err := url.Parse(r.input.URL)
	if err != nil {
		return fmt.Errorf("udp reader: parse url %q: %w", r.input.URL, err)
	}

	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(ctx, "udp", u.Host)
	if err != nil {
		return fmt.Errorf("udp reader: listen %q: %w", u.Host, err)
	}

	// Multicast: URL user-info carries the group address, e.g. udp://239.1.1.1@0.0.0.0:5000
	// Joining a multicast group requires the golang.org/x/net/ipv4 package which is
	// not a direct dependency. For now we accept the connection as-is; the OS will
	// route multicast traffic to the bound UDP socket if the kernel has the group
	// membership configured externally (e.g. via `ip maddr add`).
	// TODO: add ipv4.PacketConn.JoinGroup / ipv6.PacketConn.JoinGroup for auto-join.

	r.conn = pc
	return nil
}

// LocalAddr returns the local network address the connection is bound to.
// Useful for tests that need to know the OS-assigned port (udp://host:0).
func (r *UDPReader) LocalAddr() net.Addr {
	if r.conn == nil {
		return nil
	}
	return r.conn.LocalAddr()
}

func (r *UDPReader) Read(ctx context.Context) ([]byte, error) {
	// Propagate context cancellation by setting a near-immediate deadline when
	// the context is already done, or use the context deadline if one exists.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if deadline, ok := ctx.Deadline(); ok {
		r.conn.SetReadDeadline(deadline) //nolint:errcheck
	} else {
		// No deadline: clear any previously set deadline.
		r.conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	}

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		n, _, err := r.conn.ReadFrom(r.buf)
		if err != nil {
			ch <- result{err: err}
			return
		}
		out := make([]byte, n)
		copy(out, r.buf[:n])
		ch <- result{data: out}
	}()

	select {
	case <-ctx.Done():
		r.conn.SetReadDeadline(time.Now()) //nolint:errcheck // unblock ReadFrom
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("udp reader: read: %w", res.err)
		}
		return res.data, nil
	}
}

func (r *UDPReader) Close() error {
	if r.conn != nil {
		err := r.conn.Close()
		r.conn = nil
		return err
	}
	return nil
}
