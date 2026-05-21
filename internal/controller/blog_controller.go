package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// BlogController handles HTTP requests for blog operations
type BlogController struct {
	blogRepo     repository.BlogRepository
	userRepo     repository.UserRepository
	firebaseAuth *fbauth.Client
	adminEmail   string
}

// NewBlogController Factory for BlogController
func NewBlogController(br repository.BlogRepository, ur repository.UserRepository, fa *fbauth.Client, adminEmail string) *BlogController {
	return &BlogController{
		blogRepo:     br,
		userRepo:     ur,
		firebaseAuth: fa,
		adminEmail:   adminEmail,
	}
}

// RegisterRoutes registers routes for the blog controller on the provided router
func (c *BlogController) RegisterRoutes(r chi.Router) {
	r.Route("/blogs", func(r chi.Router) {
		// Admin-only routes (registered first for priority)
		r.With(auth.IsAdmin(c.firebaseAuth, c.adminEmail)).Post("/", c.CreateBlog)
		r.With(auth.IsAdmin(c.firebaseAuth, c.adminEmail)).Get("/admin", c.ListAllBlogs)
		r.With(auth.IsAdmin(c.firebaseAuth, c.adminEmail)).Put("/{blogID}", c.UpdateBlog)
		r.With(auth.IsAdmin(c.firebaseAuth, c.adminEmail)).Delete("/{blogID}", c.DeleteBlog)
		r.With(auth.IsAdmin(c.firebaseAuth, c.adminEmail)).Post("/{blogID}/publish", c.PublishBlog)
		r.With(auth.IsAdmin(c.firebaseAuth, c.adminEmail)).Post("/{blogID}/unpublish", c.UnpublishBlog)

		// Public routes
		r.Get("/category/{category}", c.ListBlogsByCategory)
		r.Get("/", c.ListPublishedBlogs)
		r.Get("/{blogID}", c.GetBlog)
	})
}

// ── Request types ───────────────────────────────────────────────────────────

type CreateBlogRequest struct {
	Title           string  `json:"title" validate:"required"`
	Description     *string `json:"description,omitempty"`
	Category        string  `json:"category" validate:"required"`
	Content         string  `json:"content" validate:"required"`
	CoverImageURL   *string `json:"cover_image_url,omitempty"`
	ReadTimeMinutes int     `json:"read_time_minutes,omitempty"`
}

type UpdateBlogRequest struct {
	Title           string  `json:"title"`
	Description     *string `json:"description,omitempty"`
	Category        string  `json:"category"`
	Content         string  `json:"content"`
	CoverImageURL   *string `json:"cover_image_url,omitempty"`
	ReadTimeMinutes int     `json:"read_time_minutes,omitempty"`
}

// ── Handlers ────────────────────────────────────────────────────────────────

// CreateBlog creates a new blog post (admin only)
func (c *BlogController) CreateBlog(w http.ResponseWriter, r *http.Request) {
	var req CreateBlogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	// Get admin email from context
	// email, _ := r.Context().Value(auth.ContextKeyEmail).(string)
	authUID, _ := r.Context().Value(auth.ContextKeyUID).(string)

	if authUID == "" {
		RespondError(w, http.StatusUnauthorized, "missing authenticated admin uid")
		return
	}

	adminUser, err := c.userRepo.GetByAuthUID(r.Context(), authUID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "failed to resolve admin user")
		return
	}
	if adminUser == nil {
		RespondError(w, http.StatusBadRequest, "Blog creation failed because the admin account is authenticated but its app user record is out of sync. Sign in with the admin email and finish signup once, then retry.")
		return
	}

	// Set default read time if not provided
	if req.ReadTimeMinutes == 0 {
		req.ReadTimeMinutes = 5
	}

	blog := &models.Blog{
		Title:           req.Title,
		Description:     req.Description,
		Category:        req.Category,
		Content:         req.Content,
		CoverImageURL:   req.CoverImageURL,
		ReadTimeMinutes: req.ReadTimeMinutes,
		AuthorID:        adminUser.ID,
		AuthorName:      adminUser.Name,
	}

	if err := c.blogRepo.Create(r.Context(), blog); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, blog)
}

// isAdminRequest verifies the optional bearer token on a public route and
// reports whether the caller is a configured admin. Unlike the IsAdmin
// middleware it never rejects the request — a missing/invalid token simply
// yields false. Used to selectively expose drafts to admins.
func (c *BlogController) isAdminRequest(r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}
	idToken := strings.TrimPrefix(authHeader, "Bearer ")
	if idToken == authHeader {
		return false
	}
	token, err := c.firebaseAuth.VerifyIDToken(r.Context(), idToken)
	if err != nil {
		return false
	}
	email, _ := token.Claims["email"].(string)
	if email == "" {
		return false
	}
	for _, a := range strings.Split(c.adminEmail, ",") {
		if strings.EqualFold(email, strings.TrimSpace(a)) {
			return true
		}
	}
	return false
}

