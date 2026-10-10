package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/billingportal/session"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/email"
	"github.com/KentFarr/kuu-api/internal/logging"
)

// supportStore is the part of *database.DB the support endpoints read.
type supportStore interface {
	GetCustomerByEmail(email string) (*database.Customer, error)
	GetLatestActiveServerByCustomerID(customerID int) (*database.Server, error)
}

// supportPtero is the part of *pterodactyl.Client the support endpoints use.
type supportPtero interface {
	GetNodeFQDN(ctx context.Context, nodeID int) (string, error)
}

// emailEnqueuer is satisfied by *email.Queue.
type emailEnqueuer interface {
	Enqueue(toAddr, subject, body string) error
}

type SupportHandler struct {
	DB          supportStore
	PteroClient supportPtero
	EmailQueue  emailEnqueuer
	ReturnURL   string
}

type ResendWelcomeRequest struct {
	Email string `json:"email"`
}

type ManageSubscriptionRequest struct {
	Email string `json:"email"`
}

func (h *SupportHandler) HandleResendWelcome(w http.ResponseWriter, r *http.Request) {
	var req ResendWelcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	if req.Email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}

	logger := logging.FromContext(r.Context()).With("customer_email", req.Email)

	// 1. Look up customer
	customer, err := h.DB.GetCustomerByEmail(req.Email)
	if err != nil {
		logger.Error("failed to look up customer", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 🔒 Security: Don't reveal if email exists. Just return 200 OK.
	if customer == nil || customer.PterodactylUserID == nil {
		logger.Info("resend requested for unknown email or user")
		writeJSON(w, http.StatusOK, map[string]string{"message": "If an active subscription exists, a new welcome email has been sent."})
		return
	}

	// 2. Look up their latest active server
	server, err := h.DB.GetLatestActiveServerByCustomerID(customer.ID)
	if err != nil {
		logger.Error("failed to look up server", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if server == nil {
		logger.Info("resend requested but no active server found")
		writeJSON(w, http.StatusOK, map[string]string{"message": "If an active subscription exists, a new welcome email has been sent."})
		return
	}

	// 3. Get Node Address. The password isn't touched: the email points
	// to the panel's own "Forgot password" page instead.
	nodeAddr, err := h.PteroClient.GetNodeFQDN(r.Context(), server.NodeID)
	if err != nil {
		logger.Warn("failed to get node fqdn, using fallback", "err", err)
		nodeAddr = "node1.kuu.gg"
	}

	// 4. Enqueue Email
	subject, body := email.RenderWelcome(email.WelcomeData{
		Username:    *customer.PterodactylUsername,
		ServerName:  server.ServerName,
		Game:        server.Game,
		Port:        server.Port,
		NodeAddress: nodeAddr,
	})

	if err := h.EmailQueue.Enqueue(req.Email, subject, body); err != nil {
		logger.Error("failed to enqueue welcome email", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	logger.Info("welcome email resent successfully")
	writeJSON(w, http.StatusOK, map[string]string{"message": "If an active subscription exists, a new welcome email has been sent."})
}

func (h *SupportHandler) HandleManageSubscription(w http.ResponseWriter, r *http.Request) {
	var req ManageSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	if req.Email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}

	logger := logging.FromContext(r.Context()).With("customer_email", req.Email)

	// 1. Look up customer
	customer, err := h.DB.GetCustomerByEmail(req.Email)
	if err != nil {
		logger.Error("failed to look up customer", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 🔒 Security: Don't reveal if email exists. Just return 200 OK.
	if customer == nil || customer.StripeCustomerID == nil {
		logger.Info("manage requested for unknown email or non-stripe customer")
		writeJSON(w, http.StatusOK, map[string]string{"message": "If an active subscription exists, a management link has been sent to your email."})
		return
	}

	// 2. Create Stripe Billing Portal Session
	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(*customer.StripeCustomerID),
		ReturnURL: stripe.String(h.ReturnURL),
	}
	ps, err := session.New(params)
	if err != nil {
		logger.Error("failed to create billing portal session", "err", err)
		http.Error(w, "failed to create billing portal session", http.StatusInternalServerError)
		return
	}

	// 3. Enqueue Email
	subject, body := email.RenderManageSubscription(ps.URL)
	if err := h.EmailQueue.Enqueue(req.Email, subject, body); err != nil {
		logger.Error("failed to enqueue manage subscription email", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	logger.Info("manage subscription email enqueued successfully")
	writeJSON(w, http.StatusOK, map[string]string{"message": "If an active subscription exists, a management link has been sent to your email."})
}
