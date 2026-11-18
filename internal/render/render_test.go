package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GreatsLab/ztpd/internal/inv"
)

func dir(t *testing.T, files map[string]string) string {
	d := t.TempDir()
	for n, s := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestRenderWithSharedBase(t *testing.T) {
	d := dir(t, map[string]string{
		"base.tmpl": `{{define "base"}}hostname {{.hostname}}
ntp server {{.ntp}}{{end}}`,
		"access.tmpl": `{{template "base" .}}


interface Vlan{{.mgmt_vlan}}
 ip address {{ip .mgmt_ip}} {{mask .mgmt_ip}}
ip default-gateway {{nthhost .mgmt_ip 1}}
access-list 10 permit {{network .mgmt_ip}} {{wildcard .mgmt_ip}}
{{range seq 1 2}}interface Gi1/0/{{.}}
{{end}}`,
	})
	r := New(d)
	cfg, err := r.Render(&inv.Device{Hostname: "sw1", Template: "access.tmpl",
		Vars: map[string]any{"ntp": "10.0.0.1", "mgmt_vlan": "99", "mgmt_ip": "10.20.99.11/24"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `hostname sw1
ntp server 10.0.0.1
interface Vlan99
 ip address 10.20.99.11 255.255.255.0
ip default-gateway 10.20.99.1
access-list 10 permit 10.20.99.0 0.0.0.255
interface Gi1/0/1
interface Gi1/0/2
`
	if cfg != want {
		t.Errorf("got:\n%s\nwant:\n%s", cfg, want)
	}
}

func TestMissingVarIsAnError(t *testing.T) {
	d := dir(t, map[string]string{"a.tmpl": "snmp-server location {{.rack}}\n"})
	_, err := New(d).Render(&inv.Device{Hostname: "x", Template: "a.tmpl", Vars: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "rack") || !strings.Contains(err.Error(), "doesn't have") {
		t.Fatalf("want a readable missing-variable error, got %v", err)
	}
}

func TestTemplateNameIsNotAPath(t *testing.T) {
	d := dir(t, map[string]string{"a.tmpl": "x"})
	_, err := New(d).Render(&inv.Device{Template: "../../etc/passwd"})
	if err == nil {
		t.Fatal("path in template name should be refused")
	}
}

func TestMaskEdges(t *testing.T) {
	m := Funcs["mask"].(func(any) (string, error))
	w := Funcs["wildcard"].(func(any) (string, error))
	for in, want := range map[string][2]string{
		"10.0.0.1/32":   {"255.255.255.255", "0.0.0.0"},
		"10.0.0.0/31":   {"255.255.255.254", "0.0.0.1"},
		"0.0.0.0/0":     {"0.0.0.0", "255.255.255.255"},
		"172.16.0.0/12": {"255.240.0.0", "0.15.255.255"},
	} {
		if got, _ := m(in); got != want[0] {
			t.Errorf("mask %s = %s, want %s", in, got, want[0])
		}
		if got, _ := w(in); got != want[1] {
			t.Errorf("wildcard %s = %s, want %s", in, got, want[1])
		}
	}
	if _, err := Funcs["nthhost"].(func(any, int) (string, error))("10.0.0.0/30", 4); err == nil {
		t.Error("nthhost outside the prefix should fail")
	}
}

func TestSeqTakesJSONAndCSVNumbers(t *testing.T) {
	seq := Funcs["seq"].(func(any, any) ([]int, error))
	for _, b := range []any{3, float64(3), "3", " 3"} {
		got, err := seq(1, b)
		if err != nil || len(got) != 3 {
			t.Errorf("seq 1 %#v = %v, %v", b, got, err)
		}
	}
	if _, err := seq(1, 2.5); err == nil {
		t.Error("2.5 ports should be an error")
	}
}
