package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/slug"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// reservedRootSlugs are top-level paths the website already serves. Landing
// pages live at the site root (/{slug}), and Next.js serves its static routes
// and public/ files before the dynamic [slug] route, so a page using one of
// these names would save fine but never be reachable.
//
// KEEP IN SYNC: add a name here whenever MySlotmate-Frontend gains a new
// top-level route under src/app/ or a new file/folder under public/.
var reservedRootSlugs = map[string]bool{
	"about": true, "activities": true, "admin": true, "api": true, "become-host": true,
	"blogs": true, "delete-account": true, "demo": true, "docs": true, "experience": true,
	"experiences": true, "explore": true, "host": true, "host-dashboard": true,
	"host-with-us": true, "hosts": true, "notifications": true, "privacy": true,
	"profile": true, "saved-experiences": true, "signup": true, "support": true,
	"wallet": true, "assets": true, "icon": true, "apple-icon": true,
	"login": true, "logout": true, "search": true, "terms": true,
}

// MarketingPageController serves admin-edited landing pages (/{slug} on the
// website). Admin routes create/edit/publish; the single public route returns
// published pages only.
type MarketingPageController struct {
	pageRepo     repository.MarketingPageRepository
	eventRepo    repository.EventRepository
	firebaseAuth *fbauth.Client
	adminEmail   string
	jwtSecret    string
}

func NewMarketingPageController(pr repository.MarketingPageRepository, er repository.EventRepository, fa *fbauth.Client, adminEmail, jwtSecret string) *MarketingPageController {
	return &MarketingPageController{pageRepo: pr, eventRepo: er, firebaseAuth: fa, adminEmail: adminEmail, jwtSecret: jwtSecret}
}

func (c *MarketingPageController) RegisterRoutes(r chi.Router) {
	r.Route("/marketing-pages", func(r chi.Router) {
		// Full admins and the restricted marketing login (auth.RoleMarketing).
		adminMW := auth.OrStaff(c.jwtSecret, auth.RequireAdmin(c.firebaseAuth, c.adminEmail, c.jwtSecret))
		r.With(adminMW).Get("/admin", c.List)
		r.With(adminMW).Post("/", c.Create)
		r.With(adminMW).Put("/{pageID}", c.Update)
		r.With(adminMW).Delete("/{pageID}", c.Delete)
		r.With(adminMW).Post("/{pageID}/publish", c.publishHandler(true))
		r.With(adminMW).Post("/{pageID}/unpublish", c.publishHandler(false))

		r.Get("/{slug}", c.GetPublic)
	})
}

type createMarketingPageRequest struct {
	EventID string  `json:"event_id"`
	Slug    *string `json:"slug,omitempty"` // optional; defaults to the event's slug
}

// updateMarketingPageRequest is a full replace: the editor always sends every
// field, so an empty value clears it.
type updateMarketingPageRequest struct {
	Slug           string   `json:"slug"`
	Title          string   `json:"title"`
	HookLine       string   `json:"hook_line"`
	CoverImageURL  string   `json:"cover_image_url"`
	GalleryURLs    []string `json:"gallery_urls"`
	Content        string   `json:"content"`
	CTAText        string   `json:"cta_text"`
	SEODescription string   `json:"seo_description"`
}

