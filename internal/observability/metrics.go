package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics is a dependency-free Prometheus text registry for the small set of
// operational signals promised by the application contract. Labels are always
// bounded enums/IDs chosen by the runtime; request bodies, tokens, model output
// and application payloads are never used as labels.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]map[string]uint64
	hists    map[string]*histogram
}

type histogram struct {
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

var metrics = NewMetrics()

// NewMetrics returns an empty registry (tests can use it without globals).
func NewMetrics() *Metrics {
	return &Metrics{counters: map[string]map[string]uint64{}, hists: map[string]*histogram{}}
}

// Default returns the process-wide registry used by API and worker.
func Default() *Metrics { return metrics }

func canonicalLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+`="`+escapeLabel(labels[key])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

func metricKey(name string, labels map[string]string) string {
	return name + canonicalLabels(labels)
}

// Inc increments a counter by one.
func (m *Metrics) Inc(name string, labels map[string]string) { m.Add(name, labels, 1) }

// Add increments a counter by delta.
func (m *Metrics) Add(name string, labels map[string]string, delta uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := metricKey(name, labels)
	if m.counters[key] == nil {
		m.counters[key] = map[string]uint64{"_value": 0}
	}
	m.counters[key]["_value"] += delta
}

// Observe records a duration in seconds.
func (m *Metrics) Observe(name string, labels map[string]string, duration time.Duration) {
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := metricKey(name, labels)
	h := m.hists[key]
	if h == nil {
		h = &histogram{buckets: append([]float64(nil), defaultBuckets...), counts: make([]uint64, len(defaultBuckets)+1)}
		m.hists[key] = h
	}
	for i, bucket := range h.buckets {
		if seconds <= bucket {
			h.counts[i]++
		}
	}
	h.counts[len(h.buckets)]++
	h.sum += seconds
	h.count++
}

// Handler returns a Prometheus text exposition endpoint.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.mu.Lock()
		defer m.mu.Unlock()
		writeCounters(w, m.counters)
		writeHistograms(w, m.hists)
	})
}

// MetricsHandler exposes the process-wide registry.
func MetricsHandler() http.Handler { return metrics.Handler() }

func writeCounters(w http.ResponseWriter, counters map[string]map[string]uint64) {
	type sample struct {
		name   string
		labels string
		value  uint64
	}
	byName := map[string][]sample{}
	for key, values := range counters {
		name, labels := splitMetricKey(key)
		byName[name] = append(byName[name], sample{name: name, labels: labels, value: values["_value"]})
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		samples := byName[name]
		sort.Slice(samples, func(i, j int) bool { return samples[i].labels < samples[j].labels })
		fmt.Fprintf(w, "# TYPE %s counter\n", name)
		for _, sample := range samples {
			fmt.Fprintf(w, "%s%s %d\n", name, sample.labels, sample.value)
		}
	}
}

func writeHistograms(w http.ResponseWriter, hists map[string]*histogram) {
	byName := map[string][]string{}
	for key := range hists {
		name, labels := splitMetricKey(key)
		byName[name] = append(byName[name], labels)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		labelSets := byName[name]
		sort.Strings(labelSets)
		fmt.Fprintf(w, "# TYPE %s histogram\n", name)
		for _, labels := range labelSets {
			h := hists[name+labels]
			for i, bucket := range h.buckets {
				fmt.Fprintf(w, "%s_bucket%s %d\n", name, mergeLabel(labels, "le", strconv.FormatFloat(bucket, 'g', -1, 64)), h.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", name, mergeLabel(labels, "le", "+Inf"), h.counts[len(h.buckets)])
			fmt.Fprintf(w, "%s_sum%s %s\n", name, labels, strconv.FormatFloat(h.sum, 'g', -1, 64))
			fmt.Fprintf(w, "%s_count%s %d\n", name, labels, h.count)
		}
	}
}

func mergeLabel(labels, key, value string) string {
	if labels == "" {
		return `{` + key + `="` + value + `"}`
	}
	return labels[:len(labels)-1] + `,` + key + `="` + value + `"}`
}

func splitMetricKey(key string) (string, string) {
	index := strings.IndexByte(key, '{')
	if index < 0 {
		return key, ""
	}
	return key[:index], key[index:]
}

// RecordHTTPError classifies API failures without recording tenants or payloads.
func RecordHTTPError(status int, errorCode string) {
	metrics.Inc("chronotope_http_errors_total", map[string]string{
		"status":     strconv.Itoa(status),
		"error_code": errorCode,
	})
}

// RecordRunTerminal records run latency and terminal classification.
func RecordRunTerminal(status, errorCode, category string, retryable bool, duration time.Duration) {
	labels := map[string]string{
		"status":     status,
		"error_code": errorCode,
		"category":   category,
		"retryable":  strconv.FormatBool(retryable),
	}
	metrics.Inc("chronotope_run_terminal_total", labels)
	if duration > 0 {
		metrics.Observe("chronotope_run_duration_seconds", map[string]string{"status": status}, duration)
	}
}

// RecordProviderCall records one provider/harness attempt latency.
func RecordProviderCall(outcome string, duration time.Duration) {
	metrics.Observe("chronotope_provider_call_duration_seconds", map[string]string{"outcome": outcome}, duration)
}

// RecordProviderError records normalized provider/harness failures.
func RecordProviderError(code string, duration time.Duration) {
	metrics.Inc("chronotope_provider_errors_total", map[string]string{"code": code})
	RecordProviderCall(code, duration)
}

// RecordToolBridge records one actual application bridge HTTP attempt.
func RecordToolBridge(outcome, errorCode string, duration time.Duration) {
	metrics.Inc("chronotope_tool_bridge_total", map[string]string{"outcome": outcome, "error_code": errorCode})
	metrics.Observe("chronotope_tool_bridge_duration_seconds", map[string]string{"outcome": outcome}, duration)
}

// RecordTimerFire records one timer delivery decision.
func RecordTimerFire(state, errorCode string, duration time.Duration) {
	metrics.Inc("chronotope_timer_fire_total", map[string]string{"state": state, "error_code": errorCode})
	metrics.Observe("chronotope_timer_fire_duration_seconds", map[string]string{"state": state}, duration)
}

// RecordTimerAdmission records durable timer admission outcomes.
func RecordTimerAdmission(outcome string) {
	metrics.Inc("chronotope_timer_admission_total", map[string]string{"outcome": outcome})
}

// RecordTargetChange records dynamic target enrollment/update/revocation.
func RecordTargetChange(kind, action string) {
	metrics.Inc("chronotope_application_target_changes_total", map[string]string{"kind": kind, "action": action})
}
