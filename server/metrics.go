package server

import (
	"strings"
	"time"

	"github.com/masudur-rahman/talos-discovery/state"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is every Prometheus series the service itself exports. Names,
// labels and label values are the upstream service's, so a Grafana dashboard
// or alert written for one works on the other. Series that exist only here
// say so in their help text.
//
// The state gauges are read on scrape rather than kept up to date on every
// request, so the hot path pays nothing for them.
type Metrics struct {
	state *state.State

	clusters      *prometheus.Desc
	affiliates    *prometheus.Desc
	endpoints     *prometheus.Desc
	subscriptions *prometheus.Desc

	gcRuns       prometheus.Counter
	gcClusters   prometheus.Counter
	gcAffiliates prometheus.Counter

	hellos      *prometheus.CounterVec
	rateLimited prometheus.Counter

	storageOps        *prometheus.CounterVec
	storageSize       *prometheus.GaugeVec
	storageClusters   *prometheus.GaugeVec
	storageAffiliates *prometheus.GaugeVec
	storageEndpoints  *prometheus.GaugeVec
	storageDuration   *prometheus.GaugeVec

	// GRPC is the standard grpc_server_* set: started, handled (by code),
	// messages sent and received, and handling time.
	GRPC *grpcprom.ServerMetrics
}

// Storage operation and status label values.
const (
	OperationSave = "save"
	OperationLoad = "load"
	statusSuccess = "success"
	statusError   = "error"
)

// NewMetrics registers the series on reg.
func NewMetrics(reg prometheus.Registerer, st *state.State) (*Metrics, error) {
	storageGauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, []string{"operation"})
	}

	m := &Metrics{
		state:         st,
		clusters:      prometheus.NewDesc("discovery_state_clusters", "The current number of clusters.", nil, nil),
		affiliates:    prometheus.NewDesc("discovery_state_affiliates", "The current number of affiliates across all clusters.", nil, nil),
		endpoints:     prometheus.NewDesc("discovery_state_endpoints", "The current number of endpoints across all affiliates.", nil, nil),
		subscriptions: prometheus.NewDesc("discovery_state_subscriptions", "The current number of Watch subscriptions.", nil, nil),

		gcRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "discovery_state_gc_runs_total", Help: "The number of GC runs.",
		}),
		gcClusters: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "discovery_state_gc_clusters_total", Help: "The total number of GC'ed clusters.",
		}),
		gcAffiliates: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "discovery_state_gc_affiliates_total", Help: "The total number of GC'ed affiliates.",
		}),

		hellos: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "discovery_server_hello_requests_total", Help: "Number of hello requests by client version.",
		}, []string{"client_version"}),
		rateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "discovery_server_rate_limited_total",
			Help: "Requests rejected by the per-IP rate limiter. (talos-discovery only)",
		}),

		storageOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "discovery_storage_operations_total", Help: "The total number of storage operations.",
		}, []string{"operation", "status"}),
		storageSize:       storageGauge("discovery_storage_last_snapshot_size_bytes", "The size of the last processed snapshot in bytes."),
		storageClusters:   storageGauge("discovery_storage_last_operation_clusters", "The number of clusters in the snapshot of the last operation."),
		storageAffiliates: storageGauge("discovery_storage_last_operation_affiliates", "The number of affiliates in the snapshot of the last operation."),
		storageEndpoints:  storageGauge("discovery_storage_last_operation_endpoints", "The number of endpoints in the snapshot of the last operation."),
		storageDuration:   storageGauge("discovery_storage_last_operation_duration_seconds", "The duration of the last operation in seconds."),

		GRPC: grpcprom.NewServerMetrics(
			grpcprom.WithServerHandlingTimeHistogram(grpcprom.WithHistogramBuckets([]float64{0.01, 0.1, 0.25, 0.5, 1.0, 2.5})),
		),
	}

	// Pre-create the series a dashboard would otherwise show as "no data"
	// until the first event.
	m.hellos.WithLabelValues(unknownVersion)
	for _, op := range []string{OperationSave, OperationLoad} {
		m.storageOps.WithLabelValues(op, statusSuccess)
		m.storageOps.WithLabelValues(op, statusError)
	}

	for _, c := range []prometheus.Collector{
		m, m.gcRuns, m.gcClusters, m.gcAffiliates, m.hellos, m.rateLimited,
		m.storageOps, m.storageSize, m.storageClusters, m.storageAffiliates, m.storageEndpoints, m.storageDuration,
		m.GRPC,
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Describe implements prometheus.Collector for the state gauges.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.clusters
	ch <- m.affiliates
	ch <- m.endpoints
	ch <- m.subscriptions
}

// Collect implements prometheus.Collector for the state gauges.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	st := m.state.Stats()
	ch <- prometheus.MustNewConstMetric(m.clusters, prometheus.GaugeValue, float64(st.Clusters))
	ch <- prometheus.MustNewConstMetric(m.affiliates, prometheus.GaugeValue, float64(st.Affiliates))
	ch <- prometheus.MustNewConstMetric(m.endpoints, prometheus.GaugeValue, float64(st.Endpoints))
	ch <- prometheus.MustNewConstMetric(m.subscriptions, prometheus.GaugeValue, float64(st.Subscriptions))
}

// ObserveGC records one garbage collection run.
func (m *Metrics) ObserveGC(res state.GCResult) {
	m.gcRuns.Inc()
	m.gcClusters.Add(float64(res.Clusters))
	m.gcAffiliates.Add(float64(res.Affiliates))
}

// ObserveStorage records one snapshot save or load.
func (m *Metrics) ObserveStorage(operation string, st state.SnapshotStats, took time.Duration, err error) {
	if err != nil {
		m.storageOps.WithLabelValues(operation, statusError).Inc()
		return
	}
	m.storageOps.WithLabelValues(operation, statusSuccess).Inc()
	m.storageSize.WithLabelValues(operation).Set(float64(st.Bytes))
	m.storageClusters.WithLabelValues(operation).Set(float64(st.Clusters))
	m.storageAffiliates.WithLabelValues(operation).Set(float64(st.Affiliates))
	m.storageEndpoints.WithLabelValues(operation).Set(float64(st.Endpoints))
	m.storageDuration.WithLabelValues(operation).Set(took.Seconds())
}

func (m *Metrics) observeHello(clientVersion string) {
	m.hellos.WithLabelValues(versionLabel(clientVersion)).Inc()
}

const unknownVersion = "unknown"

// versionLabel reduces a Talos version to "v<major>.<minor>", with "-pre" for
// pre-releases: "v1.11.3" is "v1.11", "v1.12.0-beta.1" is "v1.12-pre". The
// field is free text from the client, so anything else is "unknown"; used
// verbatim, one client sending random strings would create a series per
// request. These are the upstream label values, so a version panel built for
// either service groups the same way.
func versionLabel(v string) string {
	ver, suffix, _ := strings.Cut(v, "-")
	if ver == "" || ver[0] != 'v' {
		return unknownVersion
	}

	major, rest, ok := strings.Cut(ver[1:], ".")
	if !ok || !digits(major, 3) {
		return unknownVersion
	}
	minor, _, ok := strings.Cut(rest, ".")
	if !ok || !digits(minor, 3) {
		return unknownVersion
	}

	label := "v" + major + "." + minor
	if suffix != "" {
		label += "-pre"
	}
	return label
}

func digits(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
