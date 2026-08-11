package event

import (
	"testing"
	"time"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/models"
)

func weekday(d time.Weekday) *int {
	v := int(d)
	return &v
}

// istAt builds an IST wall-clock instant, matching how hosts enter times.
func istAt(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, timeutil.IST)
}

func TestGenerateWeeklySessions_PacksWindowAndRepeatsWeekly(t *testing.T) {
	// Wednesday 12 Aug 2026, 09:00 IST.
	from := istAt(2026, time.August, 12, 9, 0)
	windows := []models.SessionWindow{
		{Weekday: weekday(time.Monday), Start: "10:00", End: "12:00"},
	}

	got := GenerateWeeklySessions(windows, 30, 0, from)

	// 4 sessions per Monday, over a 4-week horizon starting mid-week: Mondays
	// 17 & 24 Aug and 7 Sep fall inside, plus 31 Aug.
	if len(got) != 16 {
		t.Fatalf("expected 16 sessions (4 Mondays x 4), got %d", len(got))
	}
	want := istAt(2026, time.August, 17, 10, 0)
	if !got[0].Equal(want) {
		t.Errorf("first session = %s, want %s", got[0], want)
	}
	// Sessions are back to back at 0 break.
	if !got[1].Equal(istAt(2026, time.August, 17, 10, 30)) {
		t.Errorf("second session = %s, want 10:30 IST", got[1])
	}
	// The window is not overrun: the last of the day starts at 11:30.
	if !got[3].Equal(istAt(2026, time.August, 17, 11, 30)) {
		t.Errorf("fourth session = %s, want 11:30 IST", got[3])
	}
}

func TestGenerateWeeklySessions_BreakBetweenSessionsOnly(t *testing.T) {
	from := istAt(2026, time.August, 12, 9, 0)
	windows := []models.SessionWindow{
		{Weekday: weekday(time.Monday), Start: "10:00", End: "12:00"},
	}

	// 30-minute sessions with a 15-minute break: 10:00, 10:45, 11:30 — three fit,
	// the trailing break after the last one doesn't have to fit.
	got := GenerateWeeklySessions(windows, 30, 15, from)

	firstMonday := got[:3]
	wantStarts := []time.Time{
		istAt(2026, time.August, 17, 10, 0),
		istAt(2026, time.August, 17, 10, 45),
		istAt(2026, time.August, 17, 11, 30),
	}
	for i, want := range wantStarts {
		if !firstMonday[i].Equal(want) {
			t.Errorf("session %d = %s, want %s", i, firstMonday[i], want)
		}
	}
	if got[3].Equal(istAt(2026, time.August, 17, 12, 15)) {
		t.Error("generated a session past the end of the window")
	}
}

func TestGenerateWeeklySessions_SkipsSessionsAlreadyStarted(t *testing.T) {
	// Monday 17 Aug, 10:40 IST — the 10:00 and 10:30 sessions have begun.
	from := istAt(2026, time.August, 17, 10, 40)
	windows := []models.SessionWindow{
		{Weekday: weekday(time.Monday), Start: "10:00", End: "12:00"},
	}

	got := GenerateWeeklySessions(windows, 30, 0, from)

	want := istAt(2026, time.August, 17, 11, 0)
	if !got[0].Equal(want) {
		t.Errorf("first session = %s, want %s (past ones dropped)", got[0], want)
	}
}

func TestGenerateWeeklySessions_MultipleWindowsSameDayAreOrdered(t *testing.T) {
	from := istAt(2026, time.August, 12, 9, 0)
	// Entered out of order, as a host might.
	windows := []models.SessionWindow{
		{Weekday: weekday(time.Monday), Start: "20:00", End: "22:00"},
		{Weekday: weekday(time.Monday), Start: "10:00", End: "12:00"},
		{Weekday: weekday(time.Monday), Start: "15:00", End: "18:00"},
	}

	got := GenerateWeeklySessions(windows, 30, 0, from)

	// 4 + 6 + 4 = 14 sessions on each Monday — the case that motivated this.
	perMonday := 0
	firstDay := got[0].In(timeutil.IST).Day()
	for _, s := range got {
		if s.In(timeutil.IST).Day() == firstDay {
			perMonday++
		}
	}
	if perMonday != 14 {
		t.Errorf("expected 14 sessions on the first Monday, got %d", perMonday)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Before(got[i-1]) {
			t.Fatalf("sessions out of order at %d: %s before %s", i, got[i], got[i-1])
		}
	}
}

