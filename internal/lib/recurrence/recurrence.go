package recurrence

import (
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// NextOccurrence returns the first occurrence of `rule` (RRULE string, with or
// without the "RRULE:" prefix) at or after `after`, anchored at `dtstart`.
// If the rule is exhausted (e.g. COUNT/UNTIL has passed), it returns dtstart
// unchanged and ok=false so callers can keep the original time.
func NextOccurrence(rule string, dtstart, after time.Time) (time.Time, bool) {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return dtstart, false
	}
	switch strings.ToLower(rule) {
	case "daily":
		rule = "FREQ=DAILY"
	case "weekly":
		rule = "FREQ=WEEKLY"
	case "monthly":
		rule = "FREQ=MONTHLY"
	case "yearly", "annually":
		rule = "FREQ=YEARLY"
	}
	if !strings.HasPrefix(strings.ToUpper(rule), "RRULE:") {
		rule = "RRULE:" + rule
	}

	r, err := rrule.StrToRRule(rule)
	if err != nil {
		return dtstart, false
	}
	r.DTStart(dtstart)

	next := r.After(after, true)
	if next.IsZero() {
		return dtstart, false
	}
	return next, true
}
