package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"myslotmate-backend/internal/auth"

	"github.com/google/uuid"
)

// These routes were public until the roster was found to be returning guests'
// phone numbers, WhatsApp numbers, ages and government-ID links to anyone
// holding an event id. The tests below are the regression guard: each asserts
// that the scope check refuses, not that the handler happens to error later.

func TestHostScopeAllowsOwnHost(t *testing.T) {
	mine := uuid.New()
	c := signedIn(mine)

	r := httptest.NewRequest("GET", "/events/calendar/"+mine.String(), nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyUID, "firebase-uid"))

	if err := c.assertHostScope(r, mine); err != nil {
		t.Fatalf("a host must be able to read their own data: %v", err)
	}
}

func TestHostScopeRefusesAnotherHost(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()
	c := signedIn(mine)

	r := httptest.NewRequest("GET", "/events/calendar/"+theirs.String(), nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyUID, "firebase-uid"))

	if err := c.assertHostScope(r, theirs); err == nil {
		t.Fatal("a host read another host's schedule — this is the leak these routes had")
	}
}

func TestHostScopeRefusesAnonymous(t *testing.T) {
	c := signedIn(uuid.New())

	// No UID in context: the request never authenticated. Fails closed.
	r := httptest.NewRequest("GET", "/events/calendar/"+uuid.New().String(), nil)

	if err := c.assertHostScope(r, uuid.New()); err == nil {
		t.Fatal("an unauthenticated caller was allowed through")
	}
}

func TestHostScopeAllowsAdminAnyHost(t *testing.T) {
	c := signedIn(uuid.New())
	theirs := uuid.New()

	// An admin session, as OptionalAdmin records it. The admin dashboard reads
	// other people's hosts by design, so this must keep working.
	r := httptest.NewRequest("GET", "/events/host/"+theirs.String(), nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyAdminUser, "admin@myslotmate.com"))

	if err := c.assertHostScope(r, theirs); err != nil {
		t.Fatalf("an admin must still read any host: %v", err)
	}
}

func TestHostScopeFailsClosedWithoutIdentityLookups(t *testing.T) {
	// A controller wired without WithAuth cannot identify anyone. It must
	// refuse rather than fall back to trusting the URL.
	c := &EventController{}

	r := httptest.NewRequest("GET", "/events/today/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ContextKeyUID, "firebase-uid"))

	if err := c.assertHostScope(r, uuid.New()); err == nil {
		t.Fatal("a controller with no identity lookups allowed a host-scoped read")
	}
}
