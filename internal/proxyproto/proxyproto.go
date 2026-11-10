// Package proxyproto transparently strips a PROXY protocol v1 header from the
// start of a connection. Reverse proxies and load balancers prepend it so the
// destination can learn the real client address; a server that ignores it would
// answer 400 to every request.
package proxyproto

import (
	"bufio"
	"net"
	"strings"
	"time"
)

// Listener wraps a net.Listener and strips PROXY v1 headers when present.
type Listener struct {
	inner net.Listener
	// PeekTimeout bounds how long we wait for the first bytes. HTTP clients
	// always speak first, so this only guards half-open connections.
	PeekTimeout time.Duration
}

func New(inner net.Listener) *Listener {
	return &Listener{inner: inner, PeekTimeout: 3 * time.Second}
}

func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.inner.Accept()
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(c, 1024)
	_ = c.SetReadDeadline(time.Now().Add(l.PeekTimeout))
	head, perr := br.Peek(6)
	_ = c.SetReadDeadline(time.Time{})
	if perr == nil && strings.EqualFold(string(head), "PROXY ") {
		line, err := br.ReadString('\n')
		if err != nil {
			c.Close()
			return nil, err
		}
		parse(line) // best effort: we only need to consume it
	}
	return &conn{Conn: c, r: br}, nil
}

func (l *Listener) Close() error   { return l.inner.Close() }
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

type conn struct {
	net.Conn
	r *bufio.Reader
}

func (c *conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// parse validates a PROXY v1 line; the result is informational only.
func parse(line string) (src, dst string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "PROXY" {
		return "", "", false
	}
	if fields[1] == "UNKNOWN" {
		return "", "", true
	}
	if len(fields) != 6 {
		return "", "", false
	}
	return fields[2] + ":" + fields[4], fields[3] + ":" + fields[5], true
}
