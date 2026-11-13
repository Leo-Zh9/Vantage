package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// maxHistoryDays bounds the persisted per-day history.
const maxHistoryDays = 90

// DayRow is one calendar day of site traffic. Visitors is a HyperLogLog
// estimate of distinct human IPs; no IP addresses are stored.
type DayRow struct {
	Date          string `json:"date"` // YYYY-MM-DD in the configured time zone
	Visitors      uint64 `json:"visitors"`
	PageViews     uint64 `json:"page_views"`
	Requests      uint64 `json:"requests"`
	BotRequests   uint64 `json:"bot_requests"`
	ScannerProbes uint64 `json:"scanner_probes"`
	RateLimited   uint64 `json:"rate_limited"`
}

// daily keeps today's counters plus a rolling history of finished days. It is
// owned by the engine's single writer goroutine.
type daily struct {
	loc      *time.Location
	today    DayRow
	visitors *HyperLogLog // distinct human IPs today
	history  []DayRow     // finished days, oldest first
}

func newDaily(loc *time.Location, now time.Time) *daily {
	if loc == nil {
		loc = time.UTC
	}
	return &daily{
		loc:      loc,
		today:    DayRow{Date: now.In(loc).Format(time.DateOnly)},
		visitors: NewHyperLogLog(14),
	}
}

// roll closes out today if the calendar date has changed.
func (d *daily) roll(now time.Time) {
	date := now.In(d.loc).Format(time.DateOnly)
	if date == d.today.Date {
		return
	}
	d.history = append(d.history, d.snapshotToday())
	if len(d.history) > maxHistoryDays {
		d.history = d.history[len(d.history)-maxHistoryDays:]
	}
	d.today = DayRow{Date: date}
	d.visitors.Reset()
}

// snapshotToday returns today's row with the visitor estimate filled in.
func (d *daily) snapshotToday() DayRow {
	row := d.today
	row.Visitors = uint64(d.visitors.Estimate() + 0.5)
	return row
}

// recentHistory returns up to n finished days, newest first.
func (d *daily) recentHistory(n int) []DayRow {
	out := make([]DayRow, 0, n)
	for i := len(d.history) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, d.history[i])
	}
	return out
}

// persisted is the on-disk form. Today's HyperLogLog registers are saved so a
// restart or deploy mid-day doesn't reset the visitor count.
type persisted struct {
	Version      int      `json:"version"`
	Today        DayRow   `json:"today"`
	TodayHLL     []byte   `json:"today_hll"`
	History      []DayRow `json:"history"`
	SavedUnixSec int64    `json:"saved_unix"`
}

// save writes the state atomically (temp file + rename), so a crash mid-write
// never leaves a truncated file behind.
func (d *daily) save(path string) error {
	data, err := json.Marshal(persisted{
		Version:      1,
		Today:        d.today,
		TodayHLL:     d.visitors.registers,
		History:      d.history,
		SavedUnixSec: time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data)
}

// load restores state saved by save. A missing file is not an error. Saved
// "today" counters are only restored if they are for the current date;
// otherwise they are closed out into history.
func (d *daily) load(path string, now time.Time) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	d.history = p.History
	if p.Today.Date == "" {
		return nil
	}
	saved := NewHyperLogLog(14)
	if len(p.TodayHLL) == len(saved.registers) {
		copy(saved.registers, p.TodayHLL)
	}
	if p.Today.Date == now.In(d.loc).Format(time.DateOnly) {
		d.today = p.Today
		d.visitors = saved
		return nil
	}
	closed := p.Today
	closed.Visitors = uint64(saved.Estimate() + 0.5)
	d.history = append(d.history, closed)
	if len(d.history) > maxHistoryDays {
		d.history = d.history[len(d.history)-maxHistoryDays:]
	}
	return nil
}

// WriteFileAtomic replaces path with data via a temp file in the same
// directory and a rename, so readers never see a half-written file.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vantage-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
