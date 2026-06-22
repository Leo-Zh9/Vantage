package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

type capture struct{ events []analytics.Event }

func (c *capture) Observe(e analytics.Event) { c.events = append(c.events, e) }

func TestProxyForwardsAndObserves(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello world")
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	obs := &capture{}
	front := httptest.NewServer(New(target, obs))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/widgets?id=1", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if string(body) != "hello world" {
		t.Errorf("body = %q", body)
	}
	if len(obs.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(obs.events))
	}
	ev := obs.events[0]
	if ev.IP != "203.0.113.7" {
		t.Errorf("client ip = %q, want 203.0.113.7 (first X-Forwarded-For hop)", ev.IP)
	}
	if ev.Path != "/api/widgets" {
		t.Errorf("path = %q, want /api/widgets", ev.Path)
	}
	if ev.Status != http.StatusCreated {
		t.Errorf("status = %d, want 201", ev.Status)
	}
	if ev.Bytes != int64(len("hello world")) {
		t.Errorf("bytes = %d, want %d", ev.Bytes, len("hello world"))
	}
	if ev.Latency <= 0 {
		t.Errorf("latency not recorded")
	}
}

func TestProxyBadGatewayOnDeadBackend(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1") // nothing is listening here
	obs := &capture{}
	front := httptest.NewServer(New(target, obs))
	defer front.Close()

	resp, err := http.Get(front.URL + "/down")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if len(obs.events) != 1 || obs.events[0].Status != http.StatusBadGateway {
		t.Fatalf("expected one 502 event, got %+v", obs.events)
	}
}

func TestProxyRewritesHostToBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host) // echo the Host the origin received
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	front := httptest.NewServer(New(target, &capture{}))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got := resp.Header.Get("X-Seen-Host"); got != target.Host {
		t.Errorf("backend saw Host %q, want the origin host %q", got, target.Host)
	}
}