func optString(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// slugTaken reports whether candidate is reserved or used by another page.
func (c *MarketingPageController) slugTaken(r *http.Request, candidate string, excludeID uuid.UUID) (bool, error) {
	if reservedRootSlugs[candidate] {
		return true, nil
	}
	return c.pageRepo.SlugExistsExcluding(r.Context(), candidate, excludeID)
}

// Create makes a page for an event, prefilling its content from the event.
// From then on the page's content is independent of the event.
func (c *MarketingPageController) Create(w http.ResponseWriter, r *http.Request) {
	var req createMarketingPageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	eventID, err := uuid.Parse(req.EventID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "event_id is required")
		return
	}
	evt, err := c.eventRepo.GetByID(r.Context(), eventID)
	if err != nil || evt == nil {
		RespondError(w, http.StatusBadRequest, "Event not found")
		return
	}

	desired := evt.Slug
	if req.Slug != nil && strings.TrimSpace(*req.Slug) != "" {
		desired = *req.Slug
	}
	pageSlug, err := slug.Disambiguate(slug.Make(desired, "event"), func(candidate string) (bool, error) {
		return c.slugTaken(r, candidate, uuid.Nil)
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	page := &models.MarketingPage{
		Slug:          pageSlug,
		EventID:       evt.ID,
		Title:         evt.Title,
		HookLine:      evt.HookLine,
		CoverImageURL: evt.CoverImageURL,
		GalleryURLs:   evt.GalleryURLs,
	}
	if evt.Description != nil {
		page.Content = *evt.Description
	}
	if err := c.pageRepo.Create(r.Context(), page); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	page.EventSlug, page.EventTitle = evt.Slug, evt.Title
	RespondSuccess(w, http.StatusCreated, page)
}

func (c *MarketingPageController) List(w http.ResponseWriter, r *http.Request) {
	pages, err := c.pageRepo.ListAll(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, pages)
}

func (c *MarketingPageController) loadPage(w http.ResponseWriter, r *http.Request) *models.MarketingPage {
	id, err := uuid.Parse(chi.URLParam(r, "pageID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid page ID")
		return nil
	}
	page, err := c.pageRepo.GetByID(r.Context(), id)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if page == nil {
		RespondError(w, http.StatusNotFound, "Page not found")
		return nil
	}
	return page
}

func (c *MarketingPageController) Update(w http.ResponseWriter, r *http.Request) {
	page := c.loadPage(w, r)
	if page == nil {
		return
	}
	var req updateMarketingPageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		RespondError(w, http.StatusBadRequest, "Title is required")
		return
	}

	// An explicit slug change is rejected (not silently suffixed) when taken,
	// so the admin knows exactly which URL they are publishing.
	newSlug := slug.Make(req.Slug, page.Slug)
	if newSlug != page.Slug {
		taken, err := c.slugTaken(r, newSlug, page.ID)
		if err != nil {
			RespondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if taken {
			RespondError(w, http.StatusConflict, "The URL /"+newSlug+" is already used or reserved. Pick another slug.")
			return
		}
	}

	page.Slug = newSlug
	page.Title = strings.TrimSpace(req.Title)
	page.HookLine = optString(req.HookLine)
	page.CoverImageURL = optString(req.CoverImageURL)
	page.GalleryURLs = req.GalleryURLs
	page.Content = req.Content
	page.CTAText = optString(req.CTAText)
	page.SEODescription = optString(req.SEODescription)

	if err := c.pageRepo.Update(r.Context(), page); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, page)
}

func (c *MarketingPageController) Delete(w http.ResponseWriter, r *http.Request) {
	page := c.loadPage(w, r)
	if page == nil {
		return
	}
	if err := c.pageRepo.Delete(r.Context(), page.ID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]string{"message": "Page deleted"})
}

func (c *MarketingPageController) publishHandler(published bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := c.loadPage(w, r)
		if page == nil {
			return
		}
		if err := c.pageRepo.SetPublished(r.Context(), page.ID, published); err != nil {
			RespondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		page, _ = c.pageRepo.GetByID(r.Context(), page.ID)
		RespondSuccess(w, http.StatusOK, page)
	}
}

// GetPublic returns a published page by slug. Drafts are 404 publicly; the
// admin panel previews drafts itself.
func (c *MarketingPageController) GetPublic(w http.ResponseWriter, r *http.Request) {
	page, err := c.pageRepo.GetBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if page == nil || page.PublishedAt == nil {
		RespondError(w, http.StatusNotFound, "Page not found")
		return
	}
	RespondSuccess(w, http.StatusOK, page)
}
