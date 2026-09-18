package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsPrometheus(t *testing.T) {
	m := NewMetrics()
	m.FilesScannedTotal.Add(42)
	m.RecordVMAF(95.5)

	out := m.PrometheusFormat()
	if !strings.Contains(out, "mediacruncher_files_scanned_total 42") {
		t.Errorf("expected scanned total 42, got:\n%s", out)
	}
	if !strings.Contains(out, "mediacruncher_vmaf_score_average 95.50") {
		t.Errorf("expected vmaf average 95.50, got:\n%s", out)
	}
}

func TestHealthEndpoint(t *testing.T) {
	m := NewMetrics()
	srv := StartHTTPServer(0, m) // just test handler directly
	defer srv.Close()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"healthy"`) {
		t.Errorf("expected healthy status, got: %s", w.Body.String())
	}
}
