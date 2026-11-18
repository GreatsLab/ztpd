package inv

import (
	"errors"
	"strings"
	"testing"
)

var file = File{
	Defaults: map[string]any{"ntp": "10.0.0.1", "domain": "lab.local"},
	Sites: map[string]Site{
		"lagos": {Vars: map[string]any{"ntp": "10.20.0.1", "syslog": "10.20.0.5"}},
	},
	Roles: map[string]Role{
		"access": {Template: "access-ios.tmpl", Platform: "ios-xe", Vars: map[string]any{"mgmt_vlan": "99", "ntp": "role-loses"}},
	},
}

func TestMergeOrder(t *testing.T) {
	rows, err := ParseCSV(strings.NewReader("Serial,Hostname,Role,Site,Mgmt IP\nfoc1234x0ab,lag-acc-01,access,lagos,10.20.99.11/24\n"))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Build(file, rows)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := inv.Lookup(" FOC1234X0AB ")
	if !ok {
		t.Fatal("lookup should be case/space insensitive")
	}
	if d.Vars["ntp"] != "10.20.0.1" {
		t.Errorf("site should beat role and defaults, got ntp=%v", d.Vars["ntp"])
	}
	if d.Vars["domain"] != "lab.local" || d.Vars["mgmt_vlan"] != "99" {
		t.Errorf("defaults/role vars missing: %v", d.Vars)
	}
	if d.Vars["mgmt_ip"] != "10.20.99.11/24" {
		t.Errorf("'Mgmt IP' header should become mgmt_ip, vars=%v", d.Vars)
	}
	if d.Platform != "ios-xe" || d.Template != "access-ios.tmpl" {
		t.Errorf("role not applied: %+v", d)
	}
}

func TestSheetProblems(t *testing.T) {
	sheet := `serial,hostname,role,site,mgmt_ip
# comment lines are fine
AAA1,sw1,access,lagos,10.0.0.1/24
AAA1,sw2,access,lagos,10.0.0.2/24
BBB2,SW1,access,lagos,10.0.0.1/24
CCC3,sw3,core,lagos,
DDD4,sw4,access,abuja,
EEE5,sw5,access,lagos,10.0.0.9
,,,,
`
	rows, err := ParseCSV(strings.NewReader(sheet))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(file, rows)
	var p *Problems
	if !errors.As(err, &p) {
		t.Fatalf("want *Problems, got %v", err)
	}
	want := []string{
		"serial AAA1 appears twice",
		"hostname SW1 is also used by AAA1",
		"mgmt_ip 10.0.0.1 is also used by AAA1",
		`role "core"`,
		`site "abuja"`,
		`mgmt_ip "10.0.0.9" should look like`,
	}
	all := strings.Join(p.List, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("missing problem %q in:\n%s", w, all)
		}
	}
	if len(p.List) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(p.List), len(want), all)
	}
}

func TestSheetNeedsColumns(t *testing.T) {
	_, err := ParseCSV(strings.NewReader("serial,name\nX,y\n"))
	if err == nil || !strings.Contains(err.Error(), `"hostname"`) {
		t.Fatalf("want missing hostname column error, got %v", err)
	}
}
