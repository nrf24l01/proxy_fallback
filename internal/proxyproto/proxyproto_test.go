package proxyproto

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type fakeListener struct {
	conns chan net.Conn
	addr  net.Addr
}

func (f *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-f.conns
	if !ok {
		return nil, io.EOF
	}
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return f.addr }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "tcp" }
func (fakeAddr) String() string  { return "127.0.0.1:443" }

func servedBy(t *testing.T, payload string) string {
	t.Helper()
	l := &fakeListener{conns: make(chan net.Conn, 1), addr: fakeAddr{}}
	pl := New(l)
	pl.PeekTimeout = time.Second
	client, server := net.Pipe()
	l.conns <- server
	go func() {
		_, _ = client.Write([]byte(payload))
	}()
	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len("GET / HTTP/1.1\r\n\r\n"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

func TestStripsProxyHeader(t *testing.T) {
	got := servedBy(t, "PROXY TCP4 203.0.113.9 10.0.0.1 51234 443\r\nGET / HTTP/1.1\r\n\r\n")
	if !strings.HasPrefix(got, "GET / HTTP/1.1") {
		t.Fatalf("PROXY header not stripped: %q", got)
	}
}

func TestPassesThroughWithoutProxyHeader(t *testing.T) {
	got := servedBy(t, "GET / HTTP/1.1\r\n\r\n")
	if !strings.HasPrefix(got, "GET / HTTP/1.1") {
		t.Fatalf("request mangled: %q", got)
	}
}

func TestUnknownFamily(t *testing.T) {
	got := servedBy(t, "PROXY UNKNOWN\r\nGET / HTTP/1.1\r\n\r\n")
	if !strings.HasPrefix(got, "GET / HTTP/1.1") {
		t.Fatalf("UNKNOWN header not stripped: %q", got)
	}
}

func TestChunkedReadsAcrossBoundary(t *testing.T) {
	// Header arrives in two writes; Peek must still see the prefix.
	l := &fakeListener{conns: make(chan net.Conn, 1), addr: fakeAddr{}}
	pl := New(l)
	pl.PeekTimeout = time.Second
	client, server := net.Pipe()
	l.conns <- server
	go func() {
		_, _ = client.Write([]byte("PROXY "))
		time.Sleep(50 * time.Millisecond)
		_, _ = client.Write([]byte("TCP4 1.1.1.1 2.2.2.2 1 2\r\nGET / HTTP/1.1\r\n\r\n"))
	}()
	conn, err := pl.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len("GET / HTTP/1.1"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "GET / HTTP/1.1" {
		t.Fatalf("got %q", string(buf))
	}
}
