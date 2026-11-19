package controller

import (
	"log"
	"net/http"
	"strconv"
	"time"
)

// ── Connections: one-way Connect with a host, saved instantly ──────────────

// ListMyConnections: GET /users/me/connections → [{host_id, created_at}]
func (c *ProfileController) ListMyConnections(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	list, err := c.connRepo.ListByUser(r.Context(), userID)
	if err != nil {
		log.Printf("[CONNECTIONS] list: %v", err)
		RespondError(w, http.StatusInternalServerError, "Could not load connections")
		return
	}
	RespondSuccess(w, http.StatusOK, list)
}

// Connect: PUT /users/me/connections/{host_id_or_slug}. Idempotent.
func (c *ProfileController) Connect(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	host, ok := c.lookupHost(w, r)
	if !ok {
		return
	}
	if host.UserID == userID {
		RespondError(w, http.StatusBadRequest, "You can't connect with yourself")
		return
	}
	if !c.connLimiter.Allow(userID.String()) {
		RespondError(w, http.StatusTooManyRequests, "Too many connects — try again in a bit")
		return
	}
	if _, err := c.connRepo.Connect(r.Context(), userID, host.ID); err != nil {
		log.Printf("[CONNECTIONS] connect: %v", err)
		RespondError(w, http.StatusInternalServerError, "Could not connect")
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]any{"host_id": host.ID, "connected": true})
}

// Disconnect: DELETE /users/me/connections/{host_id_or_slug}. Idempotent.
func (c *ProfileController) Disconnect(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	host, ok := c.lookupHost(w, r)
	if !ok {
		return
	}
	if err := c.connRepo.Disconnect(r.Context(), userID, host.ID); err != nil {
		log.Printf("[CONNECTIONS] disconnect: %v", err)
		RespondError(w, http.StatusInternalServerError, "Could not disconnect")
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]any{"host_id": host.ID, "connected": false})
}

// ListHostConnectors: GET /hosts/me/connections?limit=&before=<iso>
// The signed-in host's connections, newest first.
func (c *ProfileController) ListHostConnectors(w http.ResponseWriter, r *http.Request) {
	userID, ok := c.requireCaller(w, r)
	if !ok {
		return
	}
	host, err := c.hostService.GetHostByUserID(r.Context(), userID)
	if err != nil || host == nil {
		RespondError(w, http.StatusNotFound, "Host profile not found")
		return
	}
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 100)
	}
	var before *time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			RespondError(w, http.StatusBadRequest, "before must be an ISO timestamp")
			return
		}
		before = &t
	}
	list, err := c.connRepo.ListForHost(r.Context(), host.ID, before, limit)
	if err != nil {
		log.Printf("[CONNECTIONS] host list: %v", err)
		RespondError(w, http.StatusInternalServerError, "Could not load connections")
		return
	}
	total, _ := c.connRepo.CountForHost(r.Context(), host.ID)
	RespondSuccess(w, http.StatusOK, map[string]any{"items": list, "total": total})
}
