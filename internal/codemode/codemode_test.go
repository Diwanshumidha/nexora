package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexora/internal/protocol"
)

type fakeCaller struct {
	delay time.Duration

	mu    sync.Mutex
	calls []string

	inFlight, maxInFlight atomic.Int32
}

func (f *fakeCaller) CallTool(ctx context.Context, ns, tool string, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ns+"."+tool+" "+string(args))
	f.mu.Unlock()

	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxInFlight.Load()
		if n <= m || f.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if tool == "fail" {
		return nil, errors.New("boom")
	}
	return args, nil // echo
}

func runScript(t *testing.T, code string, opts Options) (*Result, error, *fakeCaller) {
	t.Helper()
	return runWith(&fakeCaller{}, code, opts)
}

func runWith(f *fakeCaller, code string, opts Options) (*Result, error, *fakeCaller) {
	res, err := New(f, opts).Run(context.Background(), code, []Namespace{{Name: "x", Tools: []string{"echo", "fail"}}})
	return res, err, f
}

func TestRunParallelToolCalls(t *testing.T) {
	f := &fakeCaller{delay: 100 * time.Millisecond}
	start := time.Now()
	res, err, _ := runWith(f, `
		const out = await Promise.all([1, 2, 3, 4, 5, 6, 7, 8].map(n => tools.x.echo({ n })))
		return out.map(o => o.n)
	`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("8 calls of 100ms took %s; they should overlap", elapsed)
	}
	b, _ := json.Marshal(res.Value)
	if len(res.Tools) != 1 || res.Tools[0] != "x.echo" {
		t.Fatalf("tools used: %v", res.Tools)
	}
	if string(b) != "[1,2,3,4,5,6,7,8]" || res.ToolCalls != 8 {
		t.Fatalf("got %s (%d calls)", b, res.ToolCalls)
	}
}

