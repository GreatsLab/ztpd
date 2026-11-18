package server

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// Stages a device moves through. A switch that stops at "config-sent" and
// never reports back is the interesting case: it downloaded its config and
// then something on the box went wrong.
const (
	StageWaiting      = "waiting"      // in the sheet, not seen yet
	StageBootstrapped = "bootstrapped" // ran our script
	StageConfigSent   = "config-sent"
	StageApplied      = "applied"
	StageFailed       = "failed"
	StageUnknown      = "unknown" // asked for a config, not in the sheet
)

type Event struct {
	Time     time.Time `json:"time"`
	Serial   string    `json:"serial,omitempty"`
	Hostname string    `json:"hostname,omitempty"`
	Kind     string    `json:"kind"` // script | config | report | unknown | denied | rearm
	Step     string    `json:"step,omitempty"`
	OK       bool      `json:"ok"`
	Msg      string    `json:"msg,omitempty"`
	Remote   string    `json:"remote,omitempty"`
	Hash     string    `json:"hash,omitempty"` // sha256 of the config served, first 12 chars
}

type DeviceState struct {
	Serial   string    `json:"serial"`
	Stage    string    `json:"stage"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	Remote   string    `json:"remote,omitempty"`
	Hash     string    `json:"hash,omitempty"`
	Msg      string    `json:"msg,omitempty"`
	Attempts int       `json:"attempts"`
}

// Store keeps the event log in memory and appends it to a JSONL file, which
// is replayed at start so a restart doesn't forget which switches are done.
type Store struct {
	mu     sync.Mutex
	events []Event
	state  map[string]*DeviceState
	f      *os.File
	max    int
}

func OpenStore(path string) (*Store, error) {
	s := &Store{state: map[string]*DeviceState{}, max: 5000}
	if path == "" {
		return s, nil
	}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				s.apply(e)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

func (s *Store) Close() error {
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}

func (s *Store) Add(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apply(e)
	if s.f != nil {
		b, _ := json.Marshal(e)
		s.f.Write(append(b, '\n'))
	}
}

// apply updates the per-device state for one event. Called with mu held
// (or during replay, before anyone else can see the store).
func (s *Store) apply(e Event) {
	s.events = append(s.events, e)
	if len(s.events) > s.max {
		s.events = append([]Event(nil), s.events[len(s.events)-s.max:]...)
	}
	if e.Serial == "" {
		return
	}
	d := s.state[e.Serial]
	if d == nil {
		d = &DeviceState{Serial: e.Serial}
		s.state[e.Serial] = d
	}
	d.LastSeen = e.Time
	if e.Remote != "" {
		d.Remote = e.Remote
	}
	switch e.Kind {
	case "config":
		if e.OK {
			d.Stage, d.Hash = StageConfigSent, e.Hash
			d.Attempts++
		} else {
			d.Stage, d.Msg = StageFailed, e.Msg
		}
	case "unknown":
		d.Stage, d.Msg = StageUnknown, e.Msg
	case "report":
		d.Msg = e.Msg
		switch {
		case !e.OK:
			d.Stage = StageFailed
		case e.Step == "bootstrap":
			d.Stage = StageBootstrapped
		case e.Step == "applied":
			d.Stage = StageApplied
		}
	case "rearm":
		d.Stage, d.Msg = StageWaiting, "re-armed: "+e.Msg
	}
}

func (s *Store) Get(serial string) (DeviceState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.state[serial]
	if !ok {
		return DeviceState{}, false
	}
	return *d, true
}

func (s *Store) States() map[string]DeviceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]DeviceState, len(s.state))
	for k, v := range s.state {
		out[k] = *v
	}
	return out
}

// Events returns the newest n events, newest first, optionally for one serial.
func (s *Store) Events(serial string, n int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for i := len(s.events) - 1; i >= 0 && len(out) < n; i-- {
		if serial == "" || s.events[i].Serial == serial {
			out = append(out, s.events[i])
		}
	}
	return out
}

func sortedKeys(m map[string]DeviceState) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
