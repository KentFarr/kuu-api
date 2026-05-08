package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/email"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

const readinessTimeout = 5 * time.Second

// HealthHandler reports two distinct signals: liveness (the process is
// running and not deadlocked) and readiness (every downstream the request
// path actually needs is reachable). Splitting them lets an orchestrator
// stop routing traffic during a Pterodactyl/SMTP outage without restarting
// the pod, which would just churn against the same outage.
type HealthHandler struct {
	DB    *database.DB
	Ptero *pterodactyl.Client
	Mail  *email.Mailer
}

// Live always returns 200 unless the process itself is wedged. The HTTP
// server answering at all is the proof of liveness.
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "kuu-api",
		"status":  "ok",
	})
}

// Ready pings every dependency the request path touches and returns 503 if
// any check fails. The orchestrator should wire this into its readiness
// probe so checkout traffic is shed when a downstream is degraded.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	checks := map[string]string{
		"database":    checkErr(h.DB.Ping(ctx)),
		"pterodactyl": checkPtero(ctx, h.Ptero),
		"smtp":        checkSMTP(ctx, h.Mail),
	}

	status := http.StatusOK
	for _, v := range checks {
		if v != "ok" && v != "skipped" {
			status = http.StatusServiceUnavailable
			break
		}
	}

	writeJSON(w, status, map[string]any{
		"service": "kuu-api",
		"checks":  checks,
	})
}

func checkErr(err error) string {
	if err != nil {
		return "unavailable"
	}
	return "ok"
}

func checkPtero(ctx context.Context, c *pterodactyl.Client) string {
	if c == nil {
		return "skipped"
	}
	return checkErr(c.Ping(ctx))
}

func checkSMTP(ctx context.Context, m *email.Mailer) string {
	if m == nil {
		return "skipped"
	}
	return checkErr(m.Ping(ctx))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
