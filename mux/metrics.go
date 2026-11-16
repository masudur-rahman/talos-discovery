package mux

import (
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics counts connections on the main listener. The names and labels are
// the upstream service's, so a dashboard built for one reads the other.
type Metrics struct {
	opened        *prometheus.CounterVec
	closed        *prometheus.CounterVec
	active        *prometheus.GaugeVec
	rejectedTotal prometheus.Counter
}

// NewMetrics registers the connection series on reg.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		opened: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "discovery_connections_opened_total",
			Help: "Connections accepted on the main listener, by protocol.",
		}, []string{"protocol"}),
		closed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "discovery_connections_closed_total",
			Help: "Connections closed on the main listener, by protocol.",
		}, []string{"protocol"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "discovery_connections_active",
			Help: "Open connections on the main listener, by protocol.",
		}, []string{"protocol"}),
		rejectedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "discovery_connections_rejected_total",
			Help: "Connections that never reached a server: failed TLS handshake or protocol detection, or arrived during shutdown.",
		}),
	}
	// Create every labelled series up front, so a dashboard shows zero
	// rather than "no data" for a protocol nobody has used yet.
	for _, p := range []Protocol{ProtocolGRPC, ProtocolHTTP} {
		m.opened.WithLabelValues(p.String())
		m.closed.WithLabelValues(p.String())
		m.active.WithLabelValues(p.String())
	}
	for _, c := range []prometheus.Collector{m.opened, m.closed, m.active, m.rejectedTotal} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Metrics) rejected() {
	if m != nil {
		m.rejectedTotal.Inc()
	}
}

// track counts conn as opened now and closed on its first Close, however many
// times the server closes it.
func (m *Metrics) track(p Protocol, conn net.Conn) net.Conn {
	if m == nil {
		return conn
	}
	label := p.String()
	m.opened.WithLabelValues(label).Inc()
	m.active.WithLabelValues(label).Inc()
	return &trackedConn{Conn: conn, onClose: func() {
		m.closed.WithLabelValues(label).Inc()
		m.active.WithLabelValues(label).Dec()
	}}
}

type trackedConn struct {
	net.Conn
	onClose func()
	once    sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.onClose)
	return err
}
