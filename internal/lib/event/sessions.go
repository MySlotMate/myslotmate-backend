package event

import (
	"sort"
	"time"

	"myslotmate-backend/internal/lib/timeutil"
	"myslotmate-backend/internal/models"
)

// One-on-one session generation.
//
// A one-off one-on-one event stores every session start in events.custom_dates,
// because the host named specific dates and that list is finite. A *recurring*
// one-on-one event stores no dates at all: the host declares weekly office
// hours ("Mondays 10:00–12:00") that repeat indefinitely, so the sessions are
// generated on read instead. Nothing to top up, nothing to expire.
//
// Windows are IST wall-clock, matching what the host typed (see
// lib/timeutil/ist.go for why every event time is IST).

// SessionHorizonWeeks is how far ahead recurring one-on-one sessions are
// offered. It rolls forward on its own because generation is anchored to "now".
// Deep enough to book a month out; shallow enough that the availability payload
// stays small — three windows a week at 30 minutes is already ~100 slots.
const SessionHorizonWeeks = 4

// MaxGeneratedSessions bounds one event's generated slots. A pathological
// schedule (all seven days, 12 hours each, 15-minute sessions) would otherwise
// produce over a thousand — and the public feed generates these for every
// listed event just to find its next free slot. Hitting the cap truncates the
// far end of the horizon, which is the least useful part.
const MaxGeneratedSessions = 300

// GenerateWeeklySessions expands weekly availability windows into session start
// instants, from `from` through SessionHorizonWeeks weeks later.
//
// Sessions are laid out the same way as the one-off generator on the client:
// `duration` long, separated by `breakMinutes`, packed from the window start,
// and any leftover time at the end of a window is left unused rather than
// producing a session that overruns it.
//
// Windows without a Weekday are ignored — those are dated windows, which live
// in custom_dates instead.
func GenerateWeeklySessions(
	windows []models.SessionWindow,
	durationMinutes, breakMinutes int,
	from time.Time,
) []time.Time {
	return generate(windows, durationMinutes, breakMinutes, from, MaxGeneratedSessions)
}

// generate is the shared expansion, bounded by `max` so callers that only need
// the next session don't walk the whole horizon.
func generate(
	windows []models.SessionWindow,
	durationMinutes, breakMinutes int,
	from time.Time,
	max int,
) []time.Time {
	if durationMinutes <= 0 || len(windows) == 0 {
		return nil
	}
	if breakMinutes < 0 {
		breakMinutes = 0
	}

	// Group by weekday once so each day of the horizon is a map lookup.
	byWeekday := make(map[time.Weekday][]models.SessionWindow, 7)
	for _, w := range windows {
		if w.Weekday == nil || *w.Weekday < 0 || *w.Weekday > 6 {
			continue
		}
		day := time.Weekday(*w.Weekday)
		byWeekday[day] = append(byWeekday[day], w)
	}
	if len(byWeekday) == 0 {
		return nil
	}

	// Walk IST calendar days, not 24-hour steps, so the wall-clock times the
	// host typed stay put.
	istFrom := from.In(timeutil.IST)
	cursor := time.Date(istFrom.Year(), istFrom.Month(), istFrom.Day(), 0, 0, 0, 0, timeutil.IST)
	end := cursor.AddDate(0, 0, SessionHorizonWeeks*7)

	var starts []time.Time
	for day := cursor; day.Before(end); day = day.AddDate(0, 0, 1) {
		for _, w := range byWeekday[day.Weekday()] {
			startMin, ok := parseClock(w.Start)
			if !ok {
				continue
			}
			endMin, ok := parseClock(w.End)
			if !ok || endMin <= startMin {
				continue
			}
			for offset := startMin; offset+durationMinutes <= endMin; offset += durationMinutes + breakMinutes {
				slot := day.Add(time.Duration(offset) * time.Minute)
				if slot.Before(from) {
					continue // already started
				}
				starts = append(starts, slot)
			}
		}
		if len(starts) >= max {
			break
		}
	}

	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	return starts
}

// NextWeeklySession returns the soonest session at or after `from`, or zero if
// the windows produce none within the horizon.
//
// This is the anchor for a recurring one-on-one event's `time` column. That
// column can't be rolled forward by the RRULE the way a group event's is: the
// rule only says "weekly", and stepping it in 7-day hops would pin the event to
// whichever weekday it was first created on — a session that may no longer
// exist once the host edits their windows. Deriving it from the windows keeps
// the anchor a real, bookable session at all times.
func NextWeeklySession(
	windows []models.SessionWindow,
	durationMinutes, breakMinutes int,
	from time.Time,
) (time.Time, bool) {
	// Stops at the first day that yields anything, so this stays cheap even
	// though it runs on every event scan.
	starts := generate(windows, durationMinutes, breakMinutes, from, 1)
	if len(starts) == 0 {
		return time.Time{}, false
	}
	return starts[0], true
}

// HasWeeklyWindows reports whether the event's windows describe weekly office
// hours (as opposed to dated one-off windows).
func HasWeeklyWindows(windows []models.SessionWindow) bool {
	for _, w := range windows {
		if w.Weekday != nil {
			return true
		}
	}
	return false
}

// parseClock reads "15:04" into minutes past midnight.
func parseClock(clock string) (int, bool) {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}
