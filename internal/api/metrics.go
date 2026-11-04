package api

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/obleey/omegacd/internal/domain"
)

var (
	syncOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "omegacd_sync_operations_total",
		Help: "Finished Sync Operations by Application and phase.",
	}, []string{"app", "phase"})
	syncDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "omegacd_sync_duration_seconds",
		Help:    "Duration of Sync Operations.",
		Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"app"})
	appInfoDesc = prometheus.NewDesc("omegacd_app_info", "Application Sync and Health Status (always 1).",
		[]string{"app", "sync_status", "health"}, nil)
)

// ObserveSync records a finished Sync Operation.
func ObserveSync(app string, phase domain.OpPhase, d time.Duration) {
	syncOps.WithLabelValues(app, string(phase)).Inc()
	syncDuration.WithLabelValues(app).Observe(d.Seconds())
}

type appInfo struct{ ctrl domain.Controller }

func (c appInfo) Describe(ch chan<- *prometheus.Desc) { ch <- appInfoDesc }

func (c appInfo) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	apps, err := c.ctrl.ListApps(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(appInfoDesc, err)
		return
	}
	for _, a := range apps {
		ch <- prometheus.MustNewConstMetric(appInfoDesc, prometheus.GaugeValue, 1, a.Name, string(a.SyncStatus), string(a.Health))
	}
}

func metricsHandler(ctrl domain.Controller) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(syncOps, syncDuration, appInfo{ctrl},
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
