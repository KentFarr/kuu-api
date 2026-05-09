package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stripe/stripe-go/v82"
	"golang.org/x/time/rate"

	"github.com/KentFarr/kuu-api/internal/config"
	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/email"
	"github.com/KentFarr/kuu-api/internal/handlers"
	"github.com/KentFarr/kuu-api/internal/logging"
	"github.com/KentFarr/kuu-api/internal/metrics"
	"github.com/KentFarr/kuu-api/internal/plans"
	"github.com/KentFarr/kuu-api/internal/provisioner"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

func main() {
	logging.Init()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if err := plans.ValidateConfigured(); err != nil {
		log.Fatalf("invalid plan configuration: %v", err)
	}

	stripe.Key = cfg.StripeSecretKey

	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	pteroClient := pterodactyl.NewClient(cfg.PterodactylURL, cfg.PterodactylAPIKey)

	var (
		mailer     *email.Mailer
		emailQueue *email.Queue
	)
	if cfg.SMTPHost != "" {
		mailer = email.New(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom)
		emailQueue = email.NewQueue(db, mailer)
		log.Println("email service configured")
	} else {
		log.Println("email service not configured, skipping")
	}

	prov := provisioner.New(pteroClient, db, emailQueue, cfg.NodeIDs)

	checkout := &handlers.CheckoutHandler{
		SuccessURL: cfg.CheckoutSuccessURL,
		CancelURL:  cfg.CheckoutCancelURL,
	}

	wh := &handlers.WebhookHandler{
		WebhookSecret: cfg.StripeWebhookSecret,
		Provisioner:   prov,
		DB:            db,
	}

	health := &handlers.HealthHandler{DB: db, Ptero: pteroClient, Mail: mailer, NodeIDs: cfg.NodeIDs}

	checkoutLimiter := handlers.NewIPRateLimiter(rate.Every(2*time.Second), 5, cfg.TrustedProxyCIDRs)

	admin := &handlers.AdminHandler{
		Token:   cfg.AdminBearerToken,
		DB:      db,
		Ptero:   pteroClient,
		NodeIDs: cfg.NodeIDs,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)
	mux.HandleFunc("GET /health", health.Live)
	mux.Handle("POST /api/checkout", checkoutLimiter.Middleware(http.HandlerFunc(checkout.HandleCheckout)))
	mux.HandleFunc("POST /api/webhook", wh.HandleWebhook)
	mux.Handle("GET /metrics", metricsHandler(cfg.AdminBearerToken))
	admin.Routes(mux)
	if cfg.AdminBearerToken == "" {
		log.Println("admin endpoints disabled (ADMIN_BEARER_TOKEN unset)")
	} else {
		log.Println("admin endpoints enabled at /admin/*")
	}

	handler := handlers.SecurityHeaders(handlers.CORSMiddleware(cfg.CORSAllowedOrigin)(handlers.RequestIDMiddleware(mux)))

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var workers sync.WaitGroup
	if emailQueue != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			emailQueue.Run(ctx)
		}()
	}

	cleaner := database.NewCleaner(db)
	workers.Add(1)
	go func() {
		defer workers.Done()
		cleaner.Run(ctx)
	}()

	dbStats := &metrics.DBStatsCollector{DB: db.SQL()}
	workers.Add(1)
	go func() {
		defer workers.Done()
		dbStats.Run(ctx)
	}()

	nodeMetrics := &metrics.NodeMetricsCollector{
		NodeIDs: cfg.NodeIDs,
		Fetcher: func(ctx context.Context, nodeID int) (*metrics.NodeUtilSample, error) {
			u, err := pteroClient.GetNodeUtilization(ctx, nodeID, false)
			if err != nil {
				return nil, err
			}
			return &metrics.NodeUtilSample{
				NodeID:          u.NodeID,
				MemoryUsedMB:    u.MemoryUsedMB,
				MemoryTotalMB:   u.MemoryTotalMB,
				FreeAllocations: u.FreeAllocations,
			}, nil
		},
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		nodeMetrics.Run(ctx)
	}()

	go func() {
		log.Printf("kuu-api starting on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutdown signal received, draining connections")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	} else {
		log.Println("server stopped cleanly")
	}

	workers.Wait()
	log.Println("background workers stopped")
}

// metricsHandler exposes Prometheus exposition. When ADMIN_BEARER_TOKEN is
// set we require it on /metrics too — same secret as /admin/*, so a single
// rotation rotates both. When unset we serve openly under the assumption
// that the operator has bound the service to a private network or scoped
// /metrics behind a reverse-proxy ACL.
func metricsHandler(token string) http.Handler {
	base := promhttp.Handler()
	if token == "" {
		return base
	}
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) == 1 {
			base.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}
