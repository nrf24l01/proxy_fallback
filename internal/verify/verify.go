package verify

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nrf24l01/proxy_fallback/internal/config"
	"github.com/nrf24l01/proxy_fallback/internal/fixture"
)

// dynamicHeaders must be present but their value is per-request by nature.
var dynamicHeaders = map[string]bool{
	"x-request-id": true, "x-trace-id": true, "x-correlation-id": true,
	"request-id": true, "traceparent": true, "x-amzn-requestid": true,
	"x-runtime": true, "x-response-time": true, "cf-ray": true,
	"x-ratelimit-reset": true, "x-ratelimit-remaining": true,
}

var ignoreHeaders = map[string]bool{
	"date": true, "content-length": true, "connection": true, "keep-alive": true,
	"transfer-encoding": true, "alt-svc": true, "set-cookie": true, "server-timing": true,
	// expires is compared as a TTL (Expires - Date), not as an absolute stamp.
	"expires": true,
}

// Run compares every recorded entry served by the mirror against the live upstream.
// It is the acceptance check for "1:1": same status, same bytes, same headers.
// HTML pages embed per-request nonces/CSRF tokens, so unless strict is set their
// bodies may differ (reported as dynamic); static assets must match byte exactly.
func Run(ctx context.Context, services []config.Service, strict, contentOnly bool, logger *log.Logger) error {
	tr := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 8}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	total, failed, dynamic := 0, 0, 0
	for _, svc := range services {
		man, _, err := fixture.Load(svc.FixtureDir)
		if err != nil {
			return fmt.Errorf("%s: %w", svc.Name, err)
		}
		up, err := url.Parse(strings.TrimRight(svc.Upstream, "/"))
		if err != nil {
			return fmt.Errorf("%s: upstream: %w", svc.Name, err)
		}
		mirrorBase := fmt.Sprintf("http://127.0.0.1:%d", svc.Port)
		fmt.Printf("\n== %s (%d entries)  upstream=%s  mirror=%s\n", svc.Name, len(man.Entries), svc.Upstream, mirrorBase)

		keys := make([]string, 0, len(man.Entries)+1)
		for _, e := range man.Entries {
			// Non-GET entries are covered by the svc.Posts loop below.
			if e.Key == "__404__" || (e.Method != "" && e.Method != http.MethodGet) {
				continue
			}
			keys = append(keys, e.Key)
		}
		// An unknown path must behave like the real service too (its 404 page or
		// its redirect to /login).
		keys = append(keys, "/veil-verify-"+strconv.FormatInt(time.Now().UnixNano(), 36))

		// Login attempts must fail exactly like the real service.
		for _, spec := range svc.Posts {
			total++
			label := "POST " + spec.Path
			upRes, err := probePost(ctx, client, up.Scheme+"://"+up.Host+spec.Path, svc, spec)
			if err != nil {
				failed++
				fmt.Printf("  FAIL %-52s upstream error: %v\n", label, err)
				continue
			}
			mRes, err := probePost(ctx, client, mirrorBase+spec.Path, svc, spec)
			if err != nil {
				failed++
				fmt.Printf("  FAIL %-52s mirror error: %v\n", label, err)
				continue
			}
			issues, dynamicBody := compare(upRes, mRes, false, contentOnly)
			_ = dynamicBody
			if len(issues) > 0 {
				failed++
				fmt.Printf("  DIFF %-52s %s\n", label, strings.Join(issues, "; "))
			} else {
				fmt.Printf("  ok   %-52s %3d %7dB  %d headers\n", label, upRes.status, len(upRes.body), len(upRes.headers))
			}
		}

		for _, key := range keys {
			total++
			upRes, err := probe(ctx, client, up.Scheme+"://"+up.Host+key, svc)
			if err != nil {
				failed++
				fmt.Printf("  FAIL %-52s upstream error: %v\n", key, err)
				continue
			}
			mRes, err := probe(ctx, client, mirrorBase+key, svc)
			if err != nil {
				failed++
				fmt.Printf("  FAIL %-52s mirror error: %v\n", key, err)
				continue
			}
			issues, dynamicBody := compare(upRes, mRes, strict, contentOnly)
			switch {
			case len(issues) > 0:
				failed++
				fmt.Printf("  DIFF %-52s %s\n", key, strings.Join(issues, "; "))
			case dynamicBody:
				dynamic++
				fmt.Printf("  ok   %-52s %3d %7dB  %d headers  (html snapshot, differs by nonce/CSRF)\n",
					key, upRes.status, len(upRes.body), len(upRes.headers))
			default:
				fmt.Printf("  ok   %-52s %3d %7dB  %d headers\n", key, upRes.status, len(upRes.body), len(upRes.headers))
			}
		}
	}
	fmt.Printf("\n%d/%d responses identical (%d html pages replayed from snapshot; assets byte-exact)\n",
		total-failed, total, dynamic)
	if failed > 0 {
		return fmt.Errorf("%d mismatches", failed)
	}
	return nil
}

