package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/bookingimport"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	fbauth "firebase.google.com/go/v4/auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// BookingImportController exposes the host bulk-booking upload: download a
// template, upload a filled .xlsx, then poll the resulting job for progress.
//
// Auth posture: every route is behind RequireUser and derives the acting host
// from the token via resolveHostID — never from the body. This is deliberately
// stricter than the sibling /host/bookings/walk-in/{initiate,complete} routes,
// which still read host_id from the body: one call here creates up to 1000
// bookings, so an unauthenticated body-supplied host_id would be a mass-booking
// vandalism tool against any host's events.
type BookingImportController struct {
	importService service.BookingImportService
	userRepo      repository.UserRepository
	hostRepo      repository.HostRepository
	firebaseAuth  *fbauth.Client
	jwtSecret     string
}

func NewBookingImportController(
	is service.BookingImportService,
	ur repository.UserRepository,
	hr repository.HostRepository,
	fa *fbauth.Client,
	jwtSecret string,
) *BookingImportController {
	return &BookingImportController{
		importService: is,
		userRepo:      ur,
		hostRepo:      hr,
		firebaseAuth:  fa,
		jwtSecret:     jwtSecret,
	}
}

func (c *BookingImportController) RegisterRoutes(r chi.Router) {
	r.Route("/host/bookings/import", func(r chi.Router) {
		r.Use(auth.RequireUser(c.firebaseAuth, c.jwtSecret))
		r.Get("/template", c.DownloadTemplate)
		r.Get("/validate", c.ValidateEvent)
		r.Post("/", c.Upload)
		r.Get("/", c.ListJobs)
		r.Get("/{jobID}", c.GetJob)
		r.Get("/{jobID}/rows", c.ListRows)
		r.Get("/{jobID}/report", c.DownloadReport)
	})
}

// maxImportUpload caps the request body. A 1000-row three-column .xlsx is well
// under 100KB; 5MB leaves generous room for the formatting Excel adds.
const maxImportUpload = 5 << 20

// resolveHostID derives the acting host from the auth context. Mirrors
// WalkInController.resolveHostID — any host_id in the request is ignored.
func (c *BookingImportController) resolveHostID(ctx context.Context) (uuid.UUID, error) {
	uid, ok := ctx.Value(auth.ContextKeyUID).(string)
	if !ok || uid == "" {
		return uuid.Nil, errors.New("unauthenticated")
	}
	user, err := c.userRepo.GetByAuthUID(ctx, uid)
	if err != nil {
		return uuid.Nil, fmt.Errorf("user lookup failed: %w", err)
	}
	if user == nil {
		return uuid.Nil, errors.New("user not found")
	}
	host, err := c.hostRepo.GetByUserID(ctx, user.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("host lookup failed: %w", err)
	}
	if host == nil {
		return uuid.Nil, errors.New("caller is not a host")
	}
	return host.ID, nil
}

// DownloadTemplate serves the .xlsx template, generated from the same column
// definitions the upload validator checks against.
// GET /host/bookings/import/template
func (c *BookingImportController) DownloadTemplate(w http.ResponseWriter, r *http.Request) {
	if _, err := c.resolveHostID(r.Context()); err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	f, err := bookingimport.BuildTemplate()
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "could not build the template")
		return
	}
	defer f.Close()

	// Buffer first: writing straight to the ResponseWriter would commit a 200 and
	// then have no way to report a mid-stream failure.
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		RespondError(w, http.StatusInternalServerError, "could not build the template")
		return
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="myslotmate-booking-template.xlsx"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// ValidateEvent lets the UI check importability before the host picks a file.
// GET /host/bookings/import/validate?event_id=<uuid>&coupon_code=<code>
func (c *BookingImportController) ValidateEvent(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	eventID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("event_id")))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "valid event_id is required")
		return
	}

	eligibility, err := c.importService.ValidateEventForImport(r.Context(), hostID, eventID,
		strings.TrimSpace(r.URL.Query().Get("coupon_code")))
	if err != nil {
		respondImportError(w, err)
		return
	}
	// Carries payment_mode + unit_price_cents so the modal can show the host what
	// they are taking on for a PAID event, rather than silently accepting it.
	RespondSuccess(w, http.StatusOK, eligibility)
}

