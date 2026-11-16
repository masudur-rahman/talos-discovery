package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/masudur-rahman/talos-discovery/state"
)

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestLanding(t *testing.T) {
	h := NewSite(state.New()).Handler()
	resp, body := get(t, h, "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Talos discovery service") {
		t.Errorf("landing = %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, h, "/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", resp.StatusCode)
	}
}

func TestInspect(t *testing.T) {
	st := state.New()
	_ = st.Update("c1", "node-a", []byte{0xde, 0xad}, [][]byte{{0xbe, 0xef}}, time.Minute)
	h := NewSite(st).Handler()

	_, body := get(t, h, "/inspect?clusterID=c1")
	for _, want := range []string{"node-a", "dead", "beef", "1 affiliate in"} {
		if !strings.Contains(body, want) {
			t.Errorf("inspect page lacks %q", want)
		}
	}

	_, body = get(t, h, "/inspect?clusterID=%3Cscript%3E")
	if strings.Contains(body, "<script>") {
		t.Error("cluster id is rendered unescaped")
	}

	// Looking at an unknown cluster must not create it.
	get(t, h, "/inspect?clusterID=never-seen")
	if s := st.Stats(); s.Clusters != 1 {
		t.Errorf("clusters = %d, want inspect to leave state alone", s.Clusters)
	}

	_, body = get(t, h, "/inspect?clusterID="+strings.Repeat("x", 300))
	if !strings.Contains(body, "too long") {
		t.Error("oversized cluster id not rejected")
	}
}

func TestStats(t *testing.T) {
	st := state.New()
	_ = st.Update("c1", "a1", nil, [][]byte{[]byte("e")}, time.Minute)
	h := NewSite(st).Handler()

	resp, body := get(t, h, "/stats")
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("stats lacks CORS header")
	}
	var got map[string]int
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("stats is not JSON: %q", body)
	}
	if got["affiliates"] != 1 || got["clusters"] != 1 || got["endpoints"] != 1 {
		t.Errorf("stats = %v", got)
	}

	// Cached: a change inside the TTL is not visible yet.
	_ = st.Update("c1", "a2", nil, nil, time.Minute)
	_, body = get(t, h, "/stats")
	_ = json.Unmarshal([]byte(body), &got)
	if got["affiliates"] != 1 {
		t.Errorf("stats recomputed inside its TTL: %v", got)
	}
}

func TestHealthz(t *testing.T) {
	site := NewSite(state.New())
	h := site.Handler()
	if resp, _ := get(t, h, "/healthz"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("healthz before ready = %d", resp.StatusCode)
	}
	site.SetReady(true)
	if resp, _ := get(t, h, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz when ready = %d", resp.StatusCode)
	}
}

func TestDebugHandler(t *testing.T) {
	if resp, body := get(t, DebugHandler(), "/debug/pprof/"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "goroutine") {
		t.Errorf("pprof index = %d", resp.StatusCode)
	}
}
