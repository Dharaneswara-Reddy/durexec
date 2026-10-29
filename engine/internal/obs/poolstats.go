package obs

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PoolStats is a snapshot of one connection pool, in terms this package can
// report without knowing what driver produced it.
//
// It exists so that obs does not import pgxpool. The engine's pool type is a
// detail of cmd/engined; obs only needs the numbers, and a plain struct plus a
// sampling function is the whole contract. This is the same shape as the
// one-method Observer interface that keeps internal/idem/semantic free of
// Prometheus.
type PoolStats struct {
	// Current occupancy.
	AcquiredConns int32
	IdleConns     int32
	TotalConns    int32
	MaxConns      int32

	// Cumulative. EmptyAcquireCount is the pool-pressure signal that matters:
	// it counts acquisitions that had to WAIT because every connection was
	// busy. TotalConns reaching MaxConns only says the pool is full, which is
	// normal and healthy under load; EmptyAcquireCount rising says work is
	// now queueing behind connections, which is the first thing to go wrong
	// when offered load passes what the database can serve.
	AcquireCount         int64
	EmptyAcquireCount    int64
	CanceledAcquireCount int64
	TotalAcquireWait     time.Duration
}

// RegisterPoolStats reports one pool's statistics on reg, sampling it through
// sample at scrape time.
//
// Every collector here is a GaugeFunc or CounterFunc rather than a gauge the
// engine has to remember to update. That is deliberate: there is no hook on
// the pool to subscribe to, so a self-updating gauge would need a ticker
// goroutine, and a ticker that stops leaves a stale value behind that looks
// exactly like a real measurement. Sampling at scrape time cannot go stale,
// costs nothing when nobody scrapes, and keeps the hot path untouched.
//
// Counters are declared as counters, not gauges, so that rate() over them is
// meaningful and a process restart is handled as a counter reset rather than
// read as a sudden drop to zero.
func RegisterPoolStats(reg prometheus.Registerer, sample func() PoolStats) {
	type gdef struct {
		name string
		help string
		get  func(PoolStats) float64
	}
	gauges := []gdef{
		{"db_pool_conns_acquired", "Connections currently checked out of the pool.",
			func(s PoolStats) float64 { return float64(s.AcquiredConns) }},
		{"db_pool_conns_idle", "Connections currently open and unused.",
			func(s PoolStats) float64 { return float64(s.IdleConns) }},
		{"db_pool_conns_total", "Connections currently open, busy or idle.",
			func(s PoolStats) float64 { return float64(s.TotalConns) }},
		{"db_pool_conns_max", "Configured ceiling on open connections for this node.",
			func(s PoolStats) float64 { return float64(s.MaxConns) }},
	}
	for _, g := range gauges {
		get := g.get
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: g.name, Help: g.help,
		}, func() float64 { return get(sample()) }))
	}

	counters := []gdef{
		{"db_pool_acquires_total", "Connection acquisitions from the pool.",
			func(s PoolStats) float64 { return float64(s.AcquireCount) }},
		{"db_pool_empty_acquires_total", "Acquisitions that had to wait because the pool was exhausted.",
			func(s PoolStats) float64 { return float64(s.EmptyAcquireCount) }},
		{"db_pool_canceled_acquires_total", "Acquisitions abandoned before a connection became free.",
			func(s PoolStats) float64 { return float64(s.CanceledAcquireCount) }},
		{"db_pool_acquire_wait_seconds_total", "Cumulative time spent waiting for a connection.",
			func(s PoolStats) float64 { return s.TotalAcquireWait.Seconds() }},
	}
	for _, c := range counters {
		get := c.get
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: c.name, Help: c.help,
		}, func() float64 { return get(sample()) }))
	}
}
