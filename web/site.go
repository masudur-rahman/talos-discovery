// Package web serves the HTTP side of the service: the landing page, the
// cluster inspector, the stats endpoint and health on the site, plus the
// separate metrics and debug listeners. Nodes never talk to any of it.
package web

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/masudur-rahman/talos-discovery/models"
	"github.com/masudur-rahman/talos-discovery/state"
)

//go:embed templates/*.html
var templateFS embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"hex": hex.EncodeToString,
}).ParseFS(templateFS, "templates/*.html"))

// statsTTL bounds how stale /stats may be. The count is a scan of every
// cluster; a public dashboard polling it should not cost a scan per hit.
const statsTTL = time.Minute

// Site is the landing page, /inspect, /stats and /healthz. It is served on the
// main port next to gRPC, and on the landing port when one is configured.
type Site struct {
	state *state.State
	ready atomic.Bool

	statsMu   sync.Mutex
	statsBody []byte
	statsAt   time.Time
}

func NewSite(st *state.State) *Site {
	return &Site{state: st}
}

// SetReady flips /healthz. It starts false and turns true once the snapshot is
// loaded and the listeners are up, so a probe never routes nodes to a server
// that would hand them an empty cluster; it goes false again at shutdown.
func (s *Site) SetReady(ready bool) {
	s.ready.Store(ready)
}

func (s *Site) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.landing)
	mux.HandleFunc("GET /inspect", s.inspect)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

// HTTPServer wraps a handler with the timeouts a bare http.Server lacks.
func HTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func (s *Site) landing(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "landing.html", nil)
}

type inspectData struct {
	ClusterID  string
	Affiliates []state.Affiliate
	Invalid    string
}

// inspect lists one cluster's affiliates as the server holds them: ids in the
// clear, data and endpoints as the ciphertext they are. Anyone who knows a
// cluster id can see this, which is exactly what the public service shows;
// guard the path at the proxy if cluster ids are not to be treated as public.
func (s *Site) inspect(w http.ResponseWriter, r *http.Request) {
	data := inspectData{ClusterID: r.URL.Query().Get("clusterID")}
	switch {
	case data.ClusterID == "":
	case len(data.ClusterID) > models.ClusterIDMax:
		data.Invalid = "cluster id is too long"
	default:
		data.Affiliates = s.state.List(data.ClusterID)
	}
	s.render(w, "inspect.html", data)
}

// statsJSON is upstream's {"affiliates": N} with the other counts beside it;
// a consumer of the upstream shape reads it unchanged.
type statsJSON struct {
	Clusters      int `json:"clusters"`
	Affiliates    int `json:"affiliates"`
	Endpoints     int `json:"endpoints"`
	Subscriptions int `json:"subscriptions"`
}

func (s *Site) stats(w http.ResponseWriter, _ *http.Request) {
	s.statsMu.Lock()
	if s.statsBody == nil || time.Since(s.statsAt) > statsTTL {
		st := s.state.Stats()
		body, err := json.Marshal(statsJSON(st))
		if err != nil {
			s.statsMu.Unlock()
			http.Error(w, `{"error":"stats unavailable"}`, http.StatusInternalServerError)
			return
		}
		s.statsBody, s.statsAt = body, time.Now()
	}
	body := s.statsBody
	s.statsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(body)
}

func (s *Site) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Site) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render page", "page", name, "error", err)
	}
}