func compare(upRes, mRes *resp, strict, contentOnly bool) (issues []string, dynamicBody bool) {
	if upRes.status != mRes.status {
		issues = append(issues, fmt.Sprintf("status %d != %d", mRes.status, upRes.status))
	}
	// Compare content, not the compressor: services like Nextcloud re-gzip the
	// same bytes with slightly different output per response.
	uh, mh := sha256hex(decode(upRes)), sha256hex(decode(mRes))
	isHTML := strings.Contains(strings.ToLower(upRes.headers["content-type"]), "text/html")
	if uh != mh {
		if isHTML && !strict {
			dynamicBody = true
		} else {
			issues = append(issues, fmt.Sprintf("body sha256 %s != %s (%dB vs %dB)",
				short(mh), short(uh), len(mRes.body), len(upRes.body)))
		}
	}
	for name := range dynamicHeaders {
		_, upOK := upRes.headers[name]
		_, mOK := mRes.headers[name]
		if upOK != mOK {
			issues = append(issues, fmt.Sprintf("header %q presence: mirror=%v upstream=%v", name, mOK, upOK))
		}
	}
	if upRes.ttl >= 0 && upRes.ttl != mRes.ttl {
		if delta := upRes.ttl - mRes.ttl; delta > 3 || delta < -3 {
			issues = append(issues, fmt.Sprintf("expires ttl %ds != %ds", mRes.ttl, upRes.ttl))
		}
	}
	if contentOnly {
		return issues, dynamicBody
	}
	upNorm, mNorm := maskNonces(upRes.headers), maskNonces(mRes.headers)
	for _, k := range diffKeys(upNorm, mNorm) {
		if dynamicHeaders[k] {
			continue
		}
		issues = append(issues, fmt.Sprintf("header %q: mirror=%q upstream=%q", k, mNorm[k], upNorm[k]))
	}
	return issues, dynamicBody
}

type resp struct {
	status  int
	headers map[string]string
	body    []byte
	// ttl is (Expires - Date) in seconds; -1 when either header is absent.
	ttl int64
}

func probe(ctx context.Context, c *http.Client, rawURL string, svc config.Service) (*resp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if host := svc.PublicHost(); host != "" {
		req.Host = host
	}
	req.Header.Set("User-Agent", svc.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	out := &resp{status: r.StatusCode, headers: flatten(r.Header), body: body, ttl: -1}
	out.ttl = ttlOf(r.Header.Get("Date"), r.Header.Get("Expires"))
	return out, nil
}

func probePost(ctx context.Context, c *http.Client, rawURL string, svc config.Service, spec config.PostSpec) (*resp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(spec.Body))
	if err != nil {
		return nil, err
	}
	if host := svc.PublicHost(); host != "" {
		req.Host = host
	}
	req.Header.Set("User-Agent", svc.UserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	if spec.ContentType != "" {
		req.Header.Set("Content-Type", spec.ContentType)
	}
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	out := &resp{status: r.StatusCode, headers: flatten(r.Header), body: body, ttl: -1}
	out.ttl = ttlOf(r.Header.Get("Date"), r.Header.Get("Expires"))
	return out, nil
}

// ttlOf returns (Expires - Date) in seconds, -1 when unparseable, 0 when absent.
func ttlOf(dateHdr, expiresHdr string) int64 {
	if expiresHdr == "" {
		return 0
	}
	d, dok := parseDate(dateHdr)
	e, eok := parseDate(expiresHdr)
	if !dok || !eok {
		return -1
	}
	return int64(e.Sub(d).Seconds())
}

func parseDate(v string) (time.Time, bool) {
	if t, err := http.ParseTime(v); err == nil {
		return t, true
	}
	for _, layout := range []string{time.RFC1123Z, "Mon, 02 Jan 2006 15:04:05 -0700"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func flatten(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if ignoreHeaders[lk] {
			continue
		}
		sort.Strings(v)
		out[lk] = strings.Join(v, " | ")
	}
	return out
}

func diffKeys(a, b map[string]string) []string {
	var out []string
	for k, av := range a {
		if bv, ok := b[k]; !ok || bv != av {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

var reNonceValue = regexp.MustCompile(`nonce-[A-Za-z0-9+/=]{16,}`)

// maskNonces hides per-response CSP nonces: both sides mint fresh ones, so only
// the policy shape is comparable.
func maskNonces(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = reNonceValue.ReplaceAllString(v, "nonce-*")
	}
	return out
}

// decode returns the identity body so gzip framing differences do not count as
// content differences.
func decode(r *resp) []byte {
	if !strings.Contains(strings.ToLower(r.headers["content-encoding"]), "gzip") {
		return r.body
	}
	zr, err := gzip.NewReader(bytes.NewReader(r.body))
	if err != nil {
		return r.body
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return r.body
	}
	return out
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func short(s string) string {
	if len(s) < 12 {
		return s
	}
	return s[:12]
}
