// Command catalogup brings catalog services up in Docker, records their front
// pages into fixtures/<name>/ and tears them down again.
//
//	go run ./cmd/catalogup -services vaultwarden,memos -keep
//	go run ./cmd/catalogup -all -timeout 300
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nrf24l01/proxy_fallback/internal/config"
	"github.com/nrf24l01/proxy_fallback/internal/fixture"
	"github.com/nrf24l01/proxy_fallback/internal/recorder"
)

type entry struct {
	Name      string            `yaml:"name"`
	Title     string            `yaml:"title"`
	Category  string            `yaml:"category"`
	HostPort  int               `yaml:"host_port"`
	LoginPath string            `yaml:"login_path"`
	Pages     []string          `yaml:"pages"`
	API       []string          `yaml:"api"`
	Wait      string            `yaml:"wait"`
	Posts     []config.PostSpec `yaml:"posts"`
	// Setup commands run after the service is reachable and before recording
	// (create an admin user, enable an option, etc). {port} is substituted.
	Setup     []string       `yaml:"setup"`
	PublicURL string         `yaml:"public_url"`
	Skip      string         `yaml:"skip,omitempty"`
	Compose   map[string]any `yaml:"compose"`
}

func main() {
	catalogPath := flag.String("catalog", "catalog/services.yaml", "catalog definition")
	fixturesDir := flag.String("fixtures", "fixtures", "output directory")
	only := flag.String("services", "", "comma separated names")
	all := flag.Bool("all", false, "every catalog service")
	keep := flag.Bool("keep", false, "keep containers running after recording")
	skipRecord := flag.Bool("no-record", false, "only start containers")
	timeout := flag.Duration("timeout", 240*time.Second, "per-service readiness timeout")
	capture := flag.Bool("capture", false, "ask headless Chromium which URLs the page loads, then record those too")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)

	raw, err := os.ReadFile(*catalogPath)
	must(err)
	var entries []entry
	must(yaml.Unmarshal(raw, &entries))

	want := map[string]bool{}
	if *only != "" {
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
	}
	var selected []entry
	for _, e := range entries {
		if e.Skip != "" {
			continue
		}
		if *all || want[e.Name] {
			selected = append(selected, e)
			delete(want, e.Name)
		}
	}
	if len(want) > 0 {
		var missing []string
		for n := range want {
			missing = append(missing, n)
		}
		logger.Fatalf("unknown catalog services: %s", strings.Join(missing, ","))
	}
	if len(selected) == 0 {
		logger.Fatalf("nothing selected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make([]string, 0, len(selected))
	for _, e := range selected {
		logger.Printf("=== %s (%s, :%d)", e.Name, e.Category, e.HostPort)
		if err := up(ctx, e, logger); err != nil {
			logger.Printf("[%s] up failed: %v", e.Name, err)
			results = append(results, fmt.Sprintf("%-16s FAIL up: %v", e.Name, err))
			_ = down(ctx, e, logger)
			continue
		}
		if err := waitReady(ctx, e, *timeout); err != nil {
			logger.Printf("[%s] not ready: %v", e.Name, err)
			results = append(results, fmt.Sprintf("%-16s FAIL ready: %v", e.Name, err))
			logs, _ := exec.Command("docker", "compose", "-f", composePath(e), "-p", project(e), "logs", "--tail", "5").CombinedOutput()
			logger.Printf("[%s] logs:\n%s", e.Name, strings.TrimSpace(string(logs)))
			if !*keep {
				_ = down(ctx, e, logger)
			}
			continue
		}

		if *skipRecord {
			results = append(results, fmt.Sprintf("%-16s UP (no record)", e.Name))
			continue
		}
		for _, cmdline := range e.Setup {
			cmdline = strings.ReplaceAll(cmdline, "{port}", strconv.Itoa(e.HostPort))
			out, err := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline).CombinedOutput()
			if err != nil {
				logger.Printf("[%s] setup %q failed: %v (%s)", e.Name, cmdline, err, strings.TrimSpace(string(out)))
				continue
			}
			logger.Printf("[%s] setup ok: %s", e.Name, strings.TrimSpace(string(out)))
		}

		urlsFile := ""
		if *capture {
			urlsFile = filepath.Join(*fixturesDir, e.Name, "urls.txt")
			if err := captureURLs(ctx, e, urlsFile, logger); err != nil {
				logger.Printf("[%s] capture skipped: %v", e.Name, err)
				urlsFile = ""
			}
		}

		svc := config.Service{
			Name:           e.Name,
			Port:           e.HostPort,
			FixtureDir:     filepath.Join(*fixturesDir, e.Name),
			Title:          e.Title,
			Category:       e.Category,
			LoginPath:      e.LoginPath,
			DefaultPort:    e.HostPort,
			Upstream:       fmt.Sprintf("http://127.0.0.1:%d", e.HostPort),
			PublicURL:      publicURL(e),
			Pages:          e.Pages,
			API:            e.API,
			Posts:          e.Posts,
			ExtraURLsFile:  urlsFile,
			RecordNotFound: true,
			MaxAssets:      700,
		}
		svc = svc.Defaults()
		rec, err := recorder.New(svc, logger)
		if err != nil {
			results = append(results, fmt.Sprintf("%-16s FAIL recorder: %v", e.Name, err))
			continue
		}
		if err := rec.Run(ctx); err != nil {
			results = append(results, fmt.Sprintf("%-16s FAIL record: %v", e.Name, err))
		} else {
			man, _, err := fixture.Load(svc.FixtureDir)
			if err != nil {
				results = append(results, fmt.Sprintf("%-16s FAIL load: %v", e.Name, err))
			} else {
				var size int64
				login := "no-login-page"
				for _, en := range man.Entries {
					size += en.Size
					if e.LoginPath != "" && strings.HasPrefix(en.Key, strings.TrimSuffix(e.LoginPath, "/")) && en.Status < 400 {
						login = "login-ok"
					}
				}
				results = append(results, fmt.Sprintf("%-16s ok %3d entries %6.2f MB %s", e.Name, len(man.Entries), float64(size)/(1<<20), login))
			}
		}
		if !*keep {
			_ = down(ctx, e, logger)
		}
	}

	fmt.Println("\n==== summary ====")
	for _, r := range results {
		fmt.Println(r)
	}
}

