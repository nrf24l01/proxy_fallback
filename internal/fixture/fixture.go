package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Header is one on-the-wire response header, order preserved.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Entry is one recorded response.
type Entry struct {
	Key             string   `json:"key"` // path, path?query, or "METHOD path :: content-type"
	Path            string   `json:"path"`
	Query           string   `json:"query,omitempty"`
	Method          string   `json:"method"`
	ReqType         string   `json:"req_content_type,omitempty"`
	ReqBody         string   `json:"req_body,omitempty"`
	Status          int      `json:"status"`
	Headers         []Header `json:"headers"`
	BodyFile        string   `json:"body_file"`
	Size            int64    `json:"size"`
	SHA256          string   `json:"sha256"`
	ContentEncoding string   `json:"content_encoding,omitempty"`
	// ExpiresIn is (Expires - Date) in seconds. Replayed as now+ExpiresIn so a
	// frozen timestamp never becomes a fingerprint ("Expires always in the past").
	ExpiresIn int64 `json:"expires_in,omitempty"`
	// ExpiresLayout is "z" when the service formats Expires with a numeric zone
	// (Nextcloud: "+0000") instead of the usual GMT.
	ExpiresLayout string `json:"expires_layout,omitempty"`
	// Chunked mirrors the upstream framing: some services always answer with
	// chunked transfer encoding, and a static Content-Length would be a tell.
	Chunked bool `json:"chunked,omitempty"`

	// Data is the raw (possibly gzip-encoded) payload; not serialized.
	Data []byte `json:"-"`
	// Identity is the decompressed payload, filled lazily by the mirror.
	Identity []byte `json:"-"`
}

func (e *Entry) Header(name string) string {
	for _, h := range e.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// Manifest is fixtures/<service>/manifest.json.
type Manifest struct {
	Service     string    `json:"service"`
	Title       string    `json:"title,omitempty"`
	Category    string    `json:"category,omitempty"`
	LoginPath   string    `json:"login_path,omitempty"`
	App         string    `json:"app,omitempty"`
	DefaultPort int       `json:"default_port,omitempty"`
	Upstream    string    `json:"upstream"`
	PublicURL   string    `json:"public_url,omitempty"`
	RecordedAt  time.Time `json:"recorded_at"`
	Entries     []Entry   `json:"entries"`
}

// Store writes bodies next to the manifest.
type Store struct {
	Dir string
}

func (s *Store) BodyPath(data []byte, ext string) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])[:20]
	if ext != "" {
		name += ext
	} else {
		name += ".bin"
	}
	rel := filepath.Join("bodies", name)
	if err := os.MkdirAll(filepath.Join(s.Dir, "bodies"), 0o755); err != nil {
		return "", err
	}
	full := filepath.Join(s.Dir, rel)
	if _, err := os.Stat(full); err == nil {
		return rel, nil
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

func (m *Manifest) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Key < m.Entries[j].Key })
	raw, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

// Load reads a manifest and its bodies into memory.
func Load(dir string) (*Manifest, map[string]*Entry, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("parse manifest: %w", err)
	}
	byKey := make(map[string]*Entry, len(m.Entries))
	for i := range m.Entries {
		e := &m.Entries[i]
		data, err := os.ReadFile(filepath.Join(dir, e.BodyFile))
		if err != nil {
			return nil, nil, fmt.Errorf("read body %s for %s: %w", e.BodyFile, e.Key, err)
		}
		e.Data = data
		byKey[e.Key] = e
	}
	return &m, byKey, nil
}
