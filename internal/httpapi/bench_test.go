package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// serve runs one request through the handler and fails the benchmark on a
// non-success status.
func serve(b *testing.B, s *Server, req *http.Request, want int) {
	b.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != want {
		b.Fatalf("%s %s: status = %d, want %d; body = %s", req.Method, req.URL, rec.Code, want, rec.Body.String())
	}
}

// BenchmarkReadRelation measures the full read path: authenticate, parse,
// generate, execute in a request transaction, and shape the response.
func BenchmarkReadRelation(b *testing.B) {
	s := newLiveServer(b)
	b.ReportAllocs()
	for b.Loop() {
		serve(b, s, httptest.NewRequest(http.MethodGet, "/todos?select=id,task&limit=2", nil), http.StatusOK)
	}
}

// BenchmarkReadWithCount adds the exact-count path, which runs a second
// aggregate in the same statement.
func BenchmarkReadWithCount(b *testing.B) {
	s := newLiveServer(b)
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/todos?limit=1", nil)
		req.Header.Set("Prefer", "count=exact")
		serve(b, s, req, http.StatusPartialContent)
	}
}

// BenchmarkPatchRelation measures a write path with a minimal representation.
func BenchmarkPatchRelation(b *testing.B) {
	s := newLiveServer(b)
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPatch, "/todos?id=eq.1", strings.NewReader(`{"done":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", "return=minimal")
		serve(b, s, req, http.StatusNoContent)
	}
}

// BenchmarkRPC measures a table-returning function call.
func BenchmarkRPC(b *testing.B) {
	s := newLiveServer(b)
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/rpc/add_them", strings.NewReader(`{"a":1,"b":2}`))
		req.Header.Set("Content-Type", "application/json")
		serve(b, s, req, http.StatusOK)
	}
}

// BenchmarkReadParallel measures throughput under concurrent load, where the
// pooled connections and the single-round-trip setup matter.
func BenchmarkReadParallel(b *testing.B) {
	s := newLiveServer(b)
	var failed atomic.Bool
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/todos?select=id,task&limit=2", nil))
			if rec.Code != http.StatusOK {
				failed.Store(true)
				return
			}
		}
	})
	if failed.Load() {
		b.Fatal("a concurrent request failed")
	}
}
