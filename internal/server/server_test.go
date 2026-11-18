package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T, mod func(*Options)) (*httptest.Server, *Server, string) {
	t.Helper()
	d := t.TempDir()
	write := func(name, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(d, name)), 0o755)
		if err := os.WriteFile(filepath.Join(d, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("inventory.json", `{"defaults":{"ntp":"10.0.0.1"},"sites":{"lab":{}},
	  "roles":{"access":{"template":"a.tmpl","platform":"ios-xe"},"broken":{"template":"b.tmpl","platform":"eos"}}}`)
	write("devices.csv", "serial,hostname,role,site,mgmt_ip\nSW1,sw1,access,lab,10.9.0.11/24\nSW2,sw2,access,lab,10.9.0.12/24\nSW3,sw3,broken,lab,10.9.0.13/24\n")
	write("t/a.tmpl", "hostname {{.hostname}}\nntp server {{.ntp}}\ninterface Vlan99\n ip address {{ip .mgmt_ip}} {{mask .mgmt_ip}}\n")
	write("t/b.tmpl", "hostname {{.hostname}}\nsnmp-server location {{.rack}}\n")
	opt := Options{Inventory: filepath.Join(d, "inventory.json"), Devices: filepath.Join(d, "devices.csv"),
		Templates: filepath.Join(d, "t"), Logf: func(string, ...any) {}}
	if mod != nil {
		mod(&opt)
	}
	store, err := OpenStore(filepath.Join(d, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s, err := New(opt, store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s, d
}

func do(t *testing.T, method, url, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func stage(t *testing.T, ts *httptest.Server, serial string) string {
	t.Helper()
	_, body := do(t, "GET", ts.URL+"/api/devices", "")
	var rows []deviceRow
	json.Unmarshal([]byte(body), &rows)
	for _, r := range rows {
		if r.Serial == serial {
			return r.Stage
		}
	}
	return "(missing)"
}

func TestFullProvisioningFlow(t *testing.T) {
	ts, _, _ := setup(t, nil)

	code, script := do(t, "GET", ts.URL+"/ztp/bootstrap/ios-xe.py", "")
	if code != 200 || !strings.Contains(script, `SERVER = "`+ts.URL+`"`) {
		t.Fatalf("bootstrap should have our URL baked in, got %d:\n%.300s", code, script)
	}
	if stage(t, ts, "SW1") != StageWaiting {
		t.Fatal("SW1 should start waiting")
	}
	do(t, "POST", ts.URL+"/ztp/report/SW1", `{"step":"bootstrap","ok":true}`)
	if s := stage(t, ts, "SW1"); s != StageBootstrapped {
		t.Fatalf("after bootstrap report: %s", s)
	}
	code, cfg := do(t, "GET", ts.URL+"/ztp/config/sw1", "") // lower case on purpose
	if code != 200 || !strings.Contains(cfg, "ip address 10.9.0.11 255.255.255.0") {
		t.Fatalf("config: %d\n%s", code, cfg)
	}
	if s := stage(t, ts, "SW1"); s != StageConfigSent {
		t.Fatalf("after config: %s", s)
	}
	do(t, "POST", ts.URL+"/ztp/report/SW1", `{"step":"applied","ok":true,"msg":"4 lines"}`)
	if s := stage(t, ts, "SW1"); s != StageApplied {
		t.Fatalf("after applied: %s", s)
	}
	_, ev := do(t, "GET", ts.URL+"/api/events?serial=sw1", "")
	if !strings.Contains(ev, `"hash"`) || !strings.Contains(ev, `"hostname": "sw1"`) {
		t.Errorf("events should carry hostname and config hash:\n%s", ev)
	}
}

func TestUnknownAndBroken(t *testing.T) {
	ts, _, _ := setup(t, nil)
	code, _ := do(t, "GET", ts.URL+"/ztp/config/NOPE123", "")
	if code != 404 || stage(t, ts, "NOPE123") != StageUnknown {
		t.Fatalf("unknown serial: %d, stage %s", code, stage(t, ts, "NOPE123"))
	}
	code, body := do(t, "GET", ts.URL+"/ztp/config/SW3", "")
	if code != 500 || !strings.Contains(body, "rack") {
		t.Fatalf("SW3's template needs .rack, want a 500 naming it, got %d %s", code, body)
	}
	_, sum := do(t, "GET", ts.URL+"/api/summary", "")
	if !strings.Contains(sum, `"not_rendering": 1`) {
		t.Errorf("summary should count SW3 as not rendering before it ever boots:\n%s", sum)
	}
}

func TestFailedApply(t *testing.T) {
	ts, _, _ := setup(t, nil)
	do(t, "GET", ts.URL+"/ztp/config/SW2", "")
	do(t, "POST", ts.URL+"/ztp/report/SW2", `{"step":"apply","ok":false,"msg":"% Invalid input"}`)
	if s := stage(t, ts, "SW2"); s != StageFailed {
		t.Fatalf("stage %s", s)
	}
	if code, _ := do(t, "POST", ts.URL+"/ztp/report/SW2", `not json`); code != 400 {
		t.Errorf("bad report body: %d", code)
	}
}

func TestOnceAndRearm(t *testing.T) {
	ts, _, _ := setup(t, func(o *Options) { o.Once, o.Token = true, "s3cret" })
	do(t, "GET", ts.URL+"/ztp/config/SW1", "")
	do(t, "POST", ts.URL+"/ztp/report/SW1", `{"step":"applied","ok":true}`)
	if code, body := do(t, "GET", ts.URL+"/ztp/config/SW1", ""); code != 409 || !strings.Contains(body, "re-arm") {
		t.Fatalf("second fetch with -once: %d %s", code, body)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/devices/SW1/rearm", ""); code != 401 {
		t.Fatalf("rearm without token: %d", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/devices/SW1/rearm", "", "Authorization", "Bearer s3cret"); code != 204 {
		t.Fatalf("rearm with token: %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/ztp/config/SW1", ""); code != 200 {
		t.Fatalf("after rearm: %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/devices/SW1/config", ""); code != 401 {
		t.Errorf("config preview has secrets in it, should need the token: %d", code)
	}
}

func TestAllowList(t *testing.T) {
	ts, _, _ := setup(t, func(o *Options) { o.Allow = []netip.Prefix{netip.MustParsePrefix("10.20.99.0/24")} })
	if code, _ := do(t, "GET", ts.URL+"/ztp/config/SW1", ""); code != 403 {
		t.Fatalf("127.0.0.1 is outside the allow list: %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/devices", ""); code != 200 {
		t.Fatalf("dashboard API isn't limited by -allow: %d", code)
	}
}

func TestReplayAfterRestart(t *testing.T) {
	ts, _, d := setup(t, nil)
	do(t, "GET", ts.URL+"/ztp/config/SW1", "")
	do(t, "POST", ts.URL+"/ztp/report/SW1", `{"step":"applied","ok":true}`)

	store, err := OpenStore(filepath.Join(d, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	st, ok := store.Get("SW1")
	if !ok || st.Stage != StageApplied || st.Attempts != 1 {
		t.Fatalf("after replay: %+v", st)
	}
}

func TestReloadPicksUpNewDevice(t *testing.T) {
	ts, s, d := setup(t, nil)
	do(t, "GET", ts.URL+"/ztp/config/NEW1", "") // racked before anyone updated the sheet
	f, _ := os.OpenFile(filepath.Join(d, "devices.csv"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("NEW1,sw9,access,lab,10.9.0.19/24\n")
	f.Close()
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if st := stage(t, ts, "NEW1"); st != StageWaiting {
		t.Fatalf("once it's in the sheet it should be waiting, not unknown: %s", st)
	}
	if code, _ := do(t, "GET", ts.URL+"/ztp/config/NEW1", ""); code != 200 {
		t.Fatalf("config after reload: %d", code)
	}
}
