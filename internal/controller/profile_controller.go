package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/moderation"
	"myslotmate-backend/internal/lib/storage"
	"myslotmate-backend/internal/lib/validation"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// albumUploadFolder is where the app uploads album photos. Only files under it
// are ever deleted from S3 when a photo or album goes.
const albumUploadFolder = "users/albums"

const maxPhotosPerRequest = 10

// ProfileController serves the public-profile extras: a host's public event
// list and profile albums. Its routes live under /hosts and /users, which the
// Host and User controllers own, so they mount them via RegisterHostRoutes /
// RegisterUserRoutes (chi panics on a second Route for the same prefix).
type ProfileController struct {
	hostService  service.HostService
	eventRepo    repository.EventRepository
	albumRepo    repository.AlbumRepository
	userRepo     repository.UserRepository
	uploads      *storage.UploadService // nil when S3 is not configured
	firebaseAuth *fbauth.Client
	jwtSecret    string
}

func NewProfileController(hs service.HostService, er repository.EventRepository, ar repository.AlbumRepository,
	ur repository.UserRepository, uploads *storage.UploadService, fa *fbauth.Client, jwtSecret string) *ProfileController {
	return &ProfileController{hostService: hs, eventRepo: er, albumRepo: ar, userRepo: ur, uploads: uploads, firebaseAuth: fa, jwtSecret: jwtSecret}
}

// RegisterHostRoutes is called inside the /hosts route.
func (c *ProfileController) RegisterHostRoutes(r chi.Router) {
	r.Get("/{hostID}/events", c.ListPublicHostEvents)
	r.With(auth.OptionalUser(c.firebaseAuth, c.jwtSecret)).Get("/{hostID}/albums", c.ListHostAlbums)
}

// RegisterUserRoutes is called inside the /users route.
func (c *ProfileController) RegisterUserRoutes(r chi.Router) {
	r.With(auth.OptionalUser(c.firebaseAuth, c.jwtSecret)).Get("/{userID}/albums", c.ListUserAlbums)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))
		r.Post("/me/albums", c.CreateAlbum)
		r.Patch("/me/albums/{albumID}", c.UpdateAlbum)
		r.Delete("/me/albums/{albumID}", c.DeleteAlbum)
		r.Post("/me/albums/{albumID}/photos", c.AddAlbumPhotos)
		r.Delete("/me/albums/{albumID}/photos/{photoID}", c.DeleteAlbumPhoto)
	})
}

// ── Public host events ──────────────────────────────────────────────────────

// ListPublicHostEvents: GET /hosts/{host_id_or_slug}/events?limit=&before=
// Public. Live/paused, non-private events the host owns, past included,
// newest first. Never carries access_passkey.
func (c *ProfileController) ListPublicHostEvents(w http.ResponseWriter, r *http.Request) {
	host, ok := c.lookupHost(w, r)
	if !ok {
		return
	}

	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 100)
	}
	var before *time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			if t, err = time.Parse("2006-01-02", b); err != nil {
				RespondError(w, http.StatusBadRequest, "before must be an ISO date")
				return
			}
		}
		before = &t
	}

	events, err := c.eventRepo.ListPublicByHost(r.Context(), host.ID, before, limit)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Could not load events")
		return
	}
	if events == nil {
		events = []*models.Event{}
	}
	stripEventPasskeys(events)
	RespondSuccess(w, http.StatusOK, events)
}

func (c *ProfileController) lookupHost(w http.ResponseWriter, r *http.Request) (*models.Host, bool) {
	host, err := c.hostService.GetHostBySlugOrID(r.Context(), chi.URLParam(r, "hostID"))
	if err != nil || host == nil {
		if host == nil && (err == nil || err.Error() == "host not found") {
			RespondError(w, http.StatusNotFound, "Host not found")
		} else {
			RespondError(w, http.StatusInternalServerError, "Could not load host")
		}
		return nil, false
	}
	return host, true
}

// ── Albums: public reads ────────────────────────────────────────────────────

func (c *ProfileController) ListUserAlbums(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid user id")
		return
	}
	c.respondAlbums(w, r, userID)
}

