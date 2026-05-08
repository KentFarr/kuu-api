package handlers

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/logging"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

const adminPendingEmailsLimit = 200

// AdminHandler exposes recovery + observability endpoints. The token is
// compared in constant time so a curious attacker can't time it.
//
// Endpoints (all require Authorization: Bearer <token>):
//
//	POST /admin/emails/{id}/retry  — clear backoff so the worker picks the
//	                                 row on its next tick.
//	GET  /admin/emails             — list unsent rows (capped).
//	GET  /admin/reconciliation     — diff Pterodactyl's view of this node
//	                                 against our DB so ops can spot drift.
type AdminHandler struct {
	Token  string
	DB     *database.DB
	Ptero  *pterodactyl.Client
	NodeID int
}

func (h *AdminHandler) authorized(r *http.Request) bool {
	if h.Token == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.Token)) == 1
}

func (h *AdminHandler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// Routes registers admin endpoints onto the given mux. Idempotent — call
// once at startup. When the bearer token is empty the routes are not
// registered at all so the surface area is genuinely off.
func (h *AdminHandler) Routes(mux *http.ServeMux) {
	if h.Token == "" {
		return
	}
	mux.HandleFunc("POST /admin/emails/{id}/retry", h.guard(h.retryEmail))
	mux.HandleFunc("GET /admin/emails", h.guard(h.listPendingEmails))
	mux.HandleFunc("GET /admin/reconciliation", h.guard(h.reconciliation))
}

func (h *AdminHandler) retryEmail(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.DB.ResetEmailForRetry(id); err != nil {
		logging.FromContext(r.Context()).Warn("admin retry email failed", "id", id, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	logging.FromContext(r.Context()).Info("admin reset email for retry", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "queued": true})
}

func (h *AdminHandler) listPendingEmails(w http.ResponseWriter, r *http.Request) {
	emails, err := h.DB.ListPendingEmails(adminPendingEmailsLimit)
	if err != nil {
		logging.FromContext(r.Context()).Error("admin list pending emails failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"emails": emails,
		"count":  len(emails),
		"limit":  adminPendingEmailsLimit,
	})
}

// reconciliation classifies every server known to either side:
//   - tracked:   present in both Pterodactyl and our DB
//   - orphaned:  in Pterodactyl, missing from our DB (likely a failed
//     provision that didn't roll back, or a manual create)
//   - dangling:  in our DB, missing from Pterodactyl (panel-side delete)
//
// Ops can use this to find resources that need manual cleanup. We don't
// take action automatically — admin reads only, action is operator-led.
func (h *AdminHandler) reconciliation(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromContext(r.Context())

	pteroServers, err := h.Ptero.ListServersOnNode(r.Context(), h.NodeID)
	if err != nil {
		logger.Error("admin reconciliation: ptero list failed", "err", err)
		http.Error(w, "pterodactyl unavailable", http.StatusBadGateway)
		return
	}
	dbIDs, err := h.DB.ListServerPteroIDs(h.NodeID)
	if err != nil {
		logger.Error("admin reconciliation: db list failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	dbSet := make(map[int]struct{}, len(dbIDs))
	for _, id := range dbIDs {
		dbSet[id] = struct{}{}
	}
	pteroSet := make(map[int]struct{}, len(pteroServers))

	type entry struct {
		PterodactylServerID int    `json:"pterodactyl_server_id"`
		Name                string `json:"name,omitempty"`
		Suspended           bool   `json:"suspended,omitempty"`
	}
	var (
		tracked  []entry
		orphaned []entry
		dangling []entry
	)
	for _, s := range pteroServers {
		pteroSet[s.ID] = struct{}{}
		e := entry{PterodactylServerID: s.ID, Name: s.Name, Suspended: s.Suspended}
		if _, ok := dbSet[s.ID]; ok {
			tracked = append(tracked, e)
		} else {
			orphaned = append(orphaned, e)
		}
	}
	for _, id := range dbIDs {
		if _, ok := pteroSet[id]; !ok {
			dangling = append(dangling, entry{PterodactylServerID: id})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":  h.NodeID,
		"tracked":  tracked,
		"orphaned": orphaned,
		"dangling": dangling,
		"counts": map[string]int{
			"tracked":  len(tracked),
			"orphaned": len(orphaned),
			"dangling": len(dangling),
		},
	})
}
