package analytics

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func toronto(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	return loc
}

func TestDailyRollsOverAtLocalMidnight(t *testing.T) {
	loc := toronto(t)
	start := time.Date(2026, 11, 12, 23, 59, 0, 0, loc)
	d := newDaily(loc, start)
	for i := 0; i < 50; i++ {
		d.visitors.Add(fmt.Sprintf("10.0.0.%d", i))
	}
	d.today.PageViews = 70

	d.roll(start.Add(30 * time.Second)) // still Nov 12 in Toronto
	if len(d.history) != 0 {
		t.Fatalf("rolled over before midnight")
	}
	d.roll(start.Add(2 * time.Minute)) // 00:01 Nov 13 in Toronto (05:01 UTC)
	if len(d.history) != 1 {
		t.Fatalf("expected one finished day, got %d", len(d.history))
	}
	got := d.history[0]
	if got.Date != "2026-11-12" || got.PageViews != 70 || got.Visitors < 48 || got.Visitors > 52 {
		t.Errorf("finished day = %+v, want 2026-11-12 with 70 views and ~50 visitors", got)
	}
	if d.today.Date != "2026-11-13" || d.today.PageViews != 0 || d.visitors.Estimate() != 0 {
		t.Errorf("new day not reset: %+v", d.today)
	}
}

func TestDailyPersistsAcrossRestart(t *testing.T) {
	loc := toronto(t)
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, loc)

	d := newDaily(loc, now)
	for i := 0; i < 300; i++ {
		d.visitors.Add(fmt.Sprintf("192.0.2.%d/%d", i%256, i))
	}
	d.today.PageViews = 412
	d.history = []DayRow{{Date: "2026-11-11", Visitors: 9, PageViews: 20}}
	if err := d.save(path); err != nil {
		t.Fatal(err)
	}

	// Same day: today's counters and visitor sketch come back.
	same := newDaily(loc, now.Add(time.Hour))
	if err := same.load(path, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if same.today.PageViews != 412 || len(same.history) != 1 {
		t.Errorf("restored %+v history=%d, want 412 views and 1 day", same.today, len(same.history))
	}
	if v := same.snapshotToday().Visitors; v < 290 || v > 310 {
		t.Errorf("restored visitors = %d, want ~300", v)
	}

	// Next day: the saved "today" is closed out into history instead.
	next := newDaily(loc, now.Add(24*time.Hour))
	if err := next.load(path, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(next.history) != 2 || next.history[1].Date != "2026-11-12" || next.today.PageViews != 0 {
		t.Errorf("stale today not closed out: history=%+v today=%+v", next.history, next.today)
	}
}

func TestDailyLoadMissingFileIsFine(t *testing.T) {
	d := newDaily(time.UTC, time.Now())
	if err := d.load(filepath.Join(t.TempDir(), "nope.json"), time.Now()); err != nil {
		t.Errorf("missing state file should not be an error: %v", err)
	}
}
