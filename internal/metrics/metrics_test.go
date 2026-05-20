package metrics

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/solcreek/creekd/internal/supervisor"
)

func quietSupervisor() *supervisor.Supervisor {
	sup := supervisor.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.HealthCheckInterval = 0
	return sup
}

// TestScrapeFormatPrometheus: scraping the handler yields valid
// Prometheus text exposition that contains the metrics we expect
// for an empty supervisor.
func TestScrapeFormatPrometheus(t *testing.T) {
	m := New(quietSupervisor(), "v0.test")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	// Daemon-level metrics with constant labels must appear (even at
	// zero) so dashboards can pin label sets in advance.
	for _, want := range []string{
		"creekd_build_info",
		"creekd_apps",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("scrape missing metric %q\nbody:\n%s", want, text)
		}
	}
	// Vector metrics (dispatch_*) appear only when at least one labeled
	// series has been recorded — that's standard Prom CounterVec
	// behaviour, see TestObserveDispatch for the populated case.

	// Build info must include the version label and the value 1.
	if !strings.Contains(text, `creekd_build_info{version="v0.test"} 1`) {
		t.Errorf("creekd_build_info row missing/malformed:\n%s", text)
	}

	// Apps gauge should emit every known status (zero-valued is fine
	// — dashboards need stable label sets).
	for _, status := range []string{"running", "crashed", "stopped"} {
		row := `creekd_apps{status="` + status + `"} 0`
		if !strings.Contains(text, row) {
			t.Errorf("missing apps row %q:\n%s", row, text)
		}
	}
}

// TestObserveDispatch: ObserveDispatch counts bytes and requests
// against the right app_id + status code labels.
func TestObserveDispatch(t *testing.T) {
	m := New(quietSupervisor(), "v0")

	m.ObserveDispatch("a", 100, 200)
	m.ObserveDispatch("a", 250, 200)
	m.ObserveDispatch("a", 0, 404)        // bytes=0 still counts the request
	m.ObserveDispatch("b", 1024, 500)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	want := []string{
		`creekd_dispatch_bytes_sent_total{app_id="a"} 350`,
		`creekd_dispatch_bytes_sent_total{app_id="b"} 1024`,
		`creekd_dispatch_requests_total{app_id="a",code="200"} 2`,
		`creekd_dispatch_requests_total{app_id="a",code="404"} 1`,
		`creekd_dispatch_requests_total{app_id="b",code="500"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("scrape missing row %q\nbody:\n%s", w, text)
		}
	}
}

// TestOpenMetricsNegotiation: when the client sends an Accept header
// for OpenMetrics, the handler responds with that content-type. This
// is the "OTel collector can scrape us cleanly" guard.
func TestOpenMetricsNegotiation(t *testing.T) {
	m := New(quietSupervisor(), "v0")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Errorf("Content-Type = %q, want application/openmetrics-text*", ct)
	}
}
