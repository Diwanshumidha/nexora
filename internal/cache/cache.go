// Package cache is the storage behind Code Mode's built-in `tools.store`:
// a small key-value store that lets scripts keep data between runs.
//
// It is deliberately a cache, not a database. Every entry expires (a default
// TTL applies when the script gives none, and no TTL may exceed MaxTTL), the
// store has a fixed size and entry budget, and when a write would exceed it
// the least recently used entries are evicted. Expired entries are dropped on
// read, by a periodic sweep and when the store is loaded, so nothing lingers
// on disk. Scripts must treat a miss as normal and recompute.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Options bound the store. Zero values take the defaults below.
type Options struct {
	// Path is the JSON file the store persists to; empty keeps it in memory.
	Path          string
	MaxBytes      int64         // keys plus values, default 16 MB
	MaxEntries    int           // default 10,000
	MaxValueBytes int           // one value as JSON, default 1 MB
	MaxKeyLen     int           // default 256 bytes
	DefaultTTL    time.Duration // default 24 h
	MaxTTL        time.Duration // default 30 days
	// SweepEvery is how often expired entries are purged; default 1 min.
	SweepEvery time.Duration
	// SaveDelay batches writes to disk; default 1 s.
	SaveDelay time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

func (o *Options) defaults() {
	if o.MaxBytes <= 0 {
		o.MaxBytes = 16 << 20
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = 10_000
	}
	if o.MaxValueBytes <= 0 {
		o.MaxValueBytes = 1 << 20
	}
	if o.MaxKeyLen <= 0 {
		o.MaxKeyLen = 256
	}
	if o.DefaultTTL <= 0 {
		o.DefaultTTL = 24 * time.Hour
	}
	if o.MaxTTL <= 0 {
		o.MaxTTL = 30 * 24 * time.Hour
	}
	if o.SweepEvery <= 0 {
		o.SweepEvery = time.Minute
	}
	if o.SaveDelay <= 0 {
		o.SaveDelay = time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// entry is one stored value. Accessed drives LRU eviction.
type entry struct {
	Value    json.RawMessage `json:"value"`
	Created  time.Time       `json:"created"`
	Expires  time.Time       `json:"expires"`
	Accessed time.Time       `json:"accessed"`
}

func size(key string, e *entry) int64 { return int64(len(key) + len(e.Value)) }

// Info describes an entry without its value.
type Info struct {
	Key      string    `json:"key"`
	Size     int       `json:"size"` // bytes of the value as JSON
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Accessed time.Time `json:"accessed"`
}

// Stats summarises the store against its limits.
type Stats struct {
	Entries    int           `json:"entries"`
	Bytes      int64         `json:"bytes"`
	MaxEntries int           `json:"maxEntries"`
	MaxBytes   int64         `json:"maxBytes"`
	DefaultTTL time.Duration `json:"defaultTtl"`
	MaxTTL     time.Duration `json:"maxTtl"`
	// Evicted counts entries dropped for space, and Expired those dropped
	// for age, since the app started.
	Evicted int `json:"evicted"`
	Expired int `json:"expired"`
}

type Store struct {
	opts Options

	mu       sync.Mutex
	entries  map[string]*entry
	bytes    int64
	evicted  int
	expired  int
	dirty    bool
	saveTask *time.Timer
	saveErr  error

	stop chan struct{}
	done chan struct{}
}

// Open loads the store from opts.Path (dropping anything that expired while
// the app was closed) and starts the sweeper.
func Open(opts Options) (*Store, error) {
	opts.defaults()
	s := &Store{opts: opts, entries: map[string]*entry{}, stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.load(); err != nil {
		return nil, err
	}
	go s.sweeper()
	return s, nil
}

// file is the on-disk format.
type file struct {
	Version int               `json:"version"`
	Entries map[string]*entry `json:"entries"`
}

func (s *Store) load() error {
	if s.opts.Path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.opts.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 {
		// A cache is disposable: start empty rather than refuse to run.
		_ = os.Rename(s.opts.Path, s.opts.Path+".corrupt")
		return nil
	}
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range f.Entries {
		if e == nil || !now.Before(e.Expires) || s.validKey(k) != nil {
			s.dirty = true
			continue
		}
		// Never trust a stored expiry further out than the current limit.
		if limit := now.Add(s.opts.MaxTTL); e.Expires.After(limit) {
			e.Expires = limit
		}
		s.entries[k] = e
		s.bytes += size(k, e)
	}
	s.evictLocked(0, 0)
	if s.dirty {
		s.scheduleLocked()
	}
	return nil
}

func (s *Store) validKey(k string) error {
	switch {
	case k == "":
		return errors.New("key must not be empty")
	case len(k) > s.opts.MaxKeyLen:
		return fmt.Errorf("key is longer than %d bytes", s.opts.MaxKeyLen)
	case !utf8.ValidString(k):
		return errors.New("key must be valid text")
	}
	return nil
}

// Get returns the value for key, or false if it is missing or expired.
func (s *Store) Get(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.liveLocked(key)
	if e == nil {
		return nil, false
	}
	// Reads don't schedule a save; the new access time is written with the
	// next change, which is all LRU across restarts needs.
	e.Accessed = s.opts.Now()
	return e.Value, true
}

// Peek is Get without counting as a use, for showing the cache to the user.
func (s *Store) Peek(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.liveLocked(key); e != nil {
		return e.Value, true
	}
	return nil, false
}

// Has reports whether key is present and unexpired, without counting as a
// use for eviction.
func (s *Store) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveLocked(key) != nil
}

// liveLocked returns key's entry, dropping it if it has expired.
func (s *Store) liveLocked(key string) *entry {
	e := s.entries[key]
	if e == nil {
		return nil
	}
	if !s.opts.Now().Before(e.Expires) {
		s.removeLocked(key, e)
		s.expired++
		return nil
	}
	return e
}

// Set stores value (JSON) under key for ttl (0 means DefaultTTL), evicting
// the least recently used entries if the store is full. It returns when the
// entry expires.
func (s *Store) Set(key string, value json.RawMessage, ttl time.Duration) (time.Time, error) {
	if err := s.validKey(key); err != nil {
		return time.Time{}, err
	}
	if len(value) == 0 || !json.Valid(value) {
		return time.Time{}, errors.New("value must be JSON")
	}
	if len(value) > s.opts.MaxValueBytes {
		return time.Time{}, fmt.Errorf("value is %s; the limit is %s", human(int64(len(value))), human(int64(s.opts.MaxValueBytes)))
	}
	switch {
	case ttl < 0:
		return time.Time{}, errors.New("ttl must be positive")
	case ttl == 0:
		ttl = s.opts.DefaultTTL
	case ttl > s.opts.MaxTTL:
		return time.Time{}, fmt.Errorf("ttl is longer than the %s limit", formatTTL(s.opts.MaxTTL))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	e := &entry{Value: append(json.RawMessage(nil), value...), Created: now, Expires: now.Add(ttl), Accessed: now}
	if old := s.entries[key]; old != nil {
		s.removeLocked(key, old)
	}
	s.evictLocked(size(key, e), 1)
	s.entries[key] = e
	s.bytes += size(key, e)
	s.scheduleLocked()
	return e.Expires, nil
}

// Delete removes key and reports whether it was present.
func (s *Store) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.liveLocked(key)
	if e == nil {
		return false
	}
	s.removeLocked(key, e)
	s.scheduleLocked()
	return true
}

// Keys lists unexpired entries whose key starts with prefix, sorted by key.
func (s *Store) Keys(prefix string) []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	out := []Info{}
	for k, e := range s.entries {
		if strings.HasPrefix(k, prefix) && now.Before(e.Expires) {
			out = append(out, Info{Key: k, Size: len(e.Value), Created: e.Created, Expires: e.Expires, Accessed: e.Accessed})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Clear removes every entry whose key starts with prefix ("" clears all)
// and returns how many were live.
func (s *Store) Clear(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	n := 0
	for k, e := range s.entries {
		if strings.HasPrefix(k, prefix) {
			if now.Before(e.Expires) {
				n++
			}
			s.removeLocked(k, e)
		}
	}
	s.scheduleLocked()
	return n
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Entries: len(s.entries), Bytes: s.bytes,
		MaxEntries: s.opts.MaxEntries, MaxBytes: s.opts.MaxBytes,
		DefaultTTL: s.opts.DefaultTTL, MaxTTL: s.opts.MaxTTL,
		Evicted: s.evicted, Expired: s.expired,
	}
}

// Sweep drops expired entries and returns how many it removed.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepLocked()
}

func (s *Store) sweepLocked() int {
	now := s.opts.Now()
	n := 0
	for k, e := range s.entries {
		if !now.Before(e.Expires) {
			s.removeLocked(k, e)
			n++
		}
	}
	if n > 0 {
		s.expired += n
		s.scheduleLocked()
	}
	return n
}

func (s *Store) removeLocked(key string, e *entry) {
	delete(s.entries, key)
	s.bytes -= size(key, e)
	s.dirty = true
}

// evictLocked makes room for an addition of addBytes and addEntries: first
// by dropping expired entries, then the least recently used.
func (s *Store) evictLocked(addBytes int64, addEntries int) {
	fits := func() bool {
		return s.bytes+addBytes <= s.opts.MaxBytes && len(s.entries)+addEntries <= s.opts.MaxEntries
	}
	if fits() {
		return
	}
	s.sweepLocked()
	if fits() {
		return
	}
	keys := make([]string, 0, len(s.entries))
	for k := range s.entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return s.entries[keys[i]].Accessed.Before(s.entries[keys[j]].Accessed) })
	for _, k := range keys {
		if fits() {
			break
		}
		s.removeLocked(k, s.entries[k])
		s.evicted++
	}
}

func (s *Store) sweeper() {
	defer close(s.done)
	t := time.NewTicker(s.opts.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Sweep()
		case <-s.stop:
			return
		}
	}
}

// scheduleLocked batches changes into one write after SaveDelay.
func (s *Store) scheduleLocked() {
	s.dirty = true
	if s.opts.Path == "" || s.saveTask != nil {
		return
	}
	s.saveTask = time.AfterFunc(s.opts.SaveDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.saveTask = nil
		s.saveErr = s.saveLocked()
	})
}

func (s *Store) saveLocked() error {
	if s.opts.Path == "" || !s.dirty {
		return nil
	}
	raw, err := json.Marshal(file{Version: 1, Entries: s.entries})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.Path), 0o700); err != nil {
		return err
	}
	tmp := s.opts.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.opts.Path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Flush writes pending changes now.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveTask != nil {
		s.saveTask.Stop()
		s.saveTask = nil
	}
	return s.saveLocked()
}

// Close stops the sweeper and writes pending changes.
func (s *Store) Close() error {
	select {
	case <-s.stop:
		return nil
	default:
	}
	close(s.stop)
	<-s.done
	s.mu.Lock()
	s.sweepLocked()
	s.mu.Unlock()
	return s.Flush()
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func formatTTL(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}
