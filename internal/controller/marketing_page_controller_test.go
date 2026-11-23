package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"myslotmate-backend/internal/lib/slug"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

type fakePageRepo struct {
	repository.MarketingPageRepository // unimplemented methods panic; only SlugExistsExcluding is used
	taken                              map[string]bool
}

func (f fakePageRepo) SlugExistsExcluding(_ context.Context, s string, _ uuid.UUID) (bool, error) {
	return f.taken[s], nil
}

// A landing page must never get a slug the website already routes (e.g. /about),
// and a default slug that collides must be suffixed rather than rejected.
func TestMarketingPageSlugTaken(t *testing.T) {
	c := &MarketingPageController{pageRepo: fakePageRepo{taken: map[string]bool{"yoga": true}}}
	r := httptest.NewRequest("GET", "/", nil)

	for _, s := range []string{"about", "blogs", "experience", "yoga"} {
		if taken, _ := c.slugTaken(r, s, uuid.Nil); !taken {
			t.Errorf("slug %q should be taken", s)
		}
	}
	if taken, _ := c.slugTaken(r, "sunset-yoga", uuid.Nil); taken {
		t.Error("free slug reported as taken")
	}

	got, _ := slug.Disambiguate("about", func(s string) (bool, error) { return c.slugTaken(r, s, uuid.Nil) })
	if got != "about-2" {
		t.Errorf("reserved default slug: got %q, want about-2", got)
	}
}
