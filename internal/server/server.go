// Package server is the HTTP side of ztpd: what the switches talk to
// (/ztp/...) and what the engineer watches (/api/... and the dashboard).
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GreatsLab/ztpd/internal/inv"
	"github.com/GreatsLab/ztpd/internal/render"
)

type Options struct {
	Inventory string // inventory.json
	Devices   string // devices.csv
	Templates string // dir of *.tmpl
	URL       string // how switches reach us, e.g. http://10.0.0.10:8080 (default: from the Host header)
	Allow     []netip.Prefix
	Token     string // for the dashboard's write actions and config previews
	Once      bool   // refuse to serve a config again after a switch reported success
	Web       fs.FS
	Logf      func(string, ...any)
}

type Server struct {
	opt   Options
	store *Store
	rend  *render.Renderer

	mu       sync.RWMutex
	inv      *inv.Inventory
	problems []string
	checks   map[string]string // serial -> render error, "" if it renders
	loaded   time.Time
}

func New(opt Options, store *Store) (*Server, error) {
	if opt.Logf == nil {
		opt.Logf = log.Printf
	}
	s := &Server{opt: opt, store: store, rend: render.New(opt.Templates)}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the inventory and templates. A sheet with bad rows still
// loads (the bad rows are left out and listed on the dashboard), because one
// typo shouldn't stop the other forty switches from provisioning. A file that
// doesn't parse at all keeps the previous inventory.
func (s *Server) Reload() error {
	i, err := inv.Load(s.opt.Inventory, s.opt.Devices)
	var probs *inv.Problems
	var list []string
	switch {
	case errors.As(err, &probs):
		list = probs.List
	case err != nil:
		return err
	}
	s.rend.Reload()
	checks := map[string]string{}
	for _, d := range i.Devices {
		if _, err := s.rend.Render(d); err != nil {
			checks[d.Serial] = err.Error()
		} else {
			checks[d.Serial] = ""
		}
	}
	s.mu.Lock()
	s.inv, s.problems, s.checks, s.loaded = i, list, checks, time.Now().UTC()
	s.mu.Unlock()
	bad := 0
	for _, c := range checks {
		if c != "" {
			bad++
		}
	}
	s.opt.Logf("inventory: %d devices, %d sheet problem(s), %d device(s) that don't render", len(i.Devices), len(list), bad)
	return nil
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /ztp/bootstrap/{file}", s.allowed(s.bootstrap))
	m.HandleFunc("GET /ztp/config/{serial}", s.allowed(s.config))
	m.HandleFunc("POST /ztp/report/{serial}", s.allowed(s.report))

	m.HandleFunc("GET /api/summary", s.summary)
	m.HandleFunc("GET /api/devices", s.devices)
	m.HandleFunc("GET /api/events", s.events)
	m.HandleFunc("GET /api/devices/{serial}/config", s.auth(s.preview))
	m.HandleFunc("POST /api/devices/{serial}/rearm", s.auth(s.rearm))
	m.HandleFunc("POST /api/reload", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Reload(); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		s.summary(w, r)
	}))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	if s.opt.Web != nil {
		m.Handle("GET /", http.FileServerFS(s.opt.Web))
	}
	return m
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// allowed limits the /ztp endpoints to the provisioning networks. Configs
// have enable secrets and SNMP communities in them; "anyone who can reach
// port 8080" is not the right audience.
func (s *Server) allowed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.opt.Allow) > 0 {
			a, err := netip.ParseAddr(remoteIP(r))
			ok := false
			if err == nil {
				a = a.Unmap()
				for _, p := range s.opt.Allow {
					if p.Contains(a) {
						ok = true
						break
					}
				}
			}
			if !ok {
				s.store.Add(Event{Kind: "denied", Serial: strings.ToUpper(r.PathValue("serial")), Remote: remoteIP(r),
					Msg: r.Method + " " + r.URL.Path + " from outside the allowed networks"})
				httpErr(w, 403, "not from a provisioning network")
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opt.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.Token)) != 1 {
				httpErr(w, 401, "needs Authorization: Bearer <token>")
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) baseURL(r *http.Request) string {
	if s.opt.URL != "" {
		return strings.TrimRight(s.opt.URL, "/")
	}
	return "http://" + r.Host
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	platform := strings.TrimSuffix(strings.TrimSuffix(file, ".py"), ".sh")
	b, ok := Bootstrap(platform, s.baseURL(r))
	if !ok {
		httpErr(w, 404, "no bootstrap for "+file+" (have ios-xe.py, eos.sh)")
		return
	}
	s.store.Add(Event{Kind: "script", OK: true, Remote: remoteIP(r), Msg: "downloaded " + file})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	serial := strings.ToUpper(strings.TrimSpace(r.PathValue("serial")))
	s.mu.RLock()
	d, ok := s.inv.Lookup(serial)
	s.mu.RUnlock()
	ev := Event{Kind: "config", Serial: serial, Remote: remoteIP(r)}
	if !ok {
		ev.Kind, ev.Msg = "unknown", "asked for a config but isn't in the device sheet"
		s.store.Add(ev)
		httpErr(w, 404, serial+" is not in the device sheet")
		return
	}
	ev.Hostname = d.Hostname
	if st, seen := s.store.Get(serial); s.opt.Once && seen && st.Stage == StageApplied {
		ev.Kind, ev.OK = "denied", false
		ev.Msg = "already provisioned at " + st.LastSeen.Format(time.RFC3339) + "; re-arm it to serve the config again"
		s.store.Add(ev)
		httpErr(w, 409, ev.Msg)
		return
	}
	cfg, err := s.rend.Render(d)
	if err != nil {
		ev.Msg = err.Error()
		s.store.Add(ev)
		httpErr(w, 500, "config for "+serial+" doesn't render: "+err.Error())
		return
	}
	sum := sha256.Sum256([]byte(cfg))
	ev.OK, ev.Hash = true, hex.EncodeToString(sum[:])[:12]
	ev.Msg = fmt.Sprintf("%s config, %d lines", d.Template, strings.Count(cfg, "\n"))
	s.store.Add(ev)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Config-Hash", ev.Hash)
	io.WriteString(w, cfg)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	serial := strings.ToUpper(strings.TrimSpace(r.PathValue("serial")))
	var body struct {
		Step string `json:"step"`
		OK   bool   `json:"ok"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		httpErr(w, 400, "body should be {\"step\":..,\"ok\":..,\"msg\":..}: "+err.Error())
		return
	}
	ev := Event{Kind: "report", Serial: serial, Step: body.Step, OK: body.OK, Msg: body.Msg, Remote: remoteIP(r)}
	s.mu.RLock()
	if d, ok := s.inv.Lookup(serial); ok {
		ev.Hostname = d.Hostname
	}
	s.mu.RUnlock()
	s.store.Add(ev)
	if !body.OK {
		s.opt.Logf("%s (%s) failed at %s: %s", serial, ev.Hostname, body.Step, body.Msg)
	}
	w.WriteHeader(204)
}

type deviceRow struct {
	Serial      string    `json:"serial"`
	Hostname    string    `json:"hostname,omitempty"`
	Role        string    `json:"role,omitempty"`
	Site        string    `json:"site,omitempty"`
	Platform    string    `json:"platform,omitempty"`
	MgmtIP      string    `json:"mgmt_ip,omitempty"`
	Stage       string    `json:"stage"`
	LastSeen    time.Time `json:"last_seen,omitzero"`
	Remote      string    `json:"remote,omitempty"`
	Hash        string    `json:"hash,omitempty"`
	Msg         string    `json:"msg,omitempty"`
	Attempts    int       `json:"attempts"`
	RenderError string    `json:"render_error,omitempty"`
}

func (s *Server) rows() []deviceRow {
	states := s.store.States()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []deviceRow
	for _, d := range s.inv.Sorted() {
		row := deviceRow{Serial: d.Serial, Hostname: d.Hostname, Role: d.Role, Site: d.Site,
			Platform: d.Platform, Stage: StageWaiting, RenderError: s.checks[d.Serial]}
		if ip, ok := d.Vars["mgmt_ip"].(string); ok {
			row.MgmtIP = ip
		}
		if st, ok := states[d.Serial]; ok {
			row.Stage, row.LastSeen, row.Remote, row.Hash, row.Msg, row.Attempts =
				st.Stage, st.LastSeen, st.Remote, st.Hash, st.Msg, st.Attempts
			if row.Stage == StageUnknown { // it was unknown, then got added to the sheet
				row.Stage = StageWaiting
			}
		}
		out = append(out, row)
	}
	// boxes that asked for a config but aren't in the sheet: usually a typo in
	// the serial, or a switch that was racked in the wrong place. Whatever
	// they reported after the 404 (the bootstrap script sends "config
	// failed"), they're still unknown until someone adds them.
	for _, k := range sortedKeys(states) {
		st := states[k]
		if _, known := s.inv.Lookup(k); !known {
			out = append(out, deviceRow{Serial: k, Stage: StageUnknown, LastSeen: st.LastSeen, Remote: st.Remote, Msg: st.Msg})
		}
	}
	return out
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.rows())
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	broken := 0
	for _, d := range s.rows() {
		counts[d.Stage]++
		if d.RenderError != "" {
			broken++
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, map[string]any{
		"stages":        counts,
		"total":         len(s.inv.Devices),
		"problems":      s.problems,
		"not_rendering": broken,
		"loaded":        s.loaded,
		"bootstrap": map[string]string{
			"ios-xe": s.baseURL(r) + "/ztp/bootstrap/ios-xe.py",
			"eos":    s.baseURL(r) + "/ztp/bootstrap/eos.sh",
		},
		"once":        s.opt.Once,
		"needs_token": s.opt.Token != "",
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	ev := s.store.Events(strings.ToUpper(r.URL.Query().Get("serial")), n)
	if ev == nil {
		ev = []Event{}
	}
	writeJSON(w, ev)
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	serial := strings.ToUpper(r.PathValue("serial"))
	s.mu.RLock()
	d, ok := s.inv.Lookup(serial)
	s.mu.RUnlock()
	if !ok {
		httpErr(w, 404, serial+" is not in the device sheet")
		return
	}
	cfg, err := s.rend.Render(d)
	if err != nil {
		httpErr(w, 422, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, cfg)
}

func (s *Server) rearm(w http.ResponseWriter, r *http.Request) {
	serial := strings.ToUpper(r.PathValue("serial"))
	who := r.Header.Get("X-Actor")
	if who == "" {
		who = remoteIP(r)
	}
	s.store.Add(Event{Kind: "rearm", Serial: serial, OK: true, Msg: "by " + who, Remote: remoteIP(r)})
	w.WriteHeader(204)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}
