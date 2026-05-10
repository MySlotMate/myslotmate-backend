package main

import (
	"fmt"
	"time"

	"github.com/teambition/rrule-go"
)

func main() {
	// Simulate the DB entry
	// ID: ae77f5b3-4fdb-48a3-9006-37799d74091d | Title: journey to NY | Recurring: true | Rule: weekly | Time: 2026-05-11 15:22:00 +0530 IST
	loc, _ := time.LoadLocation("Asia/Kolkata")
	evtTime := time.Date(2026, 5, 11, 15, 22, 0, 0, loc)

	ruleStr := "FREQ=WEEKLY"
	r, err := rrule.StrToRRule(ruleStr)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	r.DTStart(evtTime)

	now := time.Now()
	searchStart := now.Add(-1 * time.Hour)
	end := now.AddDate(0, 2, 0)

	fmt.Printf("Now: %v\n", now)
	fmt.Printf("SearchStart: %v\n", searchStart)
	fmt.Printf("End: %v\n", end)
	fmt.Printf("DTStart: %v\n", evtTime)

	occurrences := r.Between(searchStart, end, true)
	fmt.Printf("Found %d occurrences:\n", len(occurrences))
	for i, occ := range occurrences {
		fmt.Printf("%d: %v\n", i+1, occ)
	}
}
