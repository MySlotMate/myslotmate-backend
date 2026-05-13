package controller

import (
	"net/http"
	"strings"

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

// List returns templates filtered by ?mood=<mood>, or all if mood is omitted.
func (c *ExperienceTemplateController) List(w http.ResponseWriter, r *http.Request) {
	mood := strings.TrimSpace(r.URL.Query().Get("mood"))

	if mood != "" {
		templates, err := c.repo.ListByMood(r.Context(), strings.ToLower(mood))
		if err != nil {
			RespondError(w, http.StatusInternalServerError, err.Error())
			return
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
