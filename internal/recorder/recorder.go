package recorder

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/nrf24l01/proxy_fallback/internal/config"
	"github.com/nrf24l01/proxy_fallback/internal/fixture"
)

const maxBodyBytes = 32 << 20

var (
	// Only resource references — never <a href>, which would crawl whole sites.
	reLinkHref = regexp.MustCompile(`(?i)<link\b[^>]*\bhref\s*=\s*["']([^"']+)["']`)
	reSrcAttr  = regexp.MustCompile(`(?i)<(?:script|img|source|video|audio|embed|iframe|input)\b[^>]*\bsrc\s*=\s*["']([^"']+)["']`)
	reSrcSet   = regexp.MustCompile(`(?i)<(?:img|source)\b[^>]*\bsrcset\s*=\s*["']([^"']+)["']`)
	rePoster   = regexp.MustCompile(`(?i)\bposter\s*=\s*["']([^"']+)["']`)
	reInline   = regexp.MustCompile(`(?i)\bdata-(?:src|background)\s*=\s*["']([^"']+)["']`)
	reCSSURL   = regexp.MustCompile(`(?i)url\(\s*['"]?([^'")]+?)['"]?\s*\)`)
	reImport   = regexp.MustCompile(`(?i)@import\s+(?:url\()?\s*['"]([^'"]+)['"]`)
)

type item struct {
	u     *url.URL
	depth int
}

type Recorder struct {
	svc    config.Service
	log    *log.Logger
	client *http.Client
	base   *url.URL
	store  fixture.Store
	seen   map[string]bool
	man    fixture.Manifest
}