// GetBlog retrieves a single blog post by ID. Draft (unpublished) blogs are
// only returned to admins; everyone else gets a 404 so drafts stay hidden.
func (c *BlogController) GetBlog(w http.ResponseWriter, r *http.Request) {
	blogIDStr := chi.URLParam(r, "blogID")
	blogID, err := uuid.Parse(blogIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid blog ID")
		return
	}

	blog, err := c.blogRepo.GetByID(r.Context(), blogID)
	if err != nil {
		RespondError(w, http.StatusNotFound, "Blog not found")
		return
	}

	if blog.PublishedAt == nil && !c.isAdminRequest(r) {
		RespondError(w, http.StatusNotFound, "Blog not found")
		return
	}

	RespondSuccess(w, http.StatusOK, blog)
}

// ListAllBlogs retrieves every blog including unpublished drafts (admin only)
func (c *BlogController) ListAllBlogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	blogs, err := c.blogRepo.ListAll(r.Context(), limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if blogs == nil {
		blogs = []*models.Blog{}
	}

	RespondSuccess(w, http.StatusOK, blogs)
}

// ListPublishedBlogs retrieves all published blogs with pagination
func (c *BlogController) ListPublishedBlogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	blogs, err := c.blogRepo.ListPublished(r.Context(), limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if blogs == nil {
		blogs = []*models.Blog{}
	}

	RespondSuccess(w, http.StatusOK, blogs)
}

// ListBlogsByCategory retrieves published blogs filtered by category
func (c *BlogController) ListBlogsByCategory(w http.ResponseWriter, r *http.Request) {
	category := chi.URLParam(r, "category")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	blogs, err := c.blogRepo.ListPublishedByCategory(r.Context(), category, limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if blogs == nil {
		blogs = []*models.Blog{}
	}

	RespondSuccess(w, http.StatusOK, blogs)
}

// UpdateBlog updates an existing blog post (admin only)
func (c *BlogController) UpdateBlog(w http.ResponseWriter, r *http.Request) {
	blogIDStr := chi.URLParam(r, "blogID")
	blogID, err := uuid.Parse(blogIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid blog ID")
		return
	}

	var req UpdateBlogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	blog, err := c.blogRepo.GetByID(r.Context(), blogID)
	if err != nil {
		RespondError(w, http.StatusNotFound, "Blog not found")
		return
	}

	// Update fields that were provided
	if req.Title != "" {
		blog.Title = req.Title
	}
	if req.Description != nil {
		blog.Description = req.Description
	}
	if req.Category != "" {
		blog.Category = req.Category
	}
	if req.Content != "" {
		blog.Content = req.Content
	}
	if req.CoverImageURL != nil {
		blog.CoverImageURL = req.CoverImageURL
	}
	if req.ReadTimeMinutes > 0 {
		blog.ReadTimeMinutes = req.ReadTimeMinutes
	}

	if err := c.blogRepo.Update(r.Context(), blog); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, blog)
}

// DeleteBlog deletes a blog post (admin only)
func (c *BlogController) DeleteBlog(w http.ResponseWriter, r *http.Request) {
	blogIDStr := chi.URLParam(r, "blogID")
	blogID, err := uuid.Parse(blogIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid blog ID")
		return
	}

	if err := c.blogRepo.Delete(r.Context(), blogID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "Blog deleted successfully"})
}

// PublishBlog publishes a blog post (admin only)
func (c *BlogController) PublishBlog(w http.ResponseWriter, r *http.Request) {
	blogIDStr := chi.URLParam(r, "blogID")
	blogID, err := uuid.Parse(blogIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid blog ID")
		return
	}

	if err := c.blogRepo.Publish(r.Context(), blogID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	blog, _ := c.blogRepo.GetByID(r.Context(), blogID)
	RespondSuccess(w, http.StatusOK, blog)
}

// UnpublishBlog unpublishes a blog post (admin only)
func (c *BlogController) UnpublishBlog(w http.ResponseWriter, r *http.Request) {
	blogIDStr := chi.URLParam(r, "blogID")
	blogID, err := uuid.Parse(blogIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid blog ID")
		return
	}

	if err := c.blogRepo.Unpublish(r.Context(), blogID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	blog, _ := c.blogRepo.GetByID(r.Context(), blogID)
	RespondSuccess(w, http.StatusOK, blog)
}
