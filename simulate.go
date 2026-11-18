package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// simulate does what the bootstrap scripts do on a real switch, so the
// whole flow can be tested without hardware: fetch the script, report in,
// fetch the config, report the result.
func simulate(args []string) {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	srv := fs.String("server", "http://127.0.0.1:8080", "ztpd server")
	serial := fs.String("serial", "", "one serial to provision (default: every waiting device)")
	platform := fs.String("platform", "", "platform for -serial (ios-xe or eos)")
	fail := fs.String("fail", "", "comma-separated serials that should fail to apply their config")
	unknown := fs.Int("unknown", 0, "also boot this many switches that aren't in the sheet")
	spread := fs.Duration("spread", 0, "spread the boots over this long, like a real rollout")
	fs.Parse(args)

	type dev struct{ Serial, Platform, Stage string }
	var devs []dev
	if *serial != "" {
		devs = []dev{{strings.ToUpper(*serial), *platform, ""}}
	} else {
		resp, err := http.Get(*srv + "/api/devices")
		if err != nil {
			log.Fatal(err)
		}
		var all []dev
		json.NewDecoder(resp.Body).Decode(&all)
		resp.Body.Close()
		for _, d := range all {
			if d.Stage == "waiting" {
				devs = append(devs, d)
			}
		}
	}
	for i := 0; i < *unknown; i++ {
		devs = append(devs, dev{fmt.Sprintf("FOC9999Z%03d", rand.Intn(1000)), "ios-xe", ""})
	}
	failing := map[string]bool{}
	for _, f := range strings.Split(*fail, ",") {
		failing[strings.ToUpper(strings.TrimSpace(f))] = true
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	bad := 0
	for _, d := range devs {
		wg.Add(1)
		go func(d dev) {
			defer wg.Done()
			if *spread > 0 {
				time.Sleep(time.Duration(rand.Int63n(int64(*spread))))
			}
			err := boot(*srv, d.Serial, d.Platform, failing[d.Serial])
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bad++
				fmt.Printf("%-12s %v\n", d.Serial, err)
			} else {
				fmt.Printf("%-12s provisioned\n", d.Serial)
			}
		}(d)
	}
	wg.Wait()
	if bad > 0 {
		os.Exit(1)
	}
}

func boot(srv, serial, platform string, fail bool) error {
	file := map[string]string{"ios-xe": "ios-xe.py", "eos": "eos.sh"}[platform]
	if file == "" {
		file = "ios-xe.py"
	}
	b, code, err := get(srv + "/ztp/bootstrap/" + file)
	if err != nil || code != 200 || !bytes.HasPrefix(b, []byte("#!")) {
		return fmt.Errorf("bootstrap: HTTP %d %v", code, err)
	}
	post(srv, serial, "bootstrap", true, platform+" script running (simulated)")
	time.Sleep(time.Duration(200+rand.Intn(600)) * time.Millisecond)

	cfg, code, err := get(srv + "/ztp/config/" + serial)
	if err != nil {
		return err
	}
	if code != 200 {
		msg := fmt.Sprintf("HTTP %d: %s", code, strings.TrimSpace(string(cfg)))
		post(srv, serial, "config", false, msg)
		return fmt.Errorf("config: %s", msg)
	}
	lines := 0
	for _, l := range strings.Split(string(cfg), "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "!") {
			lines++
		}
	}
	time.Sleep(time.Duration(300+rand.Intn(900)) * time.Millisecond)
	if fail {
		msg := "1 line(s) rejected: switchport voice vlan 20 -> % Invalid input detected at '^' marker."
		post(srv, serial, "apply", false, msg)
		return fmt.Errorf("apply: %s", msg)
	}
	post(srv, serial, "applied", true, fmt.Sprintf("%d lines applied, saved to startup-config", lines))
	return nil
}

func get(url string) ([]byte, int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func post(srv, serial, step string, ok bool, msg string) {
	body, _ := json.Marshal(map[string]any{"step": step, "ok": ok, "msg": msg})
	resp, err := http.Post(srv+"/ztp/report/"+serial, "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}
