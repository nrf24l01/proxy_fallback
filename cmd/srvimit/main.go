// Command srvimit imitates self-hosted services: it serves a byte-exact copy of
// a real service's unauthenticated surface (login page, its assets and the real
// answers to login attempts) on a dedicated port, straight from a recorded
// snapshot on disk.
//
//	srvimit list                                        # catalog of recorded services
//	srvimit serve -service grafana -port 8080           # imitate one service 1:1
//	srvimit serve -service grafana:8080 -service memos:8081
//	srvimit serve -all -base-port 8000                  # whole catalog, sequential ports
//	srvimit record -config services.yaml                # crawl live services -> fixtures/
//	srvimit verify -config services.yaml                # compare snapshot with live upstream
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/nrf24l01/proxy_fallback/internal/catalog"
	"github.com/nrf24l01/proxy_fallback/internal/config"
	"github.com/nrf24l01/proxy_fallback/internal/mirror"
	"github.com/nrf24l01/proxy_fallback/internal/recorder"
	"github.com/nrf24l01/proxy_fallback/internal/verify"
)

type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*r = append(*r, p)
		}
	}
	return nil
}

// version is overridden at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)

	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "list", "ls":
		err = cmdList(args)
	case "serve", "run":
		err = cmdServe(args, logger)
	case "record":
		err = cmdRecord(args, logger)
	case "verify":
		err = cmdVerify(args, logger)
	case "help", "-h", "--help":
		usage()
	case "version", "-v", "--version":
		fmt.Printf("srvimit %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
	}
	if err != nil {
		logger.Fatalf("%v", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `srvimit - imitates self-hosted services from recorded snapshots

  srvimit list [-category files,media] [-json]
  srvimit serve -service <name> [-port 8080]            # one service, one port
  srvimit serve -service grafana:8080 -service memos:8081
  srvimit serve -all [-base-port 8000] [-category ...]
  srvimit record -config services.yaml [-only a,b]
  srvimit verify -config services.yaml [-only a,b] [-content-only]

Common flags: -fixtures ./fixtures (env SRVIMIT_FIXTURES)
`)
	os.Exit(2)
}

func fixturesRoot(fs *flag.FlagSet) *string {
	return fs.String("fixtures", envOr("SRVIMIT_FIXTURES", "fixtures"), "snapshot directory")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	root := fixturesRoot(fs)
	cat := fs.String("category", "", "comma separated categories")
	asJSON := fs.Bool("json", false, "machine readable output")
	_ = fs.Parse(args)

	list, err := catalog.Load(*root)
	if err != nil {
		return err
	}
	if *cat != "" {
		want := map[string]bool{}
		for _, c := range strings.Split(*cat, ",") {
			want[strings.ToLower(strings.TrimSpace(c))] = true
		}
		var filtered []catalog.Entry
		for _, e := range list {
			if want[strings.ToLower(e.Category)] {
				filtered = append(filtered, e)
			}
		}
		list = filtered
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		return enc.Encode(list)
	}
	if len(list) == 0 {
		return fmt.Errorf("nothing matched")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tCATEGORY\tLOGIN PATH\tRESPONSES\tSIZE\tRECORDED")
	for _, e := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%.1f MB\t%s\n",
			e.Name, e.Category, e.LoginPath, e.Entries, float64(e.Bytes)/(1<<20),
			e.RecordedAt.Format("2006-01-02"))
	}
	w.Flush()
	fmt.Printf("\n%d services in %d categories: %s\n", len(list), len(catalog.Categories(list)),
		strings.Join(catalog.Categories(list), ", "))
	return nil
}

type target struct {
	name string
	port int
}

func parseTargets(specs []string, root string) ([]config.Service, error) {
	var out []config.Service
	usedPorts := map[int]bool{}
	for _, spec := range specs {
		name, portStr := spec, ""
		if i := strings.LastIndex(spec, ":"); i > 0 {
			name, portStr = spec[:i], spec[i+1:]
		}
		entry, err := catalog.Find(root, name)
		if err != nil {
			return nil, err
		}
		port := entry.DefaultPort
		if portStr != "" {
			p, err := strconv.Atoi(portStr)
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("service %q: bad port %q", name, portStr)
			}
			port = p
		}
		if port == 0 {
			return nil, fmt.Errorf("service %q: no default port; specify %s:<port>", name, name)
		}
		if usedPorts[port] {
			return nil, fmt.Errorf("port %d used twice", port)
		}
		usedPorts[port] = true
		out = append(out, config.Service{
			Name:       entry.Name,
			Port:       port,
			FixtureDir: entry.Dir,
			Title:      entry.Title,
			Category:   entry.Category,
			LoginPath:  entry.LoginPath,
		}.Defaults())
	}
	return out, nil
}

func cmdServe(args []string, logger *log.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fixturesRoot(fs)
	listen := fs.String("listen", "127.0.0.1", "listen address")
	all := fs.Bool("all", false, "serve every service in the catalog")
	basePort := fs.Int("base-port", 8000, "first port for -all")
	port := fs.Int("port", 0, "port to serve on (with a single -service)")
	category := fs.String("category", "", "with -all: only these categories")
	var specs repeated
	fs.Var(&specs, "service", "service[:port] (repeatable, comma separated)")
	_ = fs.Parse(args)

	var services []config.Service
	switch {
	case *all:
		entries, err := catalog.Load(*root)
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, c := range strings.Split(*category, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
				want[c] = true
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		port := *basePort
		for _, e := range entries {
			if len(want) > 0 && !want[strings.ToLower(e.Category)] {
				continue
			}
			services = append(services, config.Service{
				Name: e.Name, Port: port, FixtureDir: e.Dir, Title: e.Title,
				Category: e.Category, LoginPath: e.LoginPath,
			})
			port++
		}
		if len(services) == 0 {
			return fmt.Errorf("no services matched")
		}
	case len(specs) > 0:
		if *port != 0 && len(specs) > 1 {
			return fmt.Errorf("-port works with a single -service; use name:port for several")
		}
		var err error
		services, err = parseTargets(specs, *root)
		if err != nil {
			return err
		}
		if *port != 0 {
			if *port < 1 || *port > 65535 {
				return fmt.Errorf("bad -port %d", *port)
			}
			services[0].Port = *port
		}
	default:
		fs.Usage()
		return fmt.Errorf("specify -service <name>[:port] or -all")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := &config.Config{Listen: *listen}
	return mirror.Run(ctx, cfg, services, logger)
}

func cmdRecord(args []string, logger *log.Logger) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	cfgPath := fs.String("config", "services.yaml", "path to services.yaml")
	only := fs.String("only", "", "comma separated service names")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	services := cfg.Select(*only)
	if len(services) == 0 {
		return fmt.Errorf("no services matched -only=%q", *only)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := 0
	for _, svc := range services {
		rec, err := recorder.New(svc, logger)
		if err != nil {
			logger.Printf("[record] %s: %v", svc.Name, err)
			failed++
			continue
		}
		if err := rec.Run(ctx); err != nil {
			logger.Printf("[record] %s: %v", svc.Name, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d service(s) failed", failed)
	}
	return nil
}

func cmdVerify(args []string, logger *log.Logger) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	cfgPath := fs.String("config", "services.yaml", "path to services.yaml")
	only := fs.String("only", "", "comma separated service names")
	strict := fs.Bool("strict", false, "require byte-identical HTML bodies (fails on nonce/CSRF)")
	contentOnly := fs.Bool("content-only", false, "compare status and body only (instance-specific headers ignored)")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	services := cfg.Select(*only)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return verify.Run(ctx, services, *strict, *contentOnly, logger)
}
