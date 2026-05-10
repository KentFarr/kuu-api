package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/billingportal/session"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/email"
	"github.com/KentFarr/kuu-api/internal/logging"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

type SupportHandler struct {
	DB          *database.DB
	PteroClient *pterodactyl.Client
	EmailQueue  *email.Queue
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
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "If an active subscription exists, a new welcome email has been sent."})
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
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "If an active subscription exists, a new welcome email has been sent."})
		return
	}

	// 3. Reset password in Pterodactyl
	newPassword, err := pterodactyl.GeneratePassword()
	if err != nil {
		logger.Error("failed to generate new password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err = h.PteroClient.UpdateUserPassword(r.Context(), *customer.PterodactylUserID, newPassword)
	if err != nil {
		logger.Error("failed to reset pterodactyl password", "err", err)
		http.Error(w, "failed to reset password", http.StatusInternalServerError)
		return
	}

	// 4. Get Node Address
	nodeAddr, err := h.PteroClient.GetNodeFQDN(r.Context(), server.NodeID)
	if err != nil {
		logger.Warn("failed to get node fqdn, using fallback", "err", err)
		nodeAddr = "node1.kuu.gg"
	}

	// 5. Enqueue Email
	subject, body := email.RenderWelcome(email.WelcomeData{
		Username:    *customer.PterodactylUsername,
		Password:    newPassword,
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
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "If an active subscription exists, a new welcome email has been sent."})
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
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "If an active subscription exists, a management link has been sent to your email."})
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
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "If an active subscription exists, a management link has been sent to your email."})
}
