// Package mux splits one listener between the gRPC server and the HTTP server,
// so the landing page, /inspect and /stats answer on the same port the nodes
// use. Someone who finds the address in a firewall log and opens it in a
// browser gets a page explaining it, not a protocol error.
package mux

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"
)

// detectTimeout is how long a new connection has to reveal its protocol:
// finish the TLS handshake, or send its first bytes. A client that connects
// and stays silent would otherwise hold a goroutine and a socket for ever.
const detectTimeout = 10 * time.Second

// h2Preface is what every HTTP/2 client with prior knowledge sends first. gRPC
// over plaintext, and nginx's grpc_pass, always start with it; HTTP/1.1 never
// can, since no method is spelled "PRI".
var h2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// Protocol is where a connection was routed.
type Protocol int

const (
	ProtocolGRPC Protocol = iota
	ProtocolHTTP
)

func (p Protocol) String() string {
	if p == ProtocolGRPC {
		return "grpc"
	}
	return "http"
}

// Mux owns the real listener and feeds two virtual ones.
type Mux struct {
	inner   net.Listener
	tls     *tls.Config
	metrics *Metrics

	grpc *listener
	http *listener
}

// New wraps inner. With a TLS config the mux terminates TLS itself, and both
// servers receive decrypted connections; without one it routes plaintext.
// metrics may be nil.
func New(inner net.Listener, tlsConfig *tls.Config, metrics *Metrics) *Mux {
	m := &Mux{
		inner:   inner,
		metrics: metrics,
		grpc:    newListener(inner.Addr()),
		http:    newListener(inner.Addr()),
	}
	if tlsConfig != nil {
		m.tls = alpnConfig(tlsConfig)
	}
	return m
}

// GRPCListener is what the gRPC server serves.
func (m *Mux) GRPCListener() net.Listener { return m.grpc }

// HTTPListener is what the HTTP server serves.
func (m *Mux) HTTPListener() net.Listener { return m.http }

// Serve accepts until ctx is done or the listener fails, then closes both
// virtual listeners so the servers on them return.
func (m *Mux) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = m.inner.Close()
	}()
	defer func() { _ = m.grpc.Close() }()
	defer func() { _ = m.http.Close() }()

	var backoff time.Duration
	for {
		conn, err := m.inner.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Running out of file descriptors is the usual cause. Back off
			// rather than spin, and keep serving the connections already open.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			slog.Warn("accept failed", "error", err, "retry_in", backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		go m.route(conn)
	}
}

func (m *Mux) route(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(detectTimeout))

	protocol, routed, err := m.detect(conn)
	if err != nil {
		slog.Debug("connection rejected", "remote", conn.RemoteAddr().String(), "error", err)
		m.metrics.rejected()
		_ = conn.Close()
		return
	}
	_ = routed.SetDeadline(time.Time{})

	target := m.grpc
	if protocol == ProtocolHTTP {
		target = m.http
	}
	tracked := m.metrics.track(protocol, routed)
	if !target.push(tracked) {
		// Shutdown raced the detection; the listener closed the connection.
		m.metrics.rejected()
	}
}

func (m *Mux) detect(conn net.Conn) (Protocol, net.Conn, error) {
	if m.tls != nil {
		tc := tls.Server(conn, m.tls)
		ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
		defer cancel()
		if err := tc.HandshakeContext(ctx); err != nil {
			return 0, nil, err
		}
		if tc.ConnectionState().NegotiatedProtocol == "h2" {
			return ProtocolGRPC, tc, nil
		}
		return ProtocolHTTP, tc, nil
	}

	// Read until the bytes stop matching the preface or the whole preface
	// is in. An HTTP/1.1 request diverges at its first byte.
	buf := make([]byte, len(h2Preface))
	n := 0
	for n < len(buf) {
		k, err := conn.Read(buf[n:])
		n += k
		if !bytes.Equal(buf[:n], h2Preface[:n]) {
			return ProtocolHTTP, &prefixConn{Conn: conn, prefix: buf[:n]}, nil
		}
		if err != nil {
			return 0, nil, err
		}
	}
	return ProtocolGRPC, &prefixConn{Conn: conn, prefix: buf}, nil
}

// alpnConfig routes TLS clients by what they offer. grpc-go — and so Talos —
// offers only "h2". Browsers and curl offer "http/1.1" as well and are given
// HTTP/1.1, which is all the pages need; giving them h2 would land them on the
// gRPC server, which answers anything that is not gRPC with an error.
func alpnConfig(config *tls.Config) *tls.Config {
	h2 := config.Clone()
	h2.NextProtos = []string{"h2"}
	http1 := config.Clone()
	http1.NextProtos = []string{"http/1.1"}

	out := config.Clone()
	out.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if slices.Contains(hello.SupportedProtos, "http/1.1") {
			return http1, nil
		}
		return h2, nil
	}
	return out
}

// prefixConn replays the bytes read during detection.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// listener is a net.Listener fed by the mux.
type listener struct {
	addr      net.Addr
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newListener(addr net.Addr) *listener {
	return &listener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
}

// push hands conn to the server, or closes it if the listener is closed.
func (l *listener) push(conn net.Conn) bool {
	select {
	case l.conns <- conn:
		return true
	case <-l.done:
		_ = conn.Close()
		return false
	}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *listener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *listener) Addr() net.Addr { return l.addr }