func publicURL(e entry) string {
	if e.PublicURL != "" {
		return e.PublicURL
	}
	return fmt.Sprintf("https://%s.example.com", e.Name)
}

func project(e entry) string { return "veil-" + e.Name }

func composePath(e entry) string { return filepath.Join(".catalog", e.Name, "docker-compose.yml") }

func up(ctx context.Context, e entry, logger *log.Logger) error {
	if err := os.MkdirAll(filepath.Dir(composePath(e)), 0o755); err != nil {
		return err
	}
	out, err := yaml.Marshal(e.Compose)
	if err != nil {
		return err
	}
	if err := os.WriteFile(composePath(e), out, 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath(e), "-p", project(e), "up", "-d", "--quiet-pull")
	cmd.Stdout = logger.Writer()
	cmd.Stderr = logger.Writer()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	return nil
}

func down(ctx context.Context, e entry, logger *log.Logger) error {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath(e), "-p", project(e), "down", "-v", "--remove-orphans")
	cmd.Stdout = logger.Writer()
	cmd.Stderr = logger.Writer()
	return cmd.Run()
}

func waitReady(ctx context.Context, e entry, timeout time.Duration) error {
	path := e.Wait
	if path == "" {
		path = "/"
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", e.HostPort, path)
	var last error
	for time.Now().Before(deadline) {
		if exited, state := containersExited(ctx, e); exited {
			return fmt.Errorf("container stopped (%s)", state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (catalog readiness probe)")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusBadGateway && resp.StatusCode != 503 {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(2 * time.Second)
	}
	return last
}

// containersExited reports whether every container of the project has already
// stopped, so readiness waiting fails fast instead of burning the whole timeout.
func containersExited(ctx context.Context, e entry) (bool, string) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project="+project(e),
		"--format", "{{.State}}	{{.Status}}").Output()
	if err != nil {
		return false, ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return false, ""
	}
	// If the project has an "app" container, that is the one that must stay up;
	// database sidecars exiting is irrelevant.
	var appLine string
	for _, l := range lines {
		if strings.Contains(l, "-app-") {
			appLine = l
		}
	}
	if appLine != "" {
		st := strings.Fields(appLine)
		if len(st) > 0 && st[0] == "running" || len(st) > 0 && st[0] == "restarting" {
			return false, ""
		}
		return true, strings.ReplaceAll(appLine, "\t", " ")
	}
	for _, l := range lines {
		st := strings.Fields(l)
		if len(st) == 0 {
			continue
		}
		switch st[0] {
		case "running", "created", "restarting", "paused":
			return false, ""
		}
	}
	return true, strings.ReplaceAll(lines[0], "\t", " ")
}

// captureURLs drives headless Chromium through the page and stores every URL it
// requested, so lazy JS chunks and XHR endpoints land in the snapshot too.
func captureURLs(ctx context.Context, e entry, out string, logger *log.Logger) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	page := fmt.Sprintf("http://127.0.0.1:%d%s", e.HostPort, e.LoginPath)
	cmd := exec.CommandContext(ctx, "./tools/capture-urls.sh", page, out, e.Name+".example.com")
	outb, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(outb)))
	}
	logger.Printf("[%s] %s", e.Name, strings.TrimSpace(string(outb)))
	return nil
}

func must(err error) {
	if err != nil {
		log.Fatalf("%v", err)
	}
}

var _ = strconv.Itoa