func (c *ProfileController) ListHostAlbums(w http.ResponseWriter, r *http.Request) {
	host, ok := c.lookupHost(w, r)
	if !ok {
		return
	}
	c.respondAlbums(w, r, host.UserID)
}

// respondAlbums lists a user's albums. Visitors see only albums with photos;
// the owner (signed in) also sees empty ones so they can fill them.
func (c *ProfileController) respondAlbums(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	callerID, _ := c.callerUserID(r)
	albums, err := c.albumRepo.ListByUser(r.Context(), userID, callerID == userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "Could not load albums")
		return
	}
	RespondSuccess(w, http.StatusOK, albums)
}

// ── Albums: owner writes ────────────────────────────────────────────────────

type albumBody struct {
	Title        *string    `json:"title"`
	Description  *string    `json:"description"`
	Position     *int       `json:"position"`
	CoverPhotoID *uuid.UUID `json:"cover_photo_id"`
}

// validate trims and checks text fields in place. Returns a user-facing error.
func (b *albumBody) validate(creating bool) error {
	if b.Title != nil {
		t := strings.TrimSpace(*b.Title)
		b.Title = &t
		if n := utf8.RuneCountInString(t); n < 1 || n > 40 {
			return errors.New("Album title must be 1–40 characters")
		}
		if err := moderation.Check("Album title", t); err != nil {
			return err
		}
	} else if creating {
		return errors.New("Album title is required")
	}
	if b.Description != nil {
		d := strings.TrimSpace(*b.Description)
		b.Description = &d
		if utf8.RuneCountInString(d) > 280 {
			return errors.New("Album description can be at most 280 characters")
		}
		if err := moderation.Check("Album description", d); err != nil {
			return err
		}
	}
	if b.Position != nil && *b.Position < 0 {
		return errors.New("position can't be negative")
	}
	return nil
}

