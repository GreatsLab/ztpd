// Package render turns a device + template into a config, with helpers for
// the arithmetic network templates always need (masks, wildcards, nth host).
package render

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/GreatsLab/ztpd/internal/inv"
)

type Renderer struct {
	dir   string
	mu    sync.Mutex
	cache map[string]*template.Template
}

func New(dir string) *Renderer { return &Renderer{dir: dir, cache: map[string]*template.Template{}} }

// Reload drops cached templates (after you edit one).
func (r *Renderer) Reload() { r.mu.Lock(); r.cache = map[string]*template.Template{}; r.mu.Unlock() }

func (r *Renderer) get(name string) (*template.Template, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.cache[name]; ok {
		return t, nil
	}
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("bad template name %q", name)
	}
	// every file in the dir is parsed together, so templates can share
	// {{define}} blocks (a common "base" with AAA, NTP, logging)
	t := template.New(name).Funcs(Funcs).Option("missingkey=error")
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.tmpl"))
	if len(files) == 0 {
		return nil, fmt.Errorf("no templates in %s", r.dir)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if _, err := t.New(filepath.Base(f)).Parse(string(b)); err != nil {
			return nil, err
		}
	}
	if t.Lookup(name) == nil {
		return nil, fmt.Errorf("template %q not found in %s", name, r.dir)
	}
	r.cache[name] = t
	return t, nil
}

// Render returns the device's config. A template that refers to a variable
// the device doesn't have is an error, not an empty string: a config with
// "ip address  255.255.255.0" in it is worse than no config.
func (r *Renderer) Render(d *inv.Device) (string, error) {
	t, err := r.get(d.Template)
	if err != nil {
		return "", err
	}
	data := map[string]any{}
	for k, v := range d.Vars {
		data[k] = v
	}
	data["hostname"] = d.Hostname
	data["role"] = d.Role
	data["site"] = d.Site
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, d.Template, data); err != nil {
		return "", cleanErr(err)
	}
	// tidy: no trailing spaces and no blank lines. {{range}} and {{define}}
	// leave newlines behind, and "!" is the separator a config actually uses.
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if l = strings.TrimRight(l, " \t\r"); l != "" {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n", nil
}

func cleanErr(err error) error {
	s := err.Error()
	if i := strings.Index(s, "map has no entry for key "); i >= 0 {
		return fmt.Errorf("template needs %s, which this device doesn't have (add a column to the sheet, or set it in the role/site)",
			strings.TrimPrefix(s[i:], "map has no entry for key "))
	}
	return err
}

func prefix(s any) (netip.Prefix, error) {
	return netip.ParsePrefix(fmt.Sprint(s))
}

var Funcs = template.FuncMap{
	// {{ip .mgmt_ip}} "10.0.0.5/24" -> "10.0.0.5"
	"ip": func(s any) (string, error) {
		p, err := prefix(s)
		if err != nil {
			return "", err
		}
		return p.Addr().String(), nil
	},
	// {{mask .mgmt_ip}} -> "255.255.255.0"
	"mask": func(s any) (string, error) {
		p, err := prefix(s)
		if err != nil || !p.Addr().Is4() {
			return "", fmt.Errorf("mask: need an IPv4 prefix, got %v", s)
		}
		m := ^uint32(0) << (32 - p.Bits())
		if p.Bits() == 0 {
			m = 0
		}
		return fmt.Sprintf("%d.%d.%d.%d", m>>24, m>>16&255, m>>8&255, m&255), nil
	},
	// {{wildcard .mgmt_ip}} -> "0.0.0.255" (ACLs, OSPF)
	"wildcard": func(s any) (string, error) {
		p, err := prefix(s)
		if err != nil || !p.Addr().Is4() {
			return "", fmt.Errorf("wildcard: need an IPv4 prefix, got %v", s)
		}
		m := ^(^uint32(0) << (32 - p.Bits()))
		if p.Bits() == 0 {
			m = ^uint32(0)
		}
		return fmt.Sprintf("%d.%d.%d.%d", m>>24, m>>16&255, m>>8&255, m&255), nil
	},
	// {{network .mgmt_ip}} -> "10.0.0.0"
	"network": func(s any) (string, error) {
		p, err := prefix(s)
		if err != nil {
			return "", err
		}
		return p.Masked().Addr().String(), nil
	},
	// {{nthhost .mgmt_ip 1}} -> "10.0.0.1" (the usual gateway)
	"nthhost": func(s any, n int) (string, error) {
		p, err := prefix(s)
		if err != nil {
			return "", err
		}
		a := p.Masked().Addr()
		for i := 0; i < n; i++ {
			a = a.Next()
		}
		if !p.Contains(a) {
			return "", fmt.Errorf("nthhost: %d is outside %s", n, p)
		}
		return a.String(), nil
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"join": func(sep string, xs []any) string {
		s := make([]string, len(xs))
		for i, x := range xs {
			s[i] = fmt.Sprint(x)
		}
		return strings.Join(s, sep)
	},
	"split": func(sep, s string) []string { return strings.Split(s, sep) },
	"default": func(def, v any) any {
		if v == nil || v == "" {
			return def
		}
		return v
	},
	// {{env "ZTP_ENABLE_SECRET"}}: secrets come from the environment, not
	// from files in git. Unset is an error, never an empty password.
	"env": func(k string) (string, error) {
		v := os.Getenv(k)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is not set", k)
		}
		return v, nil
	},
	// {{join "," (vlanids .vlans)}} -> "10,20,30,99"
	"vlanids": func(vlans []any) []any {
		var out []any
		for _, v := range vlans {
			if m, ok := v.(map[string]any); ok {
				out = append(out, m["id"])
			}
		}
		return out
	},
	// {{range seq 1 .access_ports}} for port ranges. Takes any number type:
	// JSON numbers arrive as float64 and CSV columns as strings.
	"seq": func(a, b any) ([]int, error) {
		x, err := toInt(a)
		if err != nil {
			return nil, err
		}
		y, err := toInt(b)
		if err != nil {
			return nil, err
		}
		if y-x > 4096 {
			return nil, fmt.Errorf("seq %d %d: that's a lot of interfaces", x, y)
		}
		var o []int
		for i := x; i <= y; i++ {
			o = append(o, i)
		}
		return o, nil
	},
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("%v is not a whole number", n)
		}
		return int(n), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(n))
	}
	return 0, fmt.Errorf("%v (%T) is not a number", v, v)
}
