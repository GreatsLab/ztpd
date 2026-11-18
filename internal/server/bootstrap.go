package server

import (
	"bytes"
	"text/template"
)

// The bootstrap script is the file DHCP option 67 points at. The switch
// downloads and runs it before it has any config, so it has to work out its
// own serial number, fetch its config from us, apply it, and tell us how it
// went. One script per platform; the serial number is what ties a box to a
// row in devices.csv.

// IOS-XE (16.5+) runs a ZTP Python script in guestshell, where the `cli`
// module talks to the switch.
var iosxe = template.Must(template.New("ios-xe").Parse(`#!/usr/bin/env python
# ztpd bootstrap for Cisco IOS-XE. Served by {{.URL}}
# DHCP option 67: {{.URL}}/ztp/bootstrap/ios-xe.py
import json, re, sys, time

import cli

try:
    from urllib.request import Request, urlopen
except ImportError:  # python 2 guestshell on older 16.x images
    from urllib2 import Request, urlopen

SERVER = "{{.URL}}"


def report(serial, step, ok=True, msg=""):
    body = json.dumps({"step": step, "ok": ok, "msg": msg[:2000]}).encode()
    try:
        urlopen(Request(SERVER + "/ztp/report/" + serial, data=body,
                        headers={"Content-Type": "application/json"}), timeout=10)
    except Exception as e:
        print("ztpd: report failed: %s" % e)


def serial_number():
    # "show license udi" works on every IOS-XE switch I've tried; "show version"
    # differs between platforms and stacks.
    out = cli.cli("show license udi")
    m = re.search(r"SN:(\w+)", out)
    if not m:
        out = cli.cli("show version")
        m = re.search(r"[Ss]ystem [Ss]erial [Nn]umber\s*:\s*(\w+)", out)
    if not m:
        print("ztpd: could not find a serial number")
        sys.exit(1)
    return m.group(1).upper()


def main():
    sn = serial_number()
    report(sn, "bootstrap", True, "ios-xe script running")
    cfg = None
    for attempt in range(5):
        try:
            cfg = urlopen(SERVER + "/ztp/config/" + sn, timeout=30).read().decode()
            break
        except Exception as e:
            err = str(e)
            # 404/409 are answers, not network trouble: stop asking
            if "404" in err or "409" in err:
                report(sn, "config", False, err)
                sys.exit(1)
            time.sleep(10)
    if cfg is None:
        report(sn, "config", False, "could not download config after 5 tries")
        sys.exit(1)

    lines = [l for l in cfg.splitlines() if l.strip() and not l.startswith("!")]
    results = cli.configure(lines)
    failed = [str(r) for r in results if not r.success]
    if failed:
        report(sn, "apply", False, "%d line(s) rejected: %s" % (len(failed), "; ".join(failed[:10])))
        sys.exit(1)
    cli.executep("copy running-config startup-config")
    report(sn, "applied", True, "%d lines applied, saved to startup-config" % len(lines))


main()
`))

// EOS runs the downloaded file as a script; writing /mnt/flash/startup-config
// and exiting 0 makes the switch reboot into that config.
var eos = template.Must(template.New("eos").Parse(`#!/bin/bash
# ztpd bootstrap for Arista EOS. Served by {{.URL}}
# DHCP option 67: {{.URL}}/ztp/bootstrap/eos.sh
SERVER="{{.URL}}"
SN=$(FastCli -p 15 -c "show version" | awk -F': *' '/^Serial number/ {print toupper($2)}' | tr -d '\r ')
if [ -z "$SN" ]; then echo "ztpd: no serial number"; exit 1; fi

report() {
  # $1 step, $2 true/false, $3 message (quotes stripped so the JSON stays valid)
  msg=$(printf '%s' "$3" | tr -d '"\\' | head -c 2000)
  curl -s -m 10 -H 'Content-Type: application/json' \
    -d "{\"step\":\"$1\",\"ok\":$2,\"msg\":\"$msg\"}" "$SERVER/ztp/report/$SN" >/dev/null || true
}

report bootstrap true "eos script running"
code=$(curl -s -m 30 --retry 5 --retry-delay 10 -o /tmp/ztpd.cfg -w '%{http_code}' "$SERVER/ztp/config/$SN")
if [ "$code" != "200" ]; then
  report config false "HTTP $code: $(head -c 300 /tmp/ztpd.cfg 2>/dev/null)"
  exit 1
fi
cp /tmp/ztpd.cfg /mnt/flash/startup-config
report applied true "startup-config written ($(wc -l < /tmp/ztpd.cfg) lines), rebooting"
exit 0
`))

var bootstraps = map[string]*template.Template{"ios-xe": iosxe, "eos": eos}

func Bootstrap(platform, url string) ([]byte, bool) {
	t, ok := bootstraps[platform]
	if !ok {
		return nil, false
	}
	var b bytes.Buffer
	t.Execute(&b, map[string]string{"URL": url})
	return b.Bytes(), true
}
