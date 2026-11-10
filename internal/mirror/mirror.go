package mirror

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nrf24l01/proxy_fallback/internal/config"
	"github.com/nrf24l01/proxy_fallback/internal/fixture"
	"github.com/nrf24l01/proxy_fallback/internal/proxyproto"
)

// dynamicHeaders are per-request values in the real service; replaying the
// recorded one would make every response identical (an obvious fingerprint).
var dynamicHeaders = map[string]bool{
	"x-request-id": true, "x-trace-id": true, "x-correlation-id": true,
	"request-id": true, "traceparent": true, "x-amzn-requestid": true,
	"x-ratelimit-reset": true,
}

var (
	// CSP nonces: real services mint a fresh one per response, so replaying a
	// frozen nonce in both header and body is itself a fingerprint.
	reNonce = regexp.MustCompile(`nonce-([A-Za-z0-9+/=]{16,})`)
	reUUID  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHex32 = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	reTrace = regexp.MustCompile(`^00-([0-9a-fA-F]{32})-([0-9a-fA-F]{16})-([0-9a-fA-F]{2})$`)
)

// refreshDynamic returns a freshly generated value shaped like the recorded one.
type rotatedBody struct {
	data     []byte
	encoding string
}

// rotateNonces mints a new CSP nonce, substitutes it in every header value that
// carried the old one and in the body, keeping the body's encoding intact.
func rotateNonces(h http.Header, data []byte, encoding string) (rotatedBody, bool) {
	old := map[string]string{}
	for k, vs := range h {
		for i, v := range vs {
			if !strings.Contains(strings.ToLower(k), "content-security-policy") {
				continue
			}
			nv := reNonce.ReplaceAllStringFunc(v, func(m string) string {
				token := m[len("nonce-"):]
				if fresh, ok := old[token]; ok {
					return "nonce-" + fresh
				}
				fresh := randomToken(len(token))
				old[token] = fresh
				return "nonce-" + fresh
			})
			h[k][i] = nv
		}
	}
	if len(old) == 0 {
		return rotatedBody{data, encoding}, false
	}

	body := data
	gzipped := false
	if encoding == "gzip" {
		if plain, err := gunzip(data); err == nil {
			body, gzipped = plain, true
		}
	}
	for from, to := range old {
		body = bytes.ReplaceAll(body, []byte(from), []byte(to))
	}
	if gzipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			return rotatedBody{data, encoding}, false
		}
		if err := zw.Close(); err != nil {
			return rotatedBody{data, encoding}, false
		}
		return rotatedBody{buf.Bytes(), "gzip"}, true
	}
	return rotatedBody{body, encoding}, true
}

// randomToken returns a fresh token of the requested length using the same
// alphabet CSP nonces use.
func randomToken(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("A", n)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func refreshDynamic(name, recorded string) string {
	if !dynamicHeaders[strings.ToLower(name)] {
		return recorded
	}
	switch {
	case reTrace.MatchString(recorded):
		m := reTrace.FindStringSubmatch(recorded)
		return "00-" + randHex(16) + "-" + randHex(8) + "-" + m[3]
	case reUUID.MatchString(recorded):
		upper := strings.ToUpper(recorded) == recorded
		v := randHex(4) + "-" + randHex(2) + "-" + randHex(2) + "-" + randHex(2) + "-" + randHex(6)
		if upper {
			v = strings.ToUpper(v)
		}
		return v
	case reHex32.MatchString(recorded):
		h := randHex(16)
		if strings.ToUpper(recorded) == recorded {
			h = strings.ToUpper(h)
		}
		return h
	}
	return recorded
}

// hopHeaders are never replayed from a recording.
var hopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"date":                true,
	"content-length":      true,
	"alt-svc":             true,
}

// Front serves one recorded service on its own port.
type Front struct {
	svc   config.Service
	man   *fixture.Manifest
	byKey map[string]*fixture.Entry
	log   *log.Logger
}

