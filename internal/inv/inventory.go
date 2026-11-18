// Package inv loads the device inventory.
//
// Two files, because that's how the information arrives in real life:
//
//   - inventory.json: sites, roles and their default variables, written by
//     the network engineer
//   - devices.csv: serial, hostname, role, site, mgmt IP... usually pasted
//     straight from the procurement / rack-and-stack sheet
//
// A device's variables are merged in this order, later wins:
// global defaults < role < site < the device's own columns.
package inv

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

type Site struct {
	Vars map[string]any `json:"vars"`
}

type Role struct {
	Template string         `json:"template"` // file name in the templates dir
	Platform string         `json:"platform"` // ios-xe | eos | nxos (picks the bootstrap script)
	Vars     map[string]any `json:"vars"`
}

type File struct {
	Defaults map[string]any  `json:"defaults"`
	Sites    map[string]Site `json:"sites"`
	Roles    map[string]Role `json:"roles"`
}

type Device struct {
	Serial   string         `json:"serial"`
	Hostname string         `json:"hostname"`
	Role     string         `json:"role"`
	Site     string         `json:"site"`
	Platform string         `json:"platform"`
	Template string         `json:"template"`
	Vars     map[string]any `json:"vars"`
}

type Inventory struct {
	File
	Devices map[string]*Device // by serial, upper case
}

func Load(invPath, devicesPath string) (*Inventory, error) {
	var f File
	b, err := os.ReadFile(invPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", invPath, err)
	}
	fh, err := os.Open(devicesPath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	devs, err := ParseCSV(fh)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", devicesPath, err)
	}
	return Build(f, devs)
}

// ParseCSV reads the device sheet. Required columns: serial, hostname, role.
// Every other column becomes a variable with the header as its name, so
// adding "mgmt_ip" or "uplink_port" to the sheet makes {{.mgmt_ip}} work
// in templates with no code change.
func ParseCSV(r io.Reader) ([]map[string]string, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.Comment = '#'
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty device sheet")
	}
	head := make([]string, len(rows[0]))
	for i, h := range rows[0] {
		head[i] = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(h), " ", "_"))
	}
	for _, need := range []string{"serial", "hostname", "role"} {
		if !contains(head, need) {
			return nil, fmt.Errorf("device sheet needs a %q column (have: %s)", need, strings.Join(head, ", "))
		}
	}
	var out []map[string]string
	for n, r := range rows[1:] {
		m := map[string]string{"_line": fmt.Sprint(n + 2)}
		empty := true
		for i, v := range r {
			if i < len(head) {
				m[head[i]] = strings.TrimSpace(v)
				if m[head[i]] != "" {
					empty = false
				}
			}
		}
		if !empty {
			out = append(out, m)
		}
	}
	return out, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Build merges the sheet with sites/roles and checks it for the mistakes
// that would otherwise only show up when a switch is sitting in a rack
// half-configured.
func Build(f File, rows []map[string]string) (*Inventory, error) {
	inv := &Inventory{File: f, Devices: map[string]*Device{}}
	var problems []string
	hostnames := map[string]string{}
	ips := map[string]string{}
	for _, r := range rows {
		line := r["_line"]
		serial := strings.ToUpper(r["serial"])
		if serial == "" {
			problems = append(problems, "line "+line+": no serial")
			continue
		}
		if _, dup := inv.Devices[serial]; dup {
			problems = append(problems, "line "+line+": serial "+serial+" appears twice")
			continue
		}
		if r["hostname"] == "" {
			problems = append(problems, "line "+line+": "+serial+" has no hostname")
			continue
		}
		role, ok := f.Roles[r["role"]]
		if !ok {
			problems = append(problems, fmt.Sprintf("line %s: %s has role %q, which isn't in the inventory (roles: %s)", line, serial, r["role"], keys(f.Roles)))
			continue
		}
		site, ok := f.Sites[r["site"]]
		if r["site"] != "" && !ok {
			problems = append(problems, fmt.Sprintf("line %s: %s is at site %q, which isn't in the inventory", line, serial, r["site"]))
			continue
		}
		vars := map[string]any{}
		for _, src := range []map[string]any{f.Defaults, role.Vars, site.Vars} {
			for k, v := range src {
				vars[k] = v
			}
		}
		for k, v := range r {
			if k != "_line" && v != "" {
				vars[k] = v
			}
		}
		vars["serial"] = serial
		d := &Device{Serial: serial, Hostname: r["hostname"], Role: r["role"], Site: r["site"],
			Platform: role.Platform, Template: role.Template, Vars: vars}
		if other, dup := hostnames[strings.ToLower(d.Hostname)]; dup {
			problems = append(problems, fmt.Sprintf("line %s: hostname %s is also used by %s", line, d.Hostname, other))
		}
		hostnames[strings.ToLower(d.Hostname)] = serial
		if ip, ok := vars["mgmt_ip"].(string); ok && ip != "" {
			p, err := netip.ParsePrefix(ip)
			if err != nil {
				problems = append(problems, fmt.Sprintf("line %s: mgmt_ip %q should look like 10.0.0.5/24", line, ip))
			} else {
				if other, dup := ips[p.Addr().String()]; dup {
					problems = append(problems, fmt.Sprintf("line %s: mgmt_ip %s is also used by %s", line, p.Addr(), other))
				}
				ips[p.Addr().String()] = serial
			}
		}
		inv.Devices[serial] = d
	}
	if len(problems) > 0 {
		return inv, &Problems{problems}
	}
	return inv, nil
}

type Problems struct{ List []string }

func (p *Problems) Error() string {
	return fmt.Sprintf("%d problem(s) in the inventory:\n  - %s", len(p.List), strings.Join(p.List, "\n  - "))
}

func keys[V any](m map[string]V) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ", ")
}

func (inv *Inventory) Lookup(serial string) (*Device, bool) {
	d, ok := inv.Devices[strings.ToUpper(strings.TrimSpace(serial))]
	return d, ok
}

func (inv *Inventory) Sorted() []*Device {
	out := make([]*Device, 0, len(inv.Devices))
	for _, d := range inv.Devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}
