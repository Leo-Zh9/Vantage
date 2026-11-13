package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

func reportSample() analytics.Snapshot {
	s := sample()
	s.StartedUnix = time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC).Unix()
	s.GeneratedUnix = time.Date(2026, 11, 12, 18, 30, 0, 0, time.UTC).Unix()
	s.Today = analytics.DayRow{Date: "2026-11-12", Visitors: 14, PageViews: 31, BotRequests: 220, ScannerProbes: 41}
	s.History = []analytics.DayRow{{Date: "2026-11-11", Visitors: 9, PageViews: 17, BotRequests: 1234}}
	s.TopPages = []analytics.KeyCount{{Key: "/", Count: 20}, {Key: "/projects", Count: 11}}
	s.TopReferrers = []analytics.KeyCount{{Key: "linkedin.com", Count: 6}}
	s.TopProbes = []analytics.KeyCount{{Key: "/wp-login.php", Count: 30}, {Key: "/x|`y`\n", Count: 1}}
	s.TopIPs = []analytics.KeyCount{{Key: "203.0.113.77", Count: 999}}
	return s
}

func TestMarkdownReport(t *testing.T) {
	md := Markdown(reportSample(), ReportOptions{Site: "www.leo-zhang.com"})
	for _, want := range []string{
		"# Traffic: www.leo-zhang.com",
		"| **Thu Nov 12 (today)** | 14 | 31 | 220 | 41 | 0 |",
		"| Wed Nov 11 | 9 | 17 | 1,234 | 0 | 0 |",
		"| `/` | 20 |",
		"| `linkedin.com` | 6 |",
		"| `/wp-login.php` | 30 |",
		"| `/x\\|'y' ` | 1 |", // pipe escaped, backticks and newline neutralised
		"| Requests proxied | 4,200 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q\n--- report ---\n%s", want, md)
		}
	}
	if strings.Contains(md, "203.0.113.77") {
		t.Errorf("report must not contain IP addresses")
	}
}

func TestReportRequiresPassword(t *testing.T) {
	h := NewReport(fakeStats{reportSample()}, ReportOptions{Site: "x"}, "s3cret")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_vantage/report", nil))
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no credentials: code=%d, want 401 with a login prompt", rr.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/_vantage/report", nil)
	req.SetBasicAuth("leo", "wrong")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: code=%d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/_vantage/report", nil)
	req.SetBasicAuth("leo", "s3cret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "## By day") {
		t.Errorf("right password: code=%d body=%q", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("report must not be cached")
	}
}
