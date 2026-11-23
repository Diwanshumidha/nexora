package extension_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexora/internal/extension"
)

func startRaw(b *testing.B) *extension.Manager {
	mgr, src := newTestManager(b)
	_, err := mgr.Install(src)
	must(b, err)
	must(b, mgr.Start(context.Background(), "raw"))
	b.Cleanup(func() { mgr.StopAll(context.Background()) })
	return mgr
}

func BenchmarkCallToolSmall(b *testing.B) {
	mgr := startRaw(b)
	args := json.RawMessage(`{"a":1}`)
	for b.Loop() {
		if _, err := mgr.CallTool(context.Background(), "raw", "ping", args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCallToolLargeArgs(b *testing.B) {
	mgr := startRaw(b)
	args := json.RawMessage(`{"rows":"` + strings.Repeat("x", 1<<20) + `"}`)
	b.SetBytes(int64(len(args)))
	for b.Loop() {
		if _, err := mgr.CallTool(context.Background(), "raw", "ping", args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCallToolLargeResult(b *testing.B) {
	mgr := startRaw(b)
	b.SetBytes(200000)
	for b.Loop() {
		if _, err := mgr.CallTool(context.Background(), "raw", "big", json.RawMessage(`{"quiet":true}`)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCallToolParallel(b *testing.B) {
	mgr := startRaw(b)
	args := json.RawMessage(`{"a":1}`)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := mgr.CallTool(context.Background(), "raw", "ping", args); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