func (c *ProfileController) CreateAlbum(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	var body albumBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if err := body.validate(true); err != nil {
		RespondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	id, err := c.albumRepo.Create(r.Context(), userID, *body.Title, body.Description)
	if err != nil {
		c.respondAlbumErr(w, err)
		return
	}
	c.respondOwnedAlbum(w, r, id, userID, http.StatusCreated)
}

func (c *ProfileController) UpdateAlbum(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	albumID, ok := parseUUIDParam(w, r, "albumID")
	if !ok {
		return
	}
	var body albumBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if err := body.validate(false); err != nil {
		RespondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	err := c.albumRepo.Update(r.Context(), albumID, userID, repository.AlbumUpdate{
		Title: body.Title, Description: body.Description, Position: body.Position, CoverPhotoID: body.CoverPhotoID,
	})
	if err != nil {
		c.respondAlbumErr(w, err)
		return
	}
	c.respondOwnedAlbum(w, r, albumID, userID, http.StatusOK)
}

func (c *ProfileController) DeleteAlbum(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	albumID, ok := parseUUIDParam(w, r, "albumID")
	if !ok {
		return
	}
	urls, err := c.albumRepo.Delete(r.Context(), albumID, userID)
	if err != nil {
		c.respondAlbumErr(w, err)
		return
	}
	c.cleanupFiles(urls)
	RespondSuccess(w, http.StatusOK, map[string]any{"id": albumID})
}

type addPhotosBody struct {
	Items []struct {
		URL     string  `json:"url"`
		Caption *string `json:"caption"`
		Width   *int    `json:"width"`
		Height  *int    `json:"height"`
	} `json:"items"`
}

func (c *ProfileController) AddAlbumPhotos(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	albumID, ok := parseUUIDParam(w, r, "albumID")
	if !ok {
		return
	}
	var body addPhotosBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if len(body.Items) == 0 {
		RespondError(w, http.StatusUnprocessableEntity, "Add at least one photo")
		return
	}
	if len(body.Items) > maxPhotosPerRequest {
		RespondError(w, http.StatusUnprocessableEntity, "Add up to 10 photos at a time")
		return
	}

	items := make([]repository.NewAlbumPhoto, 0, len(body.Items))
	for _, it := range body.Items {
		u := strings.TrimSpace(it.URL)
		if u == "" {
			RespondError(w, http.StatusUnprocessableEntity, "Every photo needs a url")
			return
		}
		if err := validation.ValidateImageURL(u); err != nil {
			RespondError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if it.Caption != nil {
			cap := strings.TrimSpace(*it.Caption)
			if utf8.RuneCountInString(cap) > 200 {
				RespondError(w, http.StatusUnprocessableEntity, "Captions can be at most 200 characters")
				return
			}
			if err := moderation.Check("Caption", cap); err != nil {
				RespondError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			if cap == "" {
				it.Caption = nil
			} else {
				it.Caption = &cap
			}
		}
		if (it.Width != nil && *it.Width <= 0) || (it.Height != nil && *it.Height <= 0) {
			RespondError(w, http.StatusUnprocessableEntity, "width and height must be positive")
			return
		}
		items = append(items, repository.NewAlbumPhoto{URL: u, Caption: it.Caption, Width: it.Width, Height: it.Height})
	}

	if err := c.albumRepo.AddPhotos(r.Context(), albumID, userID, items); err != nil {
		c.respondAlbumErr(w, err)
		return
	}
	c.respondOwnedAlbum(w, r, albumID, userID, http.StatusCreated)
}

func (c *ProfileController) DeleteAlbumPhoto(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	albumID, ok := parseUUIDParam(w, r, "albumID")
	if !ok {
		return
	}
	photoID, ok := parseUUIDParam(w, r, "photoID")
	if !ok {
		return
	}
	url, err := c.albumRepo.DeletePhoto(r.Context(), albumID, photoID, userID)
	if err != nil {
		c.respondAlbumErr(w, err)
		return
	}
	c.cleanupFiles([]string{url})
	RespondSuccess(w, http.StatusOK, map[string]any{"id": photoID})
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (c *ProfileController) respondOwnedAlbum(w http.ResponseWriter, r *http.Request, albumID, userID uuid.UUID, status int) {
	album, err := c.albumRepo.GetOwned(r.Context(), albumID, userID)
	if err != nil {
		c.respondAlbumErr(w, err)
		return
	}
	RespondSuccess(w, status, album)
}

func (c *ProfileController) respondAlbumErr(w http.ResponseWriter, err error) {
	var rule *repository.AlbumRuleError
	switch {
	case errors.Is(err, repository.ErrAlbumNotFound):
		RespondError(w, http.StatusNotFound, "Album not found")
	case errors.As(err, &rule):
		RespondError(w, http.StatusUnprocessableEntity, rule.Error())
	default:
		log.Printf("[ALBUMS] %v", err)
		RespondError(w, http.StatusInternalServerError, "Something went wrong")
	}
}

// cleanupFiles deletes removed photos from S3 in the background. Only files in
// the albums upload folder that no other album photo still uses are touched;
// URLs pasted from elsewhere (event covers, the "general" folder) are left.
func (c *ProfileController) cleanupFiles(urls []string) {
	if c.uploads == nil || len(urls) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, u := range urls {
			if used, err := c.albumRepo.URLInUse(ctx, u); err != nil || used {
				continue
			}
			if _, err := c.uploads.DeleteOwnedObject(ctx, albumUploadFolder, u); err != nil {
				log.Printf("[ALBUMS] S3 delete %s: %v", u, err)
			}
		}
	}()
}

// callerUserID resolves the signed-in caller to a users.id, or uuid.Nil.
func (c *ProfileController) callerUserID(r *http.Request) (uuid.UUID, error) {
	uid, _ := r.Context().Value(auth.ContextKeyUID).(string)
	if uid == "" {
		return uuid.Nil, errors.New("not signed in")
	}
	user, err := c.userRepo.GetByAuthUID(r.Context(), uid)
	if err != nil || user == nil {
		return uuid.Nil, errors.New("user not found")
	}
	return user.ID, nil
}

func (c *ProfileController) requireCaller(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := c.callerUserID(r)
	if err != nil {
		RespondError(w, http.StatusUnauthorized, "Sign in to do that")
		return uuid.Nil, false
	}
	return id, true
}

func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}
