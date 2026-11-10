package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nrf24l01/proxy_fallback/internal/config"
)

// TestCatalogDefinitions keeps catalog/services.yaml loadable and honest: CI runs
// this so a typo cannot silently disable a service.
func TestCatalogDefinitions(t *testing.T) {
	raw, err := os.ReadFile("../../catalog/services.yaml")
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var entries []entry
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("catalog is empty")
	}

	seenName := map[string]bool{}
	seenPort := map[int]bool{}
	for i, e := range entries {
		if e.Name == "" {
			t.Fatalf("entry %d: missing name", i)
		}
		if seenName[e.Name] {
			t.Errorf("%s: duplicate name", e.Name)
		}
		seenName[e.Name] = true

		if e.Skip != "" {
			continue // explicitly disabled, documented in the YAML
		}
		if seenPort[e.HostPort] {
			t.Errorf("%s: duplicate host_port %d", e.Name, e.HostPort)
		}
		seenPort[e.HostPort] = true

		if e.HostPort < 1024 || e.HostPort > 65535 {
			t.Errorf("%s: host_port %d out of range", e.Name, e.HostPort)
		}
		if e.Title == "" || e.Category == "" {
			t.Errorf("%s: title and category are required", e.Name)
		}
		if e.LoginPath == "" {
			t.Errorf("%s: login_path is required (services without a login page are not catalogued)", e.Name)
		}
		if len(e.Compose) == 0 {
			t.Errorf("%s: compose definition is required", e.Name)
		}
		if svcs, ok := e.Compose["services"].(map[string]any); !ok || len(svcs) == 0 {
			t.Errorf("%s: compose.services must define at least one container", e.Name)
		}
		if e.Wait == "" {
			t.Errorf("%s: wait path is required for readiness", e.Name)
		}
		for _, p := range e.Posts {
			if !strings.HasPrefix(p.Path, "/") {
				t.Errorf("%s: post path %q must be absolute", e.Name, p.Path)
			}
			if p.ContentType == "" {
				t.Errorf("%s: post %q needs content_type", e.Name, p.Path)
			}
		}
		if e.PublicURL != "" && !strings.HasPrefix(e.PublicURL, "http") {
			t.Errorf("%s: public_url must be absolute", e.Name)
		}
	}
}

// TestCatalogServicesDefaults covers the programmatic Service path used by
// `serve -service`: defaults must apply so a snapshot is never recorded with
// Go's own User-Agent.
func TestCatalogServicesDefaults(t *testing.T) {
	svc := config.Service{Name: "x", Port: 8080}.Defaults()
	if svc.UserAgent == "" || strings.Contains(svc.UserAgent, "Go-http-client") {
		t.Fatalf("user agent not defaulted: %q", svc.UserAgent)
	}
	if svc.MaxAssets <= 0 || svc.NotFound == "" || len(svc.Pages) == 0 {
		t.Fatalf("defaults incomplete: %+v", svc)
	}
	if svc.PublicHost() != "" {
		t.Fatalf("unexpected public host: %q", svc.PublicHost())
	}
}
