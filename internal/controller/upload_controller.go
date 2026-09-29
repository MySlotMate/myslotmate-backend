package controller

import (
	"fmt"
	"log"
	"net/http"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/storage"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
)

// UploadController provides a generic file upload endpoint.
// The frontend uploads files here first, gets back URLs, then includes those URLs
// in the JSON body when creating/updating events, reviews, support tickets, etc.
type UploadController struct {
	uploadService *storage.UploadService
	firebaseAuth  *fbauth.Client
	jwtSecret     string
}

func NewUploadController(us *storage.UploadService) *UploadController {
	return &UploadController{uploadService: us}
}

// WithAuth turns the endpoint from "anyone may fill our bucket" into
// "signed-in users may". Separate from the constructor so existing callers
// keep working.
func (c *UploadController) WithAuth(fa *fbauth.Client, jwtSecret string) *UploadController {
	c.firebaseAuth = fa
	c.jwtSecret = jwtSecret
	return c
}

func (c *UploadController) RegisterRoutes(r chi.Router) {
	r.Route("/upload", func(r chi.Router) {
		// Uploads cost storage and are attached to a host's own content, so the
		// caller has to be someone.
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))
		r.Post("/", c.Upload) // generic: POST /upload?folder=events/covers
	})
}

// Upload handles multipart file uploads.
//
// Query params:
//
//	folder – GCS path prefix (default "general"). Examples:
//	  events/covers, events/gallery, reviews/photos, support/evidence, hosts/documents
//
// Form field:
//
//	files – one or more files (SVG, PNG, JPG, PDF ≤ 10 MB each)
//
// Response: { "data": [ { "file_name": "...", "url": "...", "size": 12345 }, ... ] }
func (c *UploadController) Upload(w http.ResponseWriter, r *http.Request) {
	if c.uploadService == nil {
		RespondError(w, http.StatusServiceUnavailable, "File upload is not configured. Please set AWS_S3_BUCKET, AWS_ACCESS_KEY_ID, and AWS_SECRET_ACCESS_KEY environment variables.")
		return
	}

	const maxBody = 60 << 20 // 60 MB total
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseMultipartForm(maxBody); err != nil {
		msg := fmt.Sprintf("Invalid multipart form: %v", err)
		log.Printf("Upload error - ParseMultipartForm: %s", msg)
		RespondError(w, http.StatusBadRequest, msg)
		return
	}

	folder := r.URL.Query().Get("folder")
	if folder == "" {
		folder = "general"
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		msg := fmt.Sprintf("No files in 'files' form field. Available fields: %v", r.MultipartForm.File)
		log.Printf("Upload error - no files: %s", msg)
		RespondError(w, http.StatusBadRequest, msg)
		return
	}

	log.Printf("Uploading %d files to folder: %s", len(files), folder)
	results, err := c.uploadService.UploadMultiple(r.Context(), folder, files)
	if err != nil {
		msg := fmt.Sprintf("S3 upload failed: %v", err)
		log.Printf("Upload error - S3 failure: %s", msg)
		RespondError(w, http.StatusBadRequest, msg)
		return
	}

	log.Printf("Successfully uploaded %d files", len(results))
	RespondSuccess(w, http.StatusOK, results)
}