func New(svc config.Service, logger *log.Logger) (*Recorder, error) {
	svc = svc.Defaults()
	if strings.Contains(svc.UserAgent, "Go-http-client") {
		svc.UserAgent = config.DefaultUserAgent
	}
	if svc.Upstream == "" {
		return nil, fmt.Errorf("service %q: upstream is required for record mode", svc.Name)
	}
	base, err := url.Parse(strings.TrimRight(svc.Upstream, "/"))
	if err != nil {
		return nil, fmt.Errorf("service %q: bad upstream: %w", svc.Name, err)
	}
	tr := &http.Transport{
		DisableCompression:  true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Recorder{
		svc:   svc,
		log:   logger,
		base:  base,
		store: fixture.Store{Dir: svc.FixtureDir},
		seen:  map[string]bool{},
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// Run crawls the live service and writes fixtures/<service>/.
func (r *Recorder) Run(ctx context.Context) error {
	r.man = fixture.Manifest{
		Service:     r.svc.Name,
		Title:       r.svc.Title,
		Category:    r.svc.Category,
		LoginPath:   r.svc.LoginPath,
		App:         r.svc.App,
		DefaultPort: r.svc.DefaultPort,
		Upstream:    r.svc.Upstream,
		PublicURL:   r.svc.PublicURL,
		RecordedAt:  time.Now().UTC(),
	}

	queue := make([]item, 0, 64)
	for _, p := range r.svc.Pages {
		u, err := r.resolve(p)
		if err != nil {
			return fmt.Errorf("page %q: %w", p, err)
		}
		queue = append(queue, item{u: u, depth: 0})
	}
	// Extra seeds captured from a real browser (lazy JS chunks, XHR endpoints).
	if r.svc.ExtraURLsFile != "" {
		raw, err := os.ReadFile(r.svc.ExtraURLsFile)
		if err != nil {
			r.log.Printf("[record] %s extra_urls_file: %v", r.svc.Name, err)
		} else {
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				u, err := r.resolve(line)
				if err != nil {
					continue
				}
				queue = append(queue, item{u: u, depth: 0})
			}
		}
	}

	fetched := 0
	for len(queue) > 0 && fetched < r.svc.MaxAssets {
		if err := ctx.Err(); err != nil {
			return err
		}
		it := queue[0]
		queue = queue[1:]
		if it.depth > 3 {
			continue
		}
		key := keyOf(it.u)
		if r.seen[key] {
			continue
		}
		r.seen[key] = true

		resp, err := r.fetch(ctx, it.u)
		if err != nil {
			r.log.Printf("[record] %s %s: %v", r.svc.Name, key, err)
			continue
		}
		fetched++
		ctype := resp.header("Content-Type")
		storable := resp.status < 400 || it.depth == 0
		if storable && len(resp.body) > maxBodyBytes {
			storable = false
		}
		if storable {
			ent, err := r.save(key, it.u, resp)
			if err != nil {
				return err
			}
			r.man.Entries = append(r.man.Entries, ent)
			r.log.Printf("[record] %s %3d %7dB %s", r.svc.Name, resp.status, len(resp.body), key)
		}

		// Follow redirect targets so the mirror can replay them too.
		if resp.status >= 300 && resp.status < 400 {
			if loc := resp.header("Location"); loc != "" {
				if nu, err := it.u.Parse(loc); err == nil && r.sameOrigin(nu) {
					queue = append(queue, item{u: nu, depth: it.depth})
				}
			}
			continue
		}
		if resp.status != 200 {
			continue
		}

		enc := resp.header("Content-Encoding")
		switch {
		case isHTML(ctype, it.u.Path):
			for _, ref := range extractHTML(decodeBody(enc, resp.body)) {
				if nu, err := it.u.Parse(ref); err == nil && r.sameOrigin(nu) {
					queue = append(queue, item{u: nu, depth: it.depth + 1})
				}
			}
		case isCSS(ctype, it.u.Path):
			for _, ref := range extractCSS(decodeBody(enc, resp.body)) {
				if nu, err := it.u.Parse(ref); err == nil && r.sameOrigin(nu) {
					queue = append(queue, item{u: nu, depth: it.depth + 1})
				}
			}
		}
	}

	// Explicit API endpoints used by the landing page itself.
	for _, p := range r.svc.API {
		u, err := r.resolve(p)
		if err != nil {
			r.log.Printf("[record] %s api %q: %v", r.svc.Name, p, err)
			continue
		}
		if r.seen[keyOf(u)] {
			continue
		}
		r.seen[keyOf(u)] = true
		resp, err := r.fetch(ctx, u)
		if err != nil {
			r.log.Printf("[record] %s api %s: %v", r.svc.Name, p, err)
			continue
		}
		ent, err := r.save(keyOf(u), u, resp)
		if err != nil {
			return err
		}
		r.man.Entries = append(r.man.Entries, ent)
	}

	// Well-known files most services answer.
	for _, p := range []string{"/robots.txt", "/favicon.ico"} {
		u, _ := r.resolve(p)
		if u == nil || r.seen[keyOf(u)] {
			continue
		}
		r.seen[keyOf(u)] = true
		if resp, err := r.fetch(ctx, u); err == nil && resp.status < 400 {
			ent, err := r.save(keyOf(u), u, resp)
			if err != nil {
				return err
			}
			r.man.Entries = append(r.man.Entries, ent)
		}
	}

	// Login attempts with wrong credentials: record the real failure response.
	for _, spec := range r.svc.Posts {
		u, err := r.resolve(spec.Path)
		if err != nil {
			r.log.Printf("[record] %s post %q: %v", r.svc.Name, spec.Path, err)
			continue
		}
		resp, err := r.fetchMethod(ctx, http.MethodPost, u, spec.ContentType, spec.Body)
		if err != nil {
			r.log.Printf("[record] %s post %s: %v", r.svc.Name, spec.Path, err)
			continue
		}
		postKey := "POST " + keyOf(u)
		if ct := strings.ToLower(strings.TrimSpace(spec.ContentType)); ct != "" {
			postKey += " :: " + ct
		}
		ent, err := r.save(postKey, u, resp)
		if err != nil {
			return err
		}
		ent.Method = http.MethodPost
		ent.ReqType = spec.ContentType
		ent.ReqBody = spec.Body
		r.man.Entries = append(r.man.Entries, ent)
		r.log.Printf("[record] %s POST %s -> %d (%dB)", r.svc.Name, keyOf(u), resp.status, len(resp.body))
	}

	// A genuine 404 body, replayed for every unknown path.
	if r.svc.RecordNotFound {
		u, _ := r.resolve("/srvimit-404-sample-" + randHex(6))
		resp, err := r.fetch(ctx, u)
		if err == nil {
			ent, err := r.save("__404__", u, resp)
			if err != nil {
				return err
			}
			r.man.Entries = append(r.man.Entries, ent)
		}
	}

	if err := r.man.Save(r.svc.FixtureDir); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	r.log.Printf("[record] %s: %d entries -> %s", r.svc.Name, len(r.man.Entries), r.svc.FixtureDir)
	return nil
}

type response struct {
	status  int
	headers []fixture.Header
	body    []byte
	chunked bool // resp.Header hides Transfer-Encoding; net/http exposes it separately
}

func (r *response) header(name string) string {
	for _, h := range r.headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func (r *Recorder) fetch(ctx context.Context, u *url.URL) (*response, error) {
	return r.fetchMethod(ctx, http.MethodGet, u, "", "")
}

func (r *Recorder) fetchMethod(ctx context.Context, method string, u *url.URL, contentType, reqBody string) (*response, error) {
	var rdr io.Reader
	if reqBody != "" {
		rdr = strings.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	if host := r.svc.PublicHost(); host != "" {
		req.Host = host
	}
	req.Header.Set("User-Agent", r.svc.UserAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if method != http.MethodGet {
		// Plain HTTP clients (curl, scripts) send a wildcard Accept; recording
		// with it captures the error representation a client actually gets.
		req.Header.Set("Accept", "*/*")
	} else {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	}
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	out := &response{status: resp.StatusCode, body: body}
	for _, te := range resp.TransferEncoding {
		if strings.EqualFold(te, "chunked") {
			out.chunked = true
		}
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			out.headers = append(out.headers, fixture.Header{Name: k, Value: v})
		}
	}
	return out, nil
}

func (r *Recorder) save(key string, u *url.URL, resp *response) (fixture.Entry, error) {
	enc := ""
	for _, h := range resp.headers {
		if strings.EqualFold(h.Name, "Content-Encoding") {
			enc = strings.ToLower(strings.TrimSpace(h.Value))
		}
	}
	chunked := resp.chunked
	// Cache lifetime relative to the response Date, so it can be replayed as a
	// live value instead of a timestamp frozen at record time.
	var expiresIn int64
	expiresLayout := ""
	if exp := resp.header("Expires"); exp != "" {
		if d, dok := parseHTTPDate(resp.header("Date")); dok {
			if e, eok := parseHTTPDate(exp); eok {
				expiresIn = int64(e.Sub(d).Seconds())
				expiresLayout = dateStyle(exp)
			}
		}
	}
	body := resp.body
	// Rewriting only makes sense on decoded text; compressed bodies are stored as-is.
	if r.svc.RewriteBody && enc == "" {
		body = applyRewrites(body, r.svc.Rewrite, resp.header("Content-Type"))
	}
	rel, err := r.store.BodyPath(body, bodyExt(u.Path))
	if err != nil {
		return fixture.Entry{}, err
	}
	sum := sha256hex(body)
	return fixture.Entry{
		Key:             key,
		Path:            u.Path,
		Query:           u.RawQuery,
		Method:          http.MethodGet,
		Status:          resp.status,
		Headers:         rewriteHeaders(resp.headers, r.svc.Rewrite),
		BodyFile:        rel,
		Size:            int64(len(body)),
		SHA256:          sum,
		ContentEncoding: enc,
		Chunked:         chunked,
		ExpiresIn:       expiresIn,
		ExpiresLayout:   expiresLayout,
	}, nil
}

func (r *Recorder) resolve(ref string) (*url.URL, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty ref")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return nil, err
	}
	abs := r.base.ResolveReference(u)
	abs.Fragment = ""
	if !r.sameOrigin(abs) {
		return nil, fmt.Errorf("not same origin: %s", abs)
	}
	return abs, nil
}

func (r *Recorder) sameOrigin(u *url.URL) bool {
	return u.Scheme == r.base.Scheme && u.Host == r.base.Host
}

func keyOf(u *url.URL) string {
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

func rewriteHeaders(hs []fixture.Header, rw []config.Rewrite) []fixture.Header {
	if len(rw) == 0 {
		return hs
	}
	out := make([]fixture.Header, 0, len(hs))
	for _, h := range hs {
		v := h.Value
		switch strings.ToLower(h.Name) {
		case "location", "content-location", "refresh":
			for _, r := range rw {
				v = strings.ReplaceAll(v, r.From, r.To)
			}
		}
		out = append(out, fixture.Header{Name: h.Name, Value: v})
	}
	return out
}

func applyRewrites(body []byte, rw []config.Rewrite, ctype string) []byte {
	if len(rw) == 0 || !rewritable(ctype) {
		return body
	}
	out := body
	for _, r := range rw {
		if r.From == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(r.From), []byte(r.To))
	}
	return out
}

func rewritable(ctype string) bool {
	c := strings.ToLower(ctype)
	return strings.Contains(c, "text/") || strings.Contains(c, "javascript") ||
		strings.Contains(c, "json") || strings.Contains(c, "xml")
}

// decodeBody decompresses according to the response Content-Encoding header.
func decodeBody(contentEncoding string, body []byte) []byte {
	c := strings.ToLower(contentEncoding)
	var r io.Reader
	switch {
	case strings.Contains(c, "gzip"):
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer zr.Close()
		r = zr
	case strings.Contains(c, "deflate"):
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			fr := flate.NewReader(bytes.NewReader(body))
			defer fr.Close()
			r = fr
		} else {
			defer zr.Close()
			r = zr
		}
	default:
		return body
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return body
	}
	return out
}

func extractHTML(b []byte) []string {
	var refs []string
	for _, re := range []*regexp.Regexp{reLinkHref, reSrcAttr, rePoster, reInline} {
		for _, m := range re.FindAllSubmatch(b, -1) {
			refs = append(refs, string(m[1]))
		}
	}
	for _, m := range reSrcSet.FindAllSubmatch(b, -1) {
		for _, part := range strings.Split(string(m[1]), ",") {
			f := strings.Fields(strings.TrimSpace(part))
			if len(f) > 0 {
				refs = append(refs, f[0])
			}
		}
	}
	refs = append(refs, extractCSS(b)...)
	return filterRefs(refs)
}

func extractCSS(b []byte) []string {
	var refs []string
	for _, m := range reCSSURL.FindAllSubmatch(b, -1) {
		refs = append(refs, string(m[1]))
	}
	for _, m := range reImport.FindAllSubmatch(b, -1) {
		refs = append(refs, string(m[1]))
	}
	return refs
}

func filterRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, r := range refs {
		// HTML entities must be decoded: "?plain=0&amp;v=x" is really "?plain=0&v=x",
		// otherwise the asset gets recorded under a key no browser ever requests.
		r = html.UnescapeString(strings.TrimSpace(r))
		if r == "" || strings.HasPrefix(r, "#") || strings.HasPrefix(r, "data:") ||
			strings.HasPrefix(r, "blob:") || strings.HasPrefix(r, "javascript:") ||
			strings.HasPrefix(r, "mailto:") || strings.HasPrefix(r, "tel:") ||
			strings.HasPrefix(r, "about:") || strings.Contains(r, "{{") {
			continue
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func bodyExt(p string) string {
	ext := strings.ToLower(path.Ext(p))
	if len(ext) < 2 || len(ext) > 7 {
		return ""
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return ext
}

func isHTML(ctype, p string) bool {
	c := strings.ToLower(ctype)
	return strings.Contains(c, "text/html") || (c == "" && strings.HasSuffix(p, ".html"))
}

func isCSS(ctype, p string) bool {
	c := strings.ToLower(ctype)
	return strings.Contains(c, "text/css") || (c == "" && strings.HasSuffix(p, ".css"))
}

// parseHTTPDate accepts both the classic GMT form and the numeric-zone form some
// services (Nextcloud) emit.
func parseHTTPDate(v string) (time.Time, bool) {
	if t, err := http.ParseTime(v); err == nil {
		return t, true
	}
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, "Mon, 02 Jan 2006 15:04:05 -0700",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func dateStyle(v string) string {
	if strings.HasSuffix(strings.TrimSpace(v), "+0000") || strings.Contains(v, " +0000") {
		return "z"
	}
	return "gmt"
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "deadbeef"
	}
	return hex.EncodeToString(b)
}