func TestGenerateWeeklySessions_IgnoresDatedWindows(t *testing.T) {
	from := istAt(2026, time.August, 12, 9, 0)
	// Dated windows belong in custom_dates, not here.
	windows := []models.SessionWindow{
		{Date: "2026-08-17", Start: "10:00", End: "12:00"},
	}

	if got := GenerateWeeklySessions(windows, 30, 0, from); len(got) != 0 {
		t.Errorf("expected no sessions from dated windows, got %d", len(got))
	}
	if HasWeeklyWindows(windows) {
		t.Error("dated windows should not count as weekly")
	}
}

func TestGenerateWeeklySessions_RejectsUnusableInput(t *testing.T) {
	from := istAt(2026, time.August, 12, 9, 0)
	mon := weekday(time.Monday)

	cases := map[string]struct {
		windows  []models.SessionWindow
		duration int
	}{
		"zero duration":               {[]models.SessionWindow{{Weekday: mon, Start: "10:00", End: "12:00"}}, 0},
		"end before start":            {[]models.SessionWindow{{Weekday: mon, Start: "12:00", End: "10:00"}}, 30},
		"window shorter than session": {[]models.SessionWindow{{Weekday: mon, Start: "10:00", End: "10:20"}}, 30},
		"unparseable time":            {[]models.SessionWindow{{Weekday: mon, Start: "abc", End: "12:00"}}, 30},
		"no windows":                  {nil, 30},
	}

	for name, tc := range cases {
		if got := GenerateWeeklySessions(tc.windows, tc.duration, 0, from); len(got) != 0 {
			t.Errorf("%s: expected no sessions, got %d", name, len(got))
		}
	}
}

func TestGenerateWeeklySessions_RealisticScheduleReachesFullHorizon(t *testing.T) {
	from := istAt(2026, time.August, 12, 9, 0)
	// The motivating schedule: three windows on one weekday, 14 sessions a week.
	windows := []models.SessionWindow{
		{Weekday: weekday(time.Monday), Start: "10:00", End: "12:00"},
		{Weekday: weekday(time.Monday), Start: "15:00", End: "18:00"},
		{Weekday: weekday(time.Monday), Start: "20:00", End: "22:00"},
	}

	got := GenerateWeeklySessions(windows, 30, 0, from)

	// The cap must not bite here — a normal schedule gets the whole horizon, as
	// the host-facing copy promises.
	last := got[len(got)-1]
	if last.Before(from.AddDate(0, 0, 21)) {
		t.Errorf("last session %s is less than 3 weeks out; horizon truncated", last)
	}
}

func TestGenerateWeeklySessions_RespectsCap(t *testing.T) {
	from := istAt(2026, time.August, 12, 0, 0)
	var windows []models.SessionWindow
	for d := time.Sunday; d <= time.Saturday; d++ {
		windows = append(windows, models.SessionWindow{
			Weekday: weekday(d), Start: "08:00", End: "20:00",
		})
	}

	// 48 sessions a day x 7 days x 4 weeks would be >1300 without the cap.
	got := GenerateWeeklySessions(windows, 15, 0, from)

	if len(got) > MaxGeneratedSessions+48 {
		t.Errorf("generated %d sessions, expected the cap (%d) to bound it",
			len(got), MaxGeneratedSessions)
	}
	if len(got) < MaxGeneratedSessions {
		t.Errorf("generated only %d sessions, expected at least the cap", len(got))
	}
}
