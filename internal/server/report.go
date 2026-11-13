package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

// ReportOptions controls how the Markdown report is labelled.
type ReportOptions struct {
	Site     string         // shown in the title, e.g. "www.leo-zhang.com"
	Location *time.Location // time zone for dates; nil means UTC
	Days     int            // finished days to list; 0 means 14
}

// Markdown renders a human-readable traffic report as GitHub-flavoured
// Markdown tables. It contains no IP addresses: visitors are HyperLogLog
// estimates, and the report lists pages, referrers and probed paths only.
func Markdown(s analytics.Snapshot, o ReportOptions) string {
	loc := o.Location
	if loc == nil {
		loc = time.UTC
	}
	days := o.Days
	if days <= 0 {
		days = 14
	}
	started := time.Unix(s.StartedUnix, 0).In(loc)
	generated := time.Unix(s.GeneratedUnix, 0).In(loc)

	var b strings.Builder
	fmt.Fprintf(&b, "# Traffic: %s\n\n", o.Site)
	fmt.Fprintf(&b, "Updated %s · times in %s\n\n", generated.Format("Mon Jan 2, 3:04 PM"), loc.String())

	b.WriteString("## By day\n\n")
	b.WriteString("| Day | Visitors | Page views | Bot requests | Scanner probes | Rate-limited |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|\n")
	dayRow(&b, s.Today, true)
	hist := s.History
	if len(hist) > days {
		hist = hist[:days]
	}
	for _, d := range hist {
		dayRow(&b, d, false)
	}
	b.WriteString("\nVisitors are estimated unique people (by IP, ±2%), not counting bots. ")
	b.WriteString("A page view is a page someone opened; scripts, images, prefetches and bots are excluded.\n\n")

	since := "since " + started.Format("Jan 2, 3:04 PM")
	fmt.Fprintf(&b, "## Top pages (%s)\n\n", since)
	table(&b, "Page", "Views", s.TopPages, "no page views yet")
	fmt.Fprintf(&b, "## Where visitors came from (%s)\n\n", since)
	table(&b, "Referrer", "Visits", s.TopReferrers, "no outside referrers yet (direct visits don't send one)")
	fmt.Fprintf(&b, "## What scanners probed (%s)\n\n", since)
	table(&b, "Path", "Requests", s.TopProbes, "no scanner probes yet")

	b.WriteString("## Health\n\n| Metric | Value |\n|---|---:|\n")
	fmt.Fprintf(&b, "| Requests proxied | %s |\n", num(s.TotalRequests))
	fmt.Fprintf(&b, "| Latency p50 / p95 / p99 | %.0f / %.0f / %.0f ms |\n", s.LatencyP50, s.LatencyP95, s.LatencyP99)
	fmt.Fprintf(&b, "| Server errors (5xx) | %s |\n", num(s.StatusClasses["5xx"]))
	fmt.Fprintf(&b, "| IPs blocked right now | %d |\n", s.BlockedCount)
	fmt.Fprintf(&b, "| Requests rate-limited | %s |\n", num(s.ThrottledTotal))
	fmt.Fprintf(&b, "| Analytics events dropped | %s |\n", num(s.Dropped))
	fmt.Fprintf(&b, "| Sketch memory | %d KB |\n", s.MemoryBytes/1024)
	fmt.Fprintf(&b, "| Running since | %s |\n", started.Format("Jan 2, 3:04 PM"))
	return b.String()
}

func dayRow(b *strings.Builder, d analytics.DayRow, today bool) {
	label := d.Date
	if t, err := time.Parse(time.DateOnly, d.Date); err == nil {
		label = t.Format("Mon Jan 2")
	}
	if today {
		label = "**" + label + " (today)**"
	}
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", label,
		num(d.Visitors), num(d.PageViews), num(d.BotRequests), num(d.ScannerProbes), num(d.RateLimited))
}

func table(b *strings.Builder, keyHead, countHead string, rows []analytics.KeyCount, empty string) {
	if len(rows) == 0 {
		fmt.Fprintf(b, "_%s_\n\n", empty)
		return
	}
	fmt.Fprintf(b, "| %s | %s |\n|---|---:|\n", keyHead, countHead)
	for _, kc := range rows {
		fmt.Fprintf(b, "| %s | %s |\n", cell(kc.Key), num(kc.Count))
	}
	b.WriteString("\n")
}

// cell makes an arbitrary string safe inside a Markdown table cell: it is shown
// as inline code, truncated, with pipes escaped and backticks/newlines removed,
// so a crafted URL can't break the table or inject Markdown.
func cell(s string) string {
	if r := []rune(s); len(r) > 80 {
		s = string(r[:77]) + "..."
	}
	s = strings.NewReplacer("`", "'", "|", `\|`, "\n", " ", "\r", " ").Replace(s)
	return "`" + s + "`"
}

// num formats a count with thousands separators.
func num(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// NewReport serves the Markdown report. If password is non-empty the request
// must carry it via HTTP Basic auth (any username), which is what makes it safe
// to expose on the public data plane; browsers show a login prompt.
func NewReport(s Snapshotter, o ReportOptions, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password != "" {
			_, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="vantage", charset="UTF-8"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8") // shown inline, not downloaded
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Robots-Tag", "noindex")
		_, _ = w.Write([]byte(Markdown(s.Snapshot(), o)))
	})
}
