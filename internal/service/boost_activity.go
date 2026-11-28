package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Host notifications for Boost and Premium: what the team or the system did
// (a proposal sent or revised, declined, accepted for the host, live, ended,
// payments, creatives reviewed, reports, Premium decisions). The website's
// notifications feed reads these; the wording lives here, once.

// ActivityItem is one notification for the host.
type ActivityItem struct {
	ID      string     `json:"id"`
	Kind    string     `json:"kind"` // boost | premium
	Action  string     `json:"action"`
	Title   string     `json:"title"`
	Body    string     `json:"body"`
	At      time.Time  `json:"at"`
	Href    string     `json:"href"`
	BoostID *uuid.UUID `json:"boost_id,omitempty"`
	EventID *uuid.UUID `json:"event_id,omitempty"`
}

const activityDays = 60

// HostActivity is the signed-in host's Boost and Premium notifications from
// the last 60 days, newest first.
func (s *BoostService) HostActivity(ctx context.Context, userID uuid.UUID) ([]ActivityItem, error) {
	h, err := s.hostOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.HostActivity(ctx, h.ID, time.Now().AddDate(0, 0, -activityDays), 100)
	if err != nil {
		return nil, err
	}
	return activityItems(rows), nil
}

// activityItems words the log rows; rows with nothing to tell the host drop out.
func activityItems(rows []repository.ActivityRow) []ActivityItem {
	// Rows come newest first: a proposal is "revised" when an older one exists.
	proposals := map[uuid.UUID]int{}
	for _, r := range rows {
		if r.Action == "proposed" && r.BoostID != nil {
			proposals[*r.BoostID]++
		}
	}
	out := []ActivityItem{}
	for _, r := range rows {
		d := map[string]any{}
		_ = json.Unmarshal(r.Details, &d)
		str := func(k string) string { v, _ := d[k].(string); return strings.TrimSpace(v) }
		cents := func(k string) int64 { v, _ := d[k].(float64); return int64(v) }
		t := r.EventTitle
		var title, body string
		switch r.Source + ":" + r.Action {
		case "boost:proposed":
			proposals[*r.BoostID]--
			if proposals[*r.BoostID] > 0 {
				title, body = "Boost proposal revised", fmt.Sprintf("The team changed the proposal for %s. Review it and accept it again to continue.", t)
			} else {
				title, body = "Boost proposal ready", fmt.Sprintf("The team sent a proposal for %s. Review the fee and plan, then accept it.", t)
			}
		case "boost:declined":
			title, body = "Boost request declined", fmt.Sprintf("We can't boost %s this time.", t)
			if v := str("reason"); v != "" {
				body += " " + v
			}
		case "boost:accepted_for_host":
			title, body = "Proposal accepted for you", fmt.Sprintf("The team accepted the Boost proposal for %s, as agreed with you.", t)
		case "boost:moved_to_budget_model":
			title, body = "Boost pricing changed", fmt.Sprintf("%s's Boost now has an ad budget you pay upfront, and a lower fee per ticket.", t)
		case "boost:payment_link_sent":
			title, body = "Payment link sent", fmt.Sprintf("Pay %s for the Boost on %s.", rupeesText(cents("total_cents")), t)
		case "boost:payment_paid":
			if d["orphan"] == true || d["duplicate"] == true {
				continue
			}
			title, body = "Boost payment received", fmt.Sprintf("We received %s for the Boost on %s.", rupeesText(cents("total_cents")), t)
		case "boost:payment_reversed":
			title, body = "Boost payment reversed", fmt.Sprintf("The recorded payment for %s was reversed.", t)
			if v := str("reason"); v != "" {
				body += " " + v
			}
		case "boost:payment_expired":
			title, body = "Boost plan expired", fmt.Sprintf("The budget for %s wasn't paid in time. Nothing was charged.", t)
		case "boost:went_live":
			title, body = "Your Boost is live", fmt.Sprintf("Ads for %s are running.", t)
		case "boost:ended":
			switch str("reason") {
			case "host_stopped":
				continue
			case "budget_spent":
				title, body = "Boost budget used up", fmt.Sprintf("The ad budget for %s is spent, so the Boost has ended.", t)
			case "admin_ended":
				title, body = "Boost ended by the team", fmt.Sprintf("The team ended the Boost on %s.", t)
			default:
				title, body = "Your Boost has ended", fmt.Sprintf("The Boost on %s has finished its run.", t)
			}
			if v := str("note"); v != "" {
				body += " " + v
			}
		case "boost:creative_added":
			if r.ActorKind != "admin" {
				continue
			}
			title, body = "A creative to approve", fmt.Sprintf("The team made a creative for %s. Approve it or ask for changes.", t)
		case "boost:creative_in_use":
			title, body = "Creative in the ads", fmt.Sprintf("One of your creatives for %s is now running.", t)
		case "boost:creative_needs_changes":
			title, body = "A creative needs changes", fmt.Sprintf("The team asked for changes to a creative for %s.", t)
			if v := str("note"); v != "" {
				body += " " + v
			}
		case "boost:creative_not_used":
			title, body = "A creative won't be used", fmt.Sprintf("The team won't run one of your creatives for %s.", t)
			if v := str("note"); v != "" {
				body += " " + v
			}
		case "boost:report_added":
			title, body = "New Boost report", fmt.Sprintf("%s: %s.", t, str("title"))
		case "boost:closed":
			title, body = "Final Boost report ready", fmt.Sprintf("The Boost on %s is closed. Its final report is ready.", t)
		case "premium:approve", "premium:grant":
			title, body = "You're a Premium host", "Boost is unlocked for your events."
		case "premium:decline":
			title, body = "Premium application declined", "We can't add you to Premium Hosts right now."
			if v := str("reason"); v != "" {
				body += " " + v
			}
		case "premium:remove":
			title, body = "Premium membership ended", "Boost is locked again; a live Boost runs to its end."
			if v := str("reason"); v != "" {
				body += " " + v
			}
		default:
			continue
		}
		it := ActivityItem{ID: fmt.Sprintf("%s-%d", r.Source, r.ID), Kind: r.Source, Action: r.Action, Title: title,
			Body: body, At: r.CreatedAt, BoostID: r.BoostID, EventID: r.EventID, Href: "/host-dashboard/premium"}
		if r.EventID != nil {
			it.Href = fmt.Sprintf("/host-dashboard/experiences/%s?boost=1", r.EventID)
		}
		out = append(out, it)
	}
	return out
}