func Load(svc config.Service, logger *log.Logger) (*Front, error) {
	man, byKey, err := fixture.Load(svc.FixtureDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", svc.Name, err)
	}
	if man.Service != "" && man.Service != svc.Name {
		return nil, fmt.Errorf("%s: fixture belongs to %q", svc.Name, man.Service)
	}
	for _, e := range byKey {
		if strings.EqualFold(e.ContentEncoding, "gzip") {
			id, err := gunzip(e.Data)
			if err == nil {
				e.Identity = id
			}
		}
	}
	return &Front{svc: svc, man: man, byKey: byKey, log: logger}, nil
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status, size := f.serve(w, r)
	if f.log != nil {
		f.log.Printf("[serve] %-9s :%d %s %s%s -> %d %dB %s", f.svc.Name, f.svc.Port,
			r.Method, r.URL.Path, queryPart(r.URL.RawQuery), status, size,
			time.Since(start).Round(time.Millisecond))
	}
}

func queryPart(q string) string {
	if q == "" {
		return ""
	}
	return "?" + q
}

func (f *Front) serve(w http.ResponseWriter, r *http.Request) (int, int) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead &&
		r.Method != http.MethodPost && r.Method != http.MethodPut &&
		r.Method != http.MethodPatch && r.Method != http.MethodDelete &&
		r.Method != http.MethodOptions {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return http.StatusMethodNotAllowed, 0
	}

	entry := f.lookup(r)
	if entry == nil {
		return f.notFound(w, r)
	}

	if r.Method == http.MethodOptions {
		// Advertise the methods the real front would; no body.
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent, 0
	}

	data := entry.Data
	encoding := strings.ToLower(strings.TrimSpace(entry.ContentEncoding))
	if encoding == "gzip" && !acceptsGzip(r) {
		if entry.Identity == nil {
			id, err := gunzip(entry.Data)
			if err != nil {
				return f.notFound(w, r)
			}
			entry.Identity = id
		}
		data = entry.Identity
		encoding = ""
	}

	h := w.Header()
	etag := entry.Header("ETag")
	for _, kv := range entry.Headers {
		name := strings.ToLower(kv.Name)
		if hopHeaders[name] {
			continue
		}
		if name == "content-encoding" && encoding == "" {
			continue
		}
		if name == "set-cookie" && !f.svc.KeepSetCookie {
			continue
		}
		if name == "expires" && entry.ExpiresIn != 0 {
			continue // regenerated below from the recorded TTL
		}
		if dynamicHeaders[name] {
			h.Set(kv.Name, refreshDynamic(name, kv.Value))
			continue
		}
		h.Add(kv.Name, kv.Value)
	}
	if entry.ExpiresIn != 0 {
		when := time.Now().UTC().Add(time.Duration(entry.ExpiresIn) * time.Second)
		if entry.ExpiresLayout == "z" {
			h.Set("Expires", when.Format(time.RFC1123Z))
		} else {
			h.Set("Expires", when.Format(http.TimeFormat))
		}
	}
	if v := h.Get("X-Ratelimit-Reset"); v != "" {
		h.Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Unix()+60, 10))
	}
	// Fresh CSP nonce per response, rewritten in both the header and the body so
	// the page stays self-consistent.
	if rotated, ok := rotateNonces(h, data, encoding); ok {
		data, encoding = rotated.data, rotated.encoding
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" && etag != "" && etagMatches(inm, etag) {
		h.Del("Content-Type")
		h.Del("Content-Encoding")
		h.Del("Content-Length")
		w.WriteHeader(http.StatusNotModified)
		return http.StatusNotModified, 0
	}

	status := entry.Status
	if status == 0 {
		status = http.StatusOK
	}
	if entry.Chunked && r.Method != http.MethodHead {
		// Replay the upstream framing: no Content-Length, chunked stream.
		w.WriteHeader(status)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		n, _ := w.Write(data)
		return status, n
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return status, 0
	}
	n, _ := w.Write(data)
	return status, n
}

