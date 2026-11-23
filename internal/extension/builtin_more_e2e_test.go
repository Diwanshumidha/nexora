package extension_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nexora/internal/extension"
)

// tinyPDF builds a valid PDF with one line of text per page.
func tinyPDF(pages ...string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	n := len(pages)
	kids := make([]string, n)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, text := range pages {
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info << /Title (Q3 Report) /Author (Nexora) >> >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

func TestPDFExtension(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "reports"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "reports", "q3.pdf"), tinyPDF("Revenue grew 12 percent", "Churn fell in EMEA"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "notes.pdf"), tinyPDF("Nothing about money"), 0o644))

	p := builtin(t, "pdf", extension.Values{"folders": []any{dir}})
	if out := string(p.do("info", obj{"path": "reports/q3.pdf"})); !strings.Contains(out, `"pages":2`) || !strings.Contains(out, `"title":"Q3 Report"`) {
		t.Fatalf("info: %s", out)
	}
	var read struct {
		Pages []struct {
			Page int
			Text string
		}
		TotalPages int
	}
	must(t, json.Unmarshal(p.do("readText", obj{"path": "reports/q3.pdf"}), &read))
	if read.TotalPages != 2 || len(read.Pages) != 2 || !strings.Contains(read.Pages[1].Text, "Churn fell") {
		t.Fatalf("readText: %+v", read)
	}
	must(t, json.Unmarshal(p.do("readText", obj{"path": "reports/q3.pdf", "pages": "2"}), &read))
	if len(read.Pages) != 1 || read.Pages[0].Page != 2 {
		t.Fatalf("pages=2: %+v", read)
	}
	out := string(p.do("search", obj{"query": "emea"}))
	if !strings.Contains(out, `"page":2`) || !strings.Contains(out, "q3.pdf") || !strings.Contains(out, `"filesSearched":2`) {
		t.Fatalf("search: %s", out)
	}
	p.bad("Invalid pages", "readText", obj{"path": "reports/q3.pdf", "pages": "two"})
	p.bad("outside the trusted folders", "info", obj{"path": "/etc/hostname"})
}

func TestCSVExtension(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "sales.csv"), []byte(
		"region,product,revenue,date\nEMEA,Widget,\"1,200\",2026-01-03\nEMEA,Gadget,800,2026-01-04\nAPAC,Widget,950,2026-01-05\nEMEA,Widget,300,2026-02-01\nAPAC,Gadget,,2026-02-02\n"), 0o644))
	c := builtin(t, "csv", extension.Values{"folders": []any{dir}})

	if out := string(c.do("read", obj{"path": "sales.csv", "limit": 2})); !strings.Contains(out, `"totalRows":5`) || !strings.Contains(out, `"columns":["region","product","revenue","date"]`) {
		t.Fatalf("read: %s", out)
	}
	var q struct {
		Rows        []map[string]any
		MatchedRows int
	}
	must(t, json.Unmarshal(c.do("query", obj{
		"path":      "sales.csv",
		"where":     []obj{{"column": "region", "op": "eq", "value": "emea"}},
		"groupBy":   []string{"product"},
		"aggregate": []obj{{"column": "revenue", "fn": "sum"}, {"fn": "count"}},
		"sort":      []obj{{"column": "sum_revenue", "desc": true}},
	}), &q))
	if q.MatchedRows != 3 || len(q.Rows) != 2 || q.Rows[0]["product"] != "Widget" || q.Rows[0]["sum_revenue"] != 1500.0 || q.Rows[1]["count"] != 1.0 {
		t.Fatalf("query: %+v", q)
	}
	stats := string(c.do("stats", obj{"path": "sales.csv"}))
	if !strings.Contains(stats, `"name":"revenue","type":"number"`) || !strings.Contains(stats, `"empty":1`) || !strings.Contains(stats, `"type":"date"`) {
		t.Fatalf("stats: %s", stats)
	}
	c.do("write", obj{"path": "out/summary.csv", "rows": []obj{{"product": "Widget", "total": 1500}}})
	data, err := os.ReadFile(filepath.Join(dir, "out", "summary.csv"))
	must(t, err)
	if string(data) != "product,total\r\nWidget,1500\n" && string(data) != "product,total\nWidget,1500\n" {
		t.Fatalf("written csv: %q", data)
	}
	c.bad("already exists", "write", obj{"path": "out/summary.csv", "rows": []obj{{"a": 1}}})
	c.bad(`Unknown column "price"`, "query", obj{"path": "sales.csv", "where": []obj{{"column": "price", "op": "gt", "value": 1}}})
}

func writeZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		must(t, err)
		_, err = w.Write(data)
		must(t, err)
	}
	must(t, zw.Close())
	must(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func TestArchiveExtension(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Dir(dir) // the parent: never written to
	must(t, os.MkdirAll(filepath.Join(dir, "project", "src"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "project", "README.md"), []byte("hello"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "project", "src", "main.go"), []byte("package main"), 0o644))
	a := builtin(t, "archive", extension.Values{"folders": []any{dir}, "maxExtractMB": 10})

	// Round trip.
	if out := string(a.do("create", obj{"files": []string{"project"}, "to": "backup.zip"})); !strings.Contains(out, `"files":2`) {
		t.Fatalf("create: %s", out)
	}
	if out := string(a.do("list", obj{"path": "backup.zip"})); !strings.Contains(out, "project/src/main.go") {
		t.Fatalf("list: %s", out)
	}
	a.do("extract", obj{"path": "backup.zip", "to": "restored"})
	if got, _ := os.ReadFile(filepath.Join(dir, "restored", "project", "src", "main.go")); string(got) != "package main" {
		t.Fatalf("extracted content: %q", got)
	}
	a.bad("already exists", "extract", obj{"path": "backup.zip", "to": "restored"})

	// Zip-slip: an entry climbing out of the destination.
	writeZip(t, filepath.Join(dir, "evil.zip"), map[string][]byte{"ok.txt": []byte("fine"), "../../evil.txt": []byte("pwned")})
	a.bad("outside", "extract", obj{"path": "evil.zip"})
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("zip-slip wrote outside the destination")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("a refused archive must not leave a half-extracted folder")
	}

	// Zip bomb: 50 MB of zeros compresses to a few KB; the limit is 10 MB.
	writeZip(t, filepath.Join(dir, "bomb.zip"), map[string][]byte{"zeros.bin": make([]byte, 50<<20)})
	if st, _ := os.Stat(filepath.Join(dir, "bomb.zip")); st.Size() > 1<<20 {
		t.Fatalf("fixture should be tiny, is %d bytes", st.Size())
	}
	a.bad("more than 10 MB", "extract", obj{"path": "bomb.zip"})
	if _, err := os.Stat(filepath.Join(dir, "bomb")); err == nil {
		t.Fatal("the bomb's folder should be cleaned up")
	}
}

func TestSystemExtension(t *testing.T) {
	s := builtin(t, "system", nil)
	var info struct {
		CPU      struct{ Cores int }
		MemoryGB struct{ Total float64 }
	}
	must(t, json.Unmarshal(s.do("info", nil), &info))
	if info.CPU.Cores < 1 || info.MemoryGB.Total <= 0 {
		t.Fatalf("info: %+v", info)
	}
	if out := string(s.do("usage", nil)); !strings.Contains(out, "cpuPercent") {
		t.Fatalf("usage: %s", out)
	}
	if out := string(s.do("now", nil)); !strings.Contains(out, "timezone") {
		t.Fatalf("now: %s", out)
	}
}
