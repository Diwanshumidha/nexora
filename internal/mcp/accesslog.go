package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// accessLogMax caps the log; past it the file starts over.
const accessLogMax = 1 << 20

// AccessLog wraps h and appends one line per request to path: what a remote
// AI client actually sent and what it got back. Tokens are never written.
// It exists because "couldn't reach the server" from a hosted client says
// nothing about whether the request arrived.
func AccessLog(h http.Handler, g *Guard, path string) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		method := rpcMethod(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)

		line := fmt.Sprintf("%s %s %s %d %dms host=%q rpc=%q origin=%q ua=%q accept=%q auth=%v fwd=%q\n",
			start.Format(time.RFC3339), r.Method, redactPath(r.URL.Path, g.Token()), rec.status,
			time.Since(start).Milliseconds(), r.Host, method, r.Header.Get("Origin"), r.UserAgent(),
			r.Header.Get("Accept"), r.Header.Get("Authorization") != "", r.Header.Get("X-Forwarded-For"))
		mu.Lock()
		defer mu.Unlock()
		flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
		if st, err := os.Stat(path); err == nil && st.Size() > accessLogMax {
			flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		}
		if f, err := os.OpenFile(path, flags, 0o600); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	})
}

// redactPath replaces the secret path segment with whether it was right.
func redactPath(p, token string) string {
	rest, ok := strings.CutPrefix(p, "/mcp/")
	if !ok || rest == "" {
		return p
	}
	tok, tail, _ := strings.Cut(rest, "/")
	mark := "<bad-token>"
	if tokenMatches(tok, token) {
		mark = "<token>"
	}
	if tail != "" || strings.HasSuffix(rest, "/") {
		return "/mcp/" + mark + "/" + tail
	}
	return "/mcp/" + mark
}

// rpcMethod peeks at a JSON-RPC body's method without consuming it.
func rpcMethod(r *http.Request) string {
	if r.Body == nil || r.Method != http.MethodPost {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	if err != nil {
		return ""
	}
	var msg struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &msg) == nil && msg.Method != "" {
		return msg.Method
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		return "(batch)"
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush keeps streamed (SSE) responses working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