func (f *Front) lookup(r *http.Request) *fixture.Entry {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	// Non-GET requests (login attempts) have their own recorded responses, keyed
	// per content type where the real service distinguishes them (400 vs 415).
	if m := strings.ToUpper(r.Method); m != http.MethodGet && m != http.MethodHead {
		ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
		bases := []string{m + " " + p}
		if r.URL.RawQuery != "" {
			bases = append([]string{m + " " + p + "?" + r.URL.RawQuery}, bases...)
		}
		if trimmed := strings.TrimSuffix(p, "/"); trimmed != "" && trimmed != p {
			bases = append(bases, m+" "+trimmed)
		}
		for _, base := range bases {
			if ct != "" {
				if e, ok := f.byKey[base+" :: "+ct]; ok {
					return e
				}
			}
			if e, ok := f.byKey[base]; ok {
				return e
			}
		}
		// Content type not recorded separately: use the first variant for this path.
		for _, base := range bases {
			prefix := base + " :: "
			for k, e := range f.byKey {
				if strings.HasPrefix(k, prefix) {
					return e
				}
			}
		}
	}
	if r.URL.RawQuery != "" {
		if e, ok := f.byKey[p+"?"+r.URL.RawQuery]; ok {
			return e
		}
	}
	if e, ok := f.byKey[p]; ok {
		return e
	}
	// Tolerate a trailing slash difference (/login vs /login/).
	trimmed := strings.TrimSuffix(p, "/")
	if trimmed != "" && trimmed != p {
		if e, ok := f.byKey[trimmed]; ok {
			return e
		}
	}
	return nil
}

func (f *Front) notFound(w http.ResponseWriter, r *http.Request) (int, int) {
	if f.svc.NotFound != "plain" {
		if e, ok := f.byKey["__404__"]; ok {
			h := w.Header()
			for _, kv := range e.Headers {
				name := strings.ToLower(kv.Name)
				if hopHeaders[name] {
					continue
				}
				if name == "set-cookie" && !f.svc.KeepSetCookie {
					continue
				}
				h.Add(kv.Name, kv.Value)
			}
			// Replay the recorded response verbatim: many services answer unknown
			// paths with a redirect to their login page rather than a 404.
			data := e.Data
			if strings.EqualFold(e.ContentEncoding, "gzip") && !acceptsGzip(r) && e.Identity != nil {
				data = e.Identity
				h.Del("Content-Encoding")
			}
			status := e.Status
			if status == 0 {
				status = http.StatusNotFound
			}
			h.Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(status)
			if r.Method != http.MethodHead {
				w.Write(data)
			}
			return status, len(data)
		}
	}
	body := []byte("404 page not found\n")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
	return http.StatusNotFound, len(body)
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.Split(part, ";")[0]) == "gzip" {
			return true
		}
	}
	return false
}

func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || p == etag || strings.TrimPrefix(p, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Run starts one listener per service and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, services []config.Service, logger *log.Logger) error {
	if len(services) == 0 {
		return fmt.Errorf("no services selected")
	}
	var servers []*http.Server
	var wg sync.WaitGroup
	errCh := make(chan error, len(services))

	for _, svc := range services {
		front, err := Load(svc, logger)
		if err != nil {
			return err
		}
		addr := net.JoinHostPort(cfg.Listen, strconv.Itoa(svc.Port))
		srv := &http.Server{
			Addr:              addr,
			Handler:           front,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
			ErrorLog:          logger,
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("%s: listen %s: %w", svc.Name, addr, err)
		}
		// Some reverse proxies prepend a PROXY v1 line; strip it so the
		// request line is parsed correctly either way.
		ln = proxyproto.New(ln)
		logger.Printf("[serve] %-9s %-15s %d entries (%s), host=%q",
			svc.Name, addr, len(front.man.Entries), front.man.RecordedAt.Format(time.RFC3339),
			front.man.PublicURL)
		servers = append(servers, srv)
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Serve(ln); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}(srv)
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		for _, s := range servers {
			_ = s.Close()
		}
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	wg.Wait()
	logger.Printf("[serve] stopped %d listeners", len(servers))
	return nil
}
