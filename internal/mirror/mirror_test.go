package mirror

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrf24l01/proxy_fallback/internal/config"
	"github.com/nrf24l01/proxy_fallback/internal/fixture"
)

func newFixture(t *testing.T, dir string, entries []fixture.Entry, bodies [][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bodies"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		name := "bodies/b" + string(rune('a'+i)) + ".bin"
		if err := os.WriteFile(filepath.Join(dir, name), bodies[i], 0o644); err != nil {
			t.Fatal(err)
		}
		entries[i].BodyFile = name
		entries[i].Size = int64(len(bodies[i]))
	}
	man := fixture.Manifest{Service: "test", RecordedAt: time.Now(), Entries: entries}
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}
}

func TestReplayPreservesHeadersAndBody(t *testing.T) {
	dir := t.TempDir()
	body := []byte("<html>login</html>")
	newFixture(t, dir, []fixture.Entry{{
		Key: "/login", Path: "/login", Method: "GET", Status: 200,
		Headers: []fixture.Header{
			{Name: "Content-Type", Value: "text/html; charset=UTF-8"},
			{Name: "Cache-Control", Value: "no-store"},
			{Name: "X-Frame-Options", Value: "deny"},
			{Name: "ETag", Value: `"abc123"`},
			{Name: "Set-Cookie", Value: "session=leak"},
			{Name: "Date", Value: "Mon, 01 Jan 2001 00:00:00 GMT"},
		},
	}}, [][]byte{body})

	f, err := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "plain"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != string(body) {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=UTF-8" {
		t.Errorf("content-type = %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "deny" {
		t.Errorf("xfo = %q", got)
	}
	if got := rec.Header().Get("ETag"); got != `"abc123"` {
		t.Errorf("etag = %q", got)
	}
	if got := rec.Header().Get("Date"); got == "Mon, 01 Jan 2001 00:00:00 GMT" {
		t.Error("stale Date header must not be replayed")
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("set-cookie replayed: %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "18" {
		t.Errorf("content-length = %q", got)
	}
}

func TestHeadHasHeadersWithoutBody(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, []fixture.Entry{{
		Key: "/", Path: "/", Method: "GET", Status: 200,
		Headers: []fixture.Header{{Name: "Content-Type", Value: "text/html"}},
	}}, [][]byte{[]byte("hello")})

	f, _ := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "plain"}, nil)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("head: status=%d len=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != "5" {
		t.Fatalf("content-length = %q", rec.Header().Get("Content-Length"))
	}
}

func TestConditionalRequestReturns304(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, []fixture.Entry{{
		Key: "/", Path: "/", Method: "GET", Status: 200,
		Headers: []fixture.Header{{Name: "ETag", Value: `"v1"`}, {Name: "Content-Type", Value: "text/html"}},
	}}, [][]byte{[]byte("x")})

	f, _ := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "plain"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 with body")
	}
}

func TestUnknownPathUsesRecorded404(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, []fixture.Entry{{
		Key: "__404__", Path: "/veil-404", Method: "GET", Status: 404,
		Headers: []fixture.Header{{Name: "Content-Type", Value: "text/html"}, {Name: "X-Real", Value: "1"}},
	}}, [][]byte{[]byte("real 404 page")})

	f, _ := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "fixture"}, nil)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "real 404 page" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-Real") != "1" {
		t.Fatal("404 headers not replayed")
	}
}

func TestQueryStringLookup(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, []fixture.Entry{
		{Key: "/api/x?y=1", Path: "/api/x", Query: "y=1", Method: "GET", Status: 200,
			Headers: []fixture.Header{{Name: "Content-Type", Value: "application/json"}}},
		{Key: "/api/x", Path: "/api/x", Method: "GET", Status: 200,
			Headers: []fixture.Header{{Name: "Content-Type", Value: "application/json"}}},
	}, [][]byte{[]byte(`{"q":true}`), []byte(`{"q":false}`)})

	f, _ := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "plain"}, nil)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x?y=1", nil))
	if rec.Body.String() != `{"q":true}` {
		t.Fatalf("query lookup failed: %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x?y=2", nil))
	if rec.Body.String() != `{"q":false}` {
		t.Fatalf("path fallback failed: %q", rec.Body.String())
	}
}

func TestPostReplaysRecordedFailure(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, []fixture.Entry{
		{Key: "/login", Path: "/login", Method: "GET", Status: 200,
			Headers: []fixture.Header{{Name: "Content-Type", Value: "text/html"}}},
		{Key: "POST /login", Path: "/login", Method: "POST", Status: 401,
			Headers: []fixture.Header{
				{Name: "Content-Type", Value: "application/json"},
				{Name: "Cache-Control", Value: "no-store"},
			}},
	}, [][]byte{
		[]byte("<html>login page</html>"),
		[]byte(`{"message":"Invalid username or password","traceID":""}`),
	})

	f, err := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "plain"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// GET still serves the page.
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != 200 || rec.Body.String() != "<html>login page</html>" {
		t.Fatalf("GET: %d %q", rec.Code, rec.Body.String())
	}

	// Any POST body gets the service's real failure response, byte for byte.
	for _, body := range []string{"", `{"user":"admin","password":"wrong"}`, "user=a&password=b"} {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		f.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("POST %q: status = %d, want 401", body, rec.Code)
		}
		if got, want := rec.Body.String(), `{"message":"Invalid username or password","traceID":""}`; got != want {
			t.Fatalf("POST %q: body = %q, want %q", body, got, want)
		}
		if rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("POST content-type = %q", rec.Header().Get("Content-Type"))
		}
	}
}

func TestPostWithoutRecordedFailureFallsBackToPage(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, []fixture.Entry{{
		Key: "/login", Path: "/login", Method: "GET", Status: 200,
		Headers: []fixture.Header{{Name: "Content-Type", Value: "text/html"}},
	}}, [][]byte{[]byte("<html>login page</html>")})

	f, _ := Load(config.Service{Name: "test", FixtureDir: dir, NotFound: "plain"}, nil)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("x")))
	if rec.Code != 200 || rec.Body.String() != "<html>login page</html>" {
		t.Fatalf("fallback: %d %q", rec.Code, rec.Body.String())
	}
}
