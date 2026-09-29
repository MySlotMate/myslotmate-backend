package controller

import (
	"net/http"
	"strings"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/go-chi/chi/v5"
)

// ExperienceTemplateController serves mood-keyed title/hook_line suggestions
// used by the event creation form. Read-only and public to authenticated hosts.
type ExperienceTemplateController struct {
	repo repository.ExperienceTemplateRepository
}

func NewExperienceTemplateController(repo repository.ExperienceTemplateRepository) *ExperienceTemplateController {
	return &ExperienceTemplateController{repo: repo}
}

func (c *ExperienceTemplateController) RegisterRoutes(r chi.Router) {
	r.Route("/experience-templates", func(r chi.Router) {
		r.Get("/", c.List)
	})
}

// moodLookups returns the spellings a mood may be stored under, canonical form
// first: the seed data predates the rename, so both have to be tried.
func moodLookups(mood string) []string {
	raw := strings.ToLower(strings.TrimSpace(mood))
	asMood := models.EventMood(raw)
	canonical, err := models.NormalizeEventMood(&asMood)
	if err != nil || canonical == nil {
		return []string{raw}
	}
	out := []string{string(*canonical)}
	if legacy, ok := legacyMoodNames[string(*canonical)]; ok {
		out = append(out, legacy)
	}
	if raw != out[0] {
		out = append(out, raw)
	}
	return out
}

// The old names the templates were seeded under, keyed by canonical mood.
var legacyMoodNames = map[string]string{
	string(models.EventMoodAdventurous): string(models.EventMoodAdventureLegacy),
	string(models.EventMoodRelaxing):    string(models.EventMoodChillLegacy),
	string(models.EventMoodCreative):    string(models.EventMoodRomanticLegacy),
	string(models.EventMoodEducational): string(models.EventMoodIntellectualLegacy),
	string(models.EventMoodCulinary):    string(models.EventMoodFoodieLegacy),
	string(models.EventMoodCultural):    string(models.EventMoodNightlifeLegacy),
}

// List returns templates filtered by ?mood=<mood>, or all if mood is omitted.
func (c *ExperienceTemplateController) List(w http.ResponseWriter, r *http.Request) {
	mood := strings.TrimSpace(r.URL.Query().Get("mood"))

	if mood != "" {
		// Match the mood the same way an event does, then fall back to the legacy
		// spelling. Some rows were seeded under the old names ("adventure"), so a
		// plain lowercase lookup silently returns nothing for the canonical mood
		// the create form actually sends ("adventurous").
		lookups := moodLookups(mood)
		var templates []*models.ExperienceTemplate
		for _, m := range lookups {
			found, err := c.repo.ListByMood(r.Context(), m)
			if err != nil {
				RespondError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if len(found) > 0 {
				templates = found
				break
			}
		}
		if templates == nil {
			templates = []*models.ExperienceTemplate{}
		}
		RespondSuccess(w, http.StatusOK, templates)
		return
	}

	templates, err := c.repo.ListAll(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, templates)
}
