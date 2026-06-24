// Package timeutil centralizes timezone handling for user-facing time display.
//
// All event and booking times are stored as UTC instants (Postgres TIMESTAMPTZ)
// and the frontend sends them as UTC ISO strings. They must be converted to IST
// before being shown to users; formatting a stored time directly renders UTC,
// which is 5h30m behind the wall-clock time users expect.
package timeutil

import "time"

// IST is India Standard Time (UTC+05:30).
var IST = time.FixedZone("IST", 5*60*60+30*60)

// EventTimeLayout is the standard user-facing event time format, e.g.
// "Jun 24, 2026 11:33 PM".
const EventTimeLayout = "Jan 2, 2006 3:04 PM"

// FormatIST renders a UTC instant as an IST wall-clock string using layout.
func FormatIST(t time.Time, layout string) string {
	return t.In(IST).Format(layout)
}

// FormatEventTime renders a UTC instant in the standard user-facing event format
// (IST). Use this for every event/booking time shown to a customer.
func FormatEventTime(t time.Time) string {
	return FormatIST(t, EventTimeLayout)
}
