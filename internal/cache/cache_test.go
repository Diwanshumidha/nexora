package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func open(t *testing.T, o Options) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	o.Now = c.now
	o.SweepEvery = time.Hour // tests sweep by hand
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

func call(t *testing.T, s *Store, tool, args string) string {
	t.Helper()
	out, err := s.Call(tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s(%s): %v", tool, args, err)
	}
	return string(out)
}

func TestExpiry(t *testing.T) {
	s, c := open(t, Options{})
	call(t, s, "set", `{"key":"a","value":{"n":1},"ttl":"1h"}`)
	call(t, s, "set", `{"key":"b","value":[1,2]}`) // default 1 day
	if got := call(t, s, "get", `{"key":"a"}`); got != `{"n":1}` {
		t.Fatalf("get a = %s", got)
	}
	c.add(2 * time.Hour)
	if got := call(t, s, "get", `{"key":"a"}`); got != "null" {
		t.Fatalf("expired a = %s", got)
	}
	if got := call(t, s, "has", `{"key":"b"}`); got != "true" {
		t.Fatalf("has b = %s", got)
	}
	c.add(23 * time.Hour)
	if n := s.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if st := s.Stats(); st.Entries != 0 || st.Bytes != 0 || st.Expired != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestLimits(t *testing.T) {
	s, _ := open(t, Options{MaxValueBytes: 10})
	for args, want := range map[string]string{
		`{"key":"k","value":"0123456789abc"}`:                  "limit",
		`{"key":"k","value":1,"ttl":"31d"}`:                    "30d limit",
		`{"key":"k","value":1,"ttl":-5}`:                       "ttl must be",
		`{"key":"k","value":1,"ttl":"soon"}`:                   "ttl must be",
		`{"key":"k"}`:                                          "value is required",
		`{"key":"k","value":null}`:                             "value is required",
		`{"value":1}`:                                          "key is required",
		`{"key":"` + strings.Repeat("x", 300) + `","value":1}`: "longer than 256",
	} {
		if _, err := s.Call("set", json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("set %.40s: err %v, want %q", args, err, want)
		}
	}
	if _, err := s.Call("fetch", nil); err == nil {
		t.Error("unknown tool accepted")
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	s, c := open(t, Options{MaxEntries: 3})
	for _, k := range []string{"a", "b", "c"} {
		call(t, s, "set", `{"key":"`+k+`","value":1}`)
		c.add(time.Second)
	}
	call(t, s, "get", `{"key":"a"}`) // a is now the most recent
	c.add(time.Second)
	call(t, s, "set", `{"key":"d","value":1}`)
	if got := call(t, s, "keys", `{}`); strings.Contains(got, `"b"`) || !strings.Contains(got, `"a"`) {
		t.Fatalf("keys after eviction: %s", got)
	}
	if s.Stats().Evicted != 1 {
		t.Fatalf("stats %+v", s.Stats())
	}

	// Expired entries go before live ones.
	s2, c2 := open(t, Options{MaxBytes: 40})
	call(t, s2, "set", `{"key":"old","value":"xxxxxxxxxx","ttl":60}`)
	call(t, s2, "set", `{"key":"new","value":"xxxxxxxxxx","ttl":"1d"}`)
	c2.add(2 * time.Minute)
	call(t, s2, "set", `{"key":"more","value":"xxxxxxxxxx"}`)
	if st := s2.Stats(); st.Evicted != 0 || st.Expired != 1 || st.Entries != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPrefixAndClear(t *testing.T) {
	s, _ := open(t, Options{})
	for _, k := range []string{"r:1", "r:2", "x"} {
		call(t, s, "set", `{"key":"`+k+`","value":true}`)
	}
	var keys []struct{ Key string }
	_ = json.Unmarshal([]byte(call(t, s, "keys", `{"prefix":"r:"}`)), &keys)
	if len(keys) != 2 || keys[0].Key != "r:1" {
		t.Fatalf("keys %+v", keys)
	}
	if got := call(t, s, "clear", `{"prefix":"r:"}`); got != "2" {
		t.Fatalf("clear = %s", got)
	}
	if got := call(t, s, "delete", `{"key":"x"}`); got != "true" {
		t.Fatalf("delete = %s", got)
	}
	if got := call(t, s, "delete", `{"key":"x"}`); got != "false" {
		t.Fatalf("delete again = %s", got)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	s, c := open(t, Options{Path: path})
	call(t, s, "set", `{"key":"keep","value":"v","ttl":"7d"}`)
	call(t, s, "set", `{"key":"short","value":"v","ttl":"1m"}`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", st, err)
	}

	// Reopened later: the short entry expired while the app was closed and
	// is dropped from disk too.
	c.add(time.Hour)
	s2, err := Open(Options{Path: path, Now: c.now, SweepEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, s2, "get", `{"key":"keep"}`); got != `"v"` {
		t.Fatalf("keep = %s", got)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "short") {
		t.Fatalf("expired entry still on disk: %s", raw)
	}

	// A damaged file starts the cache empty instead of failing.
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	s3, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if s3.Stats().Entries != 0 {
		t.Fatal("loaded entries from a damaged file")
	}
}
