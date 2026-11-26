package services

import (
	"sort"
	"time"
	"unicode/utf8"

	"nexora/internal/cache"
)

// StorageService shows and clears tools.store, Code Mode's cache.
type StorageService struct {
	store *cache.Store
}

func NewStorageService(store *cache.Store) *StorageService {
	return &StorageService{store: store}
}

// StorageEntry is one cached key with a short preview of its value.
type StorageEntry struct {
	Key       string    `json:"key"`
	Size      int       `json:"size"`
	Preview   string    `json:"preview"`
	Created   time.Time `json:"created"`
	ExpiresAt time.Time `json:"expiresAt"`
	Accessed  time.Time `json:"accessed"`
}

type StorageView struct {
	Stats   cache.Stats    `json:"stats"`
	Entries []StorageEntry `json:"entries"`
}

const previewChars = 160

// List returns the cache's usage and its entries (keys starting with
// prefix), most recently used first. Viewing doesn't count as a use.
func (s *StorageService) List(prefix string) StorageView {
	s.store.Sweep()
	view := StorageView{Stats: s.store.Stats(), Entries: []StorageEntry{}}
	for _, i := range s.store.Keys(prefix) {
		e := StorageEntry{Key: i.Key, Size: i.Size, Created: i.Created, ExpiresAt: i.Expires, Accessed: i.Accessed}
		if v, ok := s.store.Peek(i.Key); ok {
			e.Preview = preview(string(v))
		}
		view.Entries = append(view.Entries, e)
	}
	sort.SliceStable(view.Entries, func(a, b int) bool { return view.Entries[a].Accessed.After(view.Entries[b].Accessed) })
	return view
}

func preview(s string) string {
	if utf8.RuneCountInString(s) <= previewChars {
		return s
	}
	return string([]rune(s)[:previewChars]) + "…"
}

// Delete removes one key.
func (s *StorageService) Delete(key string) bool { return s.store.Delete(key) }

// Clear removes every key and returns how many there were.
func (s *StorageService) Clear() int { return s.store.Clear("") }
