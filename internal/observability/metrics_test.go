package observability

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExpositionAndLabelEscaping(t *testing.T) {
	metrics := NewMetrics()
	metrics.Inc("chronotope_test_total", map[string]string{"status": "ok", "quote": `a"b`})
	metrics.Observe("chronotope_test_duration_seconds", map[string]string{"outcome": "success"}, 120*time.Millisecond)
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics status: %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	for _, needle := range []string{
		`chronotope_test_total{quote="a\"b",status="ok"} 1`,
		`chronotope_test_duration_seconds_bucket{outcome="success",le="0.1"} 0`,
		`chronotope_test_duration_seconds_bucket{outcome="success",le="0.25"} 1`,
		`chronotope_test_duration_seconds_count{outcome="success"} 1`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("metrics exposition missing %q:\n%s", needle, text)
		}
	}
	if strings.Contains(text, "Bearer") || strings.Contains(text, "token=") {
		t.Fatal("metrics must not contain credentials")
	}
}

func TestRecordHTTPErrorUsesBoundedLabels(t *testing.T) {
	RecordHTTPError(422, "validation_failed")
	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `chronotope_http_errors_total{error_code="validation_failed",status="422"}`) {
		t.Fatalf("http error metric missing:\n%s", rec.Body.String())
	}
}