// Upload accepts the filled spreadsheet and starts a background job.
// POST /host/bookings/import  (multipart: file, event_id, occurrence_date?, coupon_code?)
func (c *BookingImportController) Upload(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxImportUpload)
	if err := r.ParseMultipartForm(maxImportUpload); err != nil {
		RespondError(w, http.StatusBadRequest, "file is too large or the upload is malformed (limit 5MB)")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		RespondError(w, http.StatusBadRequest, "a spreadsheet file is required")
		return
	}
	defer file.Close()

	// .xls (the pre-2007 binary format) is a different container that excelize
	// cannot read, and its error message is unhelpful — name it here instead.
	name := header.Filename
	if lower := strings.ToLower(name); !strings.HasSuffix(lower, ".xlsx") {
		RespondError(w, http.StatusBadRequest,
			"please upload a .xlsx file — if yours is .xls or .csv, open it and use Save As → Excel Workbook (.xlsx)")
		return
	}

	eventID, err := uuid.Parse(strings.TrimSpace(r.FormValue("event_id")))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "valid event_id is required")
		return
	}
	occ, err := parseOptionalOccurrence(strings.TrimSpace(r.FormValue("occurrence_date")))
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	job, err := c.importService.StartImport(r.Context(), service.BookingImportRequest{
		HostID:         hostID,
		EventID:        eventID,
		OccurrenceDate: occ,
		CouponCode:     strings.TrimSpace(r.FormValue("coupon_code")),
		OfflineAck:     r.FormValue("offline_ack") == "true",
		FileName:       name,
		File:           file,
	})
	if err != nil {
		respondImportError(w, err)
		return
	}
	// 202: the rows are still being booked. The client polls GET /{jobID}.
	RespondSuccess(w, http.StatusAccepted, job)
}

// GetJob returns job counters plus the failed rows — the whole progress UI.
// GET /host/bookings/import/{jobID}
func (c *BookingImportController) GetJob(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	status, err := c.importService.GetJob(r.Context(), hostID, jobID)
	if err != nil {
		respondImportError(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, status)
}

// ListRows returns the job's rows, optionally filtered by status, for the
// on-screen report tabs.
// GET /host/bookings/import/{jobID}/rows?status=success|failed
func (c *BookingImportController) ListRows(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "", models.ImportRowSuccess, models.ImportRowFailed, models.ImportRowPending:
	default:
		RespondError(w, http.StatusBadRequest, "status must be success, failed or pending")
		return
	}

	rows, err := c.importService.ListRows(r.Context(), hostID, jobID, status)
	if err != nil {
		respondImportError(w, err)
		return
	}
	RespondSuccess(w, http.StatusOK, rows)
}

// DownloadReport serves the finished import as an .xlsx: a summary sheet plus
// every row with its outcome and failure reason.
// GET /host/bookings/import/{jobID}/report
func (c *BookingImportController) DownloadReport(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	f, filename, err := c.importService.BuildReport(r.Context(), hostID, jobID)
	if err != nil {
		respondImportError(w, err)
		return
	}
	defer f.Close()

	// Buffer before writing: streaming straight out would commit a 200 status
	// with no way to report a mid-stream failure.
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		RespondError(w, http.StatusInternalServerError, "could not build the report")
		return
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// ListJobs returns the host's recent imports.
// GET /host/bookings/import?limit=20
func (c *BookingImportController) ListJobs(w http.ResponseWriter, r *http.Request) {
	hostID, err := c.resolveHostID(r.Context())
	if err != nil {
		RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	jobs, err := c.importService.ListJobs(r.Context(), hostID, limit)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []*models.BookingImportJob{}
	}
	RespondSuccess(w, http.StatusOK, jobs)
}

func respondImportError(w http.ResponseWriter, err error) {
	// A header mismatch carries structured detail so the UI can show exactly which
	// columns are missing rather than a wall of text.
	var herr *bookingimport.HeaderError
	if errors.As(err, &herr) {
		RespondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Message: herr.Error(),
			Error:   herr.Error(),
			Data:    map[string]any{"header_error": herr},
		})
		return
	}

	switch {
	case errors.Is(err, service.ErrImportJobNotFound):
		RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrImportNotOwner):
		RespondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, service.ErrImportOfflineAckRequired):
		// 428: the request is valid but needs the host's explicit confirmation.
		RespondError(w, http.StatusPreconditionRequired, err.Error())
	case errors.Is(err, service.ErrImportAttendeeDetails),
		errors.Is(err, service.ErrImportTiered):
		RespondError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		if err.Error() == "event not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		// Parse/validation failures ("the spreadsheet is empty", "too many rows",
		// a bad coupon code) are all the host's to fix.
		RespondError(w, http.StatusBadRequest, err.Error())
	}
}
