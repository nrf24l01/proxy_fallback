// Package catalog discovers recorded service snapshots on disk.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one service available for serving.
type Entry struct {
	Name        string    `json:"name"`
	Title       string    `json:"title"`
	Category    string    `json:"category"`
	LoginPath   string    `json:"login_path"`
	App         string    `json:"app"`
	RecordedAt  time.Time `json:"recorded_at"`
	Entries     int       `json:"responses"`
	Bytes       int64     `json:"bytes"`
	DefaultPort int       `json:"default_port"`
	Dir         string    `json:"-"`
}

// Load reads every fixtures/<name>/manifest.json under root.
func Load(root string) ([]Entry, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read fixtures dir %s: %w", root, err)
	}
	var out []Entry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		e, err := loadOne(filepath.Join(root, d.Name()))
		if err != nil {
			continue // not a snapshot
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("no snapshots found under %s (record them with `srvimit record`, or mount them at /data/fixtures)", root)
	}
	return out, nil
}

func loadOne(dir string) (Entry, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Entry{}, err
	}
	var m struct {
		Service     string    `json:"service"`
		Title       string    `json:"title"`
		Category    string    `json:"category"`
		LoginPath   string    `json:"login_path"`
		App         string    `json:"app"`
		DefaultPort int       `json:"default_port"`
		RecordedAt  time.Time `json:"recorded_at"`
		Entries     []struct {
			Size int64 `json:"size"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Entry{}, err
	}
	if m.Service == "" {
		return Entry{}, fmt.Errorf("%s: no service name", dir)
	}
	var size int64
	for _, e := range m.Entries {
		size += e.Size
	}
	e := Entry{
		Name:        m.Service,
		Title:       m.Title,
		Category:    m.Category,
		LoginPath:   m.LoginPath,
		App:         m.App,
		RecordedAt:  m.RecordedAt,
		Entries:     len(m.Entries),
		Bytes:       size,
		DefaultPort: m.DefaultPort,
		Dir:         dir,
	}
	if e.Title == "" {
		e.Title = e.Name
	}
	if e.Category == "" {
		e.Category = "other"
	}
	return e, nil
}

// Find resolves one service by name (case-insensitive).
func Find(root, name string) (Entry, error) {
	list, err := Load(root)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range list {
		if strings.EqualFold(e.Name, name) {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("service %q not found in %s (try: srvimit list)", name, root)
}

// Categories returns unique categories present in the catalog.
func Categories(list []Entry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range list {
		if !seen[e.Category] {
			seen[e.Category] = true
			out = append(out, e.Category)
		}
	}
	sort.Strings(out)
	return out
}
