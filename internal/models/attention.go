package models

import "time"

// AttentionItem is one thing about an experience that needs the host's eye.
// The host dashboard and the admin editor render the same list, so the
// wording lives here, once.
type AttentionItem struct {
	// Kind: join_requests | code_used_up | code_near_limit | code_expiring | passes_expiring
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Count of things behind the item (requests, passes); 0 for a single code.
	Count int `json:"count,omitempty"`
	// Code is set for the code_* kinds.
	Code string `json:"code,omitempty"`
	// Due is when the thing runs out (code or earliest pass end), if it does.
	Due *time.Time `json:"due,omitempty"`
}