func TestRunConcurrencyLimit(t *testing.T) {
	f := &fakeCaller{delay: 20 * time.Millisecond}
	_, err, _ := runWith(f, `
		await Promise.all(Array.from({ length: 20 }, (_, n) => tools.x.echo({ n })))
	`, Options{MaxConcurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.maxInFlight.Load(); got != 3 {
		t.Fatalf("max in-flight calls = %d, want 3", got)
	}
}

func TestRunSequentialAwaitsStillWork(t *testing.T) {
	res, err, _ := runScript(t, `
		let total = 0
		for (let i = 1; i <= 5; i++) total += (await tools.x.echo({ i })).i
		return total
	`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != float64(15) {
		t.Fatalf("got %v", res.Value)
	}
}

func TestRunNeverResolves(t *testing.T) {
	_, err, _ := runScript(t, `await new Promise(() => {})`, Options{})
	if err == nil || !strings.Contains(err.Error(), "never resolve") {
		t.Fatalf("got %v", err)
	}
}

func TestRunTimeoutDuringToolCall(t *testing.T) {
	f := &fakeCaller{delay: time.Hour}
	_, err, _ := runWith(f, `await tools.x.echo({})`, Options{Timeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
}

func TestRunResultIsNativeJS(t *testing.T) {
	res, err, _ := runScript(t, `
		const r = await tools.x.echo({ list: [1, 2] })
		return [Array.isArray(r.list), r.list.map(n => n * 2), Object.keys(r)]
	`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Value)
	if string(b) != `[true,[2,4],["list"]]` {
		t.Fatalf("got %s", b)
	}
}

func TestRunEchoAndJSONResult(t *testing.T) {
	res, err, f := runScript(t, `
		const r = await tools.x.echo({ n: 1, when: new Date(0) })
		return { r, fn: () => 1, skip: undefined }
	`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Value)
	if string(b) != `{"r":{"n":1,"when":"1970-01-01T00:00:00.000Z"}}` {
		t.Fatalf("unexpected result %s", b)
	}
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], `x.echo {"n":1`) {
		t.Fatalf("unexpected calls %v", f.calls)
	}
}

func TestRunNoArgsSendsEmptyObject(t *testing.T) {
	_, err, f := runScript(t, `await tools.x.echo()`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "x.echo {}" {
		t.Fatalf("got %q", f.calls[0])
	}
}

func TestRunToolErrorIsCatchable(t *testing.T) {
	res, err, _ := runScript(t, `
		try { await tools.x.fail({}) } catch (e) { return "caught: " + e.message }
	`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Value.(string), "boom") {
		t.Fatalf("got %v", res.Value)
	}
}

func TestRunUncaughtError(t *testing.T) {
	_, err, _ := runScript(t, `throw new Error("nope")`, Options{})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
}

func TestRunTimeout(t *testing.T) {
	_, err, _ := runScript(t, `while (true) {}`, Options{Timeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
}

func TestRunLogCap(t *testing.T) {
	res, err, _ := runScript(t, `for (let i = 0; i < 50; i++) console.log(i)`, Options{MaxLogLines: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Logs) != 11 {
		t.Fatalf("expected 10 lines + truncation marker, got %d", len(res.Logs))
	}
}

func TestRunNoAmbientCapabilities(t *testing.T) {
	res, err, _ := runScript(t, `return [typeof require, typeof process, typeof fetch, typeof setTimeout]`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Value)
	if string(b) != `["undefined","undefined","undefined","undefined"]` {
		t.Fatalf("sandbox leaks globals: %s", b)
	}
}

func TestGenerateTypes(t *testing.T) {
	out := GenerateTypes([]APINamespace{{
		Name:        "x",
		Description: "evil */ comment",
		Tools: []protocol.ToolSpec{{
			Name:         "get",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"first-name":{"type":["string","null"]},"id":{"type":"integer"}},"required":["id"]}`),
			OutputSchema: json.RawMessage(`{"type":"array","items":{"enum":["a","b"]}}`),
		}},
	}})
	for _, want := range []string{`evil *\/ comment`, `"first-name"?: string | null;`, `id: number;`, `Promise<Array<"a" | "b">>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func BenchmarkRunOverhead(b *testing.B) {
	rt := New(&fakeCaller{}, Options{})
	ns := []Namespace{{Name: "x", Tools: []string{"echo"}}}
	for b.Loop() {
		if _, err := rt.Run(context.Background(), `return (await tools.x.echo({ a: 1 })).a`, ns); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunFanOut8x10ms(b *testing.B) {
	rt := New(&fakeCaller{delay: 10 * time.Millisecond}, Options{})
	ns := []Namespace{{Name: "x", Tools: []string{"echo"}}}
	for b.Loop() {
		if _, err := rt.Run(context.Background(), `await Promise.all([1,2,3,4,5,6,7,8].map(n => tools.x.echo({ n })))`, ns); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRunRecordsCalls(t *testing.T) {
	res, err, _ := runScript(t, `
		await tools.x.echo({ n: 1 })
		try { await tools.x.fail({ why: "test" }) } catch {}
		return 1
	`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %+v", res.Calls)
	}
	ok, failed := res.Calls[0], res.Calls[1]
	if ok.Tool != "x.echo" || ok.Status != "ok" || string(ok.Args) != `{"n":1}` || ok.ResultBytes == 0 {
		t.Fatalf("ok call: %+v", ok)
	}
	if failed.Tool != "x.fail" || failed.Status != "error" || !strings.Contains(failed.Error, "boom") {
		t.Fatalf("failed call: %+v", failed)
	}
}

func TestRunUnknownToolsExplain(t *testing.T) {
	for code, want := range map[string]string{
		`return await tools.nope.echo({})`: "tools.nope does not exist. Available: x",
		`return await tools.x.ehco({})`:    "tools.x.ehco does not exist. Its tools: echo, fail",
	} {
		_, err, _ := runScript(t, code, Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", code, err, want)
		}
	}
	// Feature checks and awaiting the objects themselves still work.
	res, err, _ := runScript(t, `await tools; await tools.x; return ["x" in tools, typeof tools.x.echo, Object.keys(tools)]`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(res.Value); string(got) != `[true,"function",["x"]]` {
		t.Fatalf("got %s", got)
	}
}
