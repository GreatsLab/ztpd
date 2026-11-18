// ztpd: zero-touch provisioning for switches.
//
//	ztpd serve    -inventory inventory.json -devices devices.csv -templates templates/
//	ztpd check    (same flags)   render every device, exit 1 on any problem: run it in CI
//	ztpd render   -serial X      print one device's config
//	ztpd simulate -server URL    pretend to be the switches booting (for testing)
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GreatsLab/ztpd/internal/inv"
	"github.com/GreatsLab/ztpd/internal/render"
	"github.com/GreatsLab/ztpd/internal/server"
)

//go:embed web
var webFS embed.FS

var version = "0.3.0"

type common struct {
	inventory, devices, templates string
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.inventory, "inventory", env("ZTPD_INVENTORY", "inventory.json"), "sites, roles and defaults (JSON)")
	fs.StringVar(&c.devices, "devices", env("ZTPD_DEVICES", "devices.csv"), "device sheet (CSV: serial,hostname,role,site,...)")
	fs.StringVar(&c.templates, "templates", env("ZTPD_TEMPLATES", "templates"), "directory of *.tmpl config templates")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ztpd: ")
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		serve(args)
	case "check":
		os.Exit(check(args))
	case "render":
		renderOne(args)
	case "simulate":
		simulate(args)
	case "version", "-v", "--version":
		fmt.Println("ztpd", version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: ztpd <command> [flags]

  serve      run the provisioning server and dashboard
  check      render every device's config and report problems (exit 1 if any)
  render     print one device's config: ztpd render -serial FOC2231X0A1
  simulate   act like switches booting against a running server
  version

run "ztpd <command> -h" for flags`)
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.flags(fs)
	listen := fs.String("listen", env("ZTPD_LISTEN", ":8080"), "listen address")
	url := fs.String("url", env("ZTPD_URL", ""), "URL the switches use to reach this server (default: the Host header they send)")
	allow := fs.String("allow", env("ZTPD_ALLOW", ""), "comma-separated networks allowed to fetch configs, e.g. 10.20.99.0/24 (default: any)")
	eventLog := fs.String("log", env("ZTPD_LOG", "ztpd-events.jsonl"), "event log file (replayed on start)")
	once := fs.Bool("once", os.Getenv("ZTPD_ONCE") == "1", "serve a config only until the switch reports success")
	fs.Parse(args)

	var nets []netip.Prefix
	for _, a := range strings.Split(*allow, ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		p, err := netip.ParsePrefix(a)
		if err != nil {
			log.Fatalf("-allow %q: %v", a, err)
		}
		nets = append(nets, p.Masked())
	}
	store, err := server.OpenStore(*eventLog)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	web, _ := fs_sub(webFS, "web")
	srv, err := server.New(server.Options{
		Inventory: c.inventory, Devices: c.devices, Templates: c.templates,
		URL: *url, Allow: nets, Token: os.Getenv("ZTPD_TOKEN"), Once: *once, Web: web,
	}, store)
	if err != nil {
		log.Fatal(err)
	}

	// kill -HUP reloads the sheet and templates, same as the dashboard button
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := srv.Reload(); err != nil {
				log.Printf("reload: %v", err)
			}
		}
	}()

	h := &http.Server{Addr: *listen, Handler: logRequests(srv.Handler()), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("ztpd %s listening on %s", version, *listen)
	log.Fatal(h.ListenAndServe())
}

func fs_sub(f embed.FS, dir string) (fs.FS, error) { return fs.Sub(f, dir) }

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ztp/") {
			log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}

func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var c common
	c.flags(fs)
	fs.Parse(args)
	i, err := inv.Load(c.inventory, c.devices)
	bad := 0
	var probs *inv.Problems
	if errors.As(err, &probs) {
		for _, p := range probs.List {
			fmt.Println("sheet:", p)
		}
		bad += len(probs.List)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r := render.New(c.templates)
	for _, d := range i.Sorted() {
		cfg, err := r.Render(d)
		if err != nil {
			fmt.Printf("FAIL %-14s %-12s %v\n", d.Hostname, d.Serial, err)
			bad++
			continue
		}
		fmt.Printf("ok   %-14s %-12s %-16s %4d lines\n", d.Hostname, d.Serial, d.Template, strings.Count(cfg, "\n"))
	}
	if bad > 0 {
		fmt.Printf("\n%d problem(s)\n", bad)
		return 1
	}
	fmt.Printf("\nall %d devices render\n", len(i.Devices))
	return 0
}

func renderOne(args []string) {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	var c common
	c.flags(fs)
	serial := fs.String("serial", "", "serial number (or hostname)")
	fs.Parse(args)
	i, err := inv.Load(c.inventory, c.devices)
	var probs *inv.Problems
	if err != nil && !errors.As(err, &probs) {
		log.Fatal(err)
	}
	d, ok := i.Lookup(*serial)
	if !ok {
		for _, x := range i.Devices {
			if strings.EqualFold(x.Hostname, *serial) {
				d, ok = x, true
			}
		}
	}
	if !ok {
		log.Fatalf("%q is not in %s", *serial, c.devices)
	}
	cfg, err := render.New(c.templates).Render(d)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(cfg)
}
