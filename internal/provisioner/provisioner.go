package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/dns"
	"github.com/KentFarr/kuu-api/internal/email"
	"github.com/KentFarr/kuu-api/internal/logging"
	"github.com/KentFarr/kuu-api/internal/metrics"
	"github.com/KentFarr/kuu-api/internal/plans"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

const (
	allocationRetryAttempts = 3
	cleanupTimeout          = 30 * time.Second
)

var allocationRetryBackoff = []time.Duration{
	250 * time.Millisecond,
	1 * time.Second,
	4 * time.Second,
}

type Provisioner struct {
	PteroClient *pterodactyl.Client
	DB          *database.DB
	EmailQueue  *email.Queue
	DNSClient   *dns.CloudflareClient
	NodeIDs     []int
}

func New(pteroClient *pterodactyl.Client, db *database.DB, emailQueue *email.Queue, dnsClient *dns.CloudflareClient, nodeIDs []int) *Provisioner {
	return &Provisioner{
		PteroClient: pteroClient,
		DB:          db,
		EmailQueue:  emailQueue,
		DNSClient:   dnsClient,
		NodeIDs:     nodeIDs,
	}
}

// Request bundles every field ProvisionServer needs. Using a struct keeps
// call sites readable now that we carry game + variant + version + Stripe
// IDs + requested username — a positional arg list got unwieldy fast.
type Request struct {
	CustomerEmail     string
	Game              string
	Variant           string // empty → game's DefaultVariant
	Version           string // empty → variant's VersionDefault
	Subdomain         string
	PriceID           string
	StripeSubID       string
	StripeCustomerID  string
	RequestedUsername string
}

type ProvisionResult struct {
	UserID     int
	Username   string
	Email      string
	Password   string
	ServerID   int
	ServerName string
	Port       int
	Game       string
	Variant    string
	Version    string
	Subdomain  string
	PlanName   string
	NewUser    bool
}

// cleanupCtxFor returns a Background-rooted context with the standard
// cleanup timeout, while preserving the request_id from the source ctx so
// rollback log lines stitch up with the request that triggered them. We
// can't reuse the request ctx itself because it may already be cancelled
// by the time the deferred rollback runs.
func cleanupCtxFor(src context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	if id := logging.RequestIDFrom(src); id != "" {
		ctx = logging.WithRequestID(ctx, id)
	}
	return ctx, cancel
}

func (p *Provisioner) ProvisionServer(ctx context.Context, req Request) (result *ProvisionResult, retErr error) {
	start := time.Now()
	defer func() {
		outcome := metrics.OutcomeOK
		if retErr != nil {
			outcome = metrics.OutcomeError
		}
		metrics.ProvisionDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}()

	logger := logging.FromContext(ctx)

	plan := plans.FindByPriceID(req.PriceID)
	if plan == nil {
		return nil, fmt.Errorf("unknown price ID: %s", req.PriceID)
	}

	gameConfig, ok := plans.Games[req.Game]
	if !ok {
		return nil, fmt.Errorf("unknown game: %s", req.Game)
	}
	if !gameConfig.Configured() {
		return nil, fmt.Errorf("game %s is not yet configured for provisioning", req.Game)
	}

	variant, ok := plans.ResolveVariant(req.Game, req.Variant)
	if !ok || !variant.Configured() {
		return nil, fmt.Errorf("game %s has no configured variant %q", req.Game, req.Variant)
	}
	resolvedVariantKey := req.Variant
	if resolvedVariantKey == "" {
		resolvedVariantKey = gameConfig.DefaultVariant
	}

	logger = logger.With(
		"game", req.Game,
		"variant", resolvedVariantKey,
		"plan", plan.Name,
		"email", req.CustomerEmail,
	)
	logger.Info("provisioning server")

	customer, err := p.DB.GetCustomerByEmail(req.CustomerEmail)
	if err != nil {
		return nil, fmt.Errorf("failed to look up customer: %w", err)
	}

	var (
		pteroUserID           int
		pteroUsername         string
		password              string
		newUser               bool
		createdPteroUser      bool
		createdCustomerID     int
		createdSubscriptionID int
	)

	if customer != nil && customer.PterodactylUserID != nil && customer.PterodactylUsername != nil {
		pteroUserID = *customer.PterodactylUserID
		pteroUsername = *customer.PterodactylUsername
		logger.Info("reusing existing pterodactyl user", "ptero_user_id", pteroUserID, "username", pteroUsername)
	} else {
		user, pwd, source, err := p.PteroClient.CreateUser(ctx, req.CustomerEmail, req.RequestedUsername)
		if err != nil {
			return nil, fmt.Errorf("failed to create pterodactyl user: %w", err)
		}
		pteroUserID = user.ID
		pteroUsername = user.Username
		password = pwd
		newUser = true
		createdPteroUser = true
		logger.Info("created pterodactyl user",
			"ptero_user_id", pteroUserID,
			"username", pteroUsername,
			"username_source", string(source),
		)

		defer func() {
			if createdPteroUser {
				cleanupCtx, cancel := cleanupCtxFor(ctx)
				defer cancel()
				clog := logging.FromContext(cleanupCtx)
				if delErr := p.PteroClient.DeleteUser(cleanupCtx, pteroUserID); delErr != nil {
					clog.Error("CRITICAL: failed to clean up orphaned pterodactyl user", "ptero_user_id", pteroUserID, "err", delErr)
				} else {
					clog.Info("cleaned up orphaned pterodactyl user", "ptero_user_id", pteroUserID)
				}
			}
		}()
	}

	if customer == nil {
		newCustomer, err := p.DB.CreateCustomer(req.CustomerEmail, pteroUserID, pteroUsername)
		if err != nil {
			return nil, fmt.Errorf("failed to store customer: %w", err)
		}
		customer = newCustomer
		createdCustomerID = newCustomer.ID

		defer func() {
			if createdCustomerID != 0 {
				if delErr := p.DB.DeleteCustomer(createdCustomerID); delErr != nil {
					logger.Error("CRITICAL: failed to roll back customer row", "customer_id", createdCustomerID, "err", delErr)
				} else {
					logger.Info("rolled back customer row", "customer_id", createdCustomerID)
				}
			}
		}()
	}

	if req.StripeCustomerID != "" {
		if err := p.DB.UpdateStripeCustomerID(customer.ID, req.StripeCustomerID); err != nil {
			logger.Warn("failed to update stripe customer id", "err", err)
		}
	}

	sub, err := p.DB.CreateSubscription(customer.ID, req.StripeSubID, req.PriceID, req.Game)
	if err != nil {
		return nil, fmt.Errorf("failed to store subscription: %w", err)
	}
	createdSubscriptionID = sub.ID

	defer func() {
		if createdSubscriptionID != 0 {
			if delErr := p.DB.DeleteSubscription(createdSubscriptionID); delErr != nil {
				logger.Error("CRITICAL: failed to roll back subscription row", "subscription_id", createdSubscriptionID, "err", delErr)
			} else {
				logger.Info("rolled back subscription row", "subscription_id", createdSubscriptionID)
			}
		}
	}()

	serverName := fmt.Sprintf("%s-%s-%d", req.Game, plan.Name, pteroUserID)
	server, allocPort, chosenNodeID, nodeAddress, resolvedVersion, err := p.createServerWithRetry(ctx, serverName, pteroUserID, variant, req.Version, plan, req.Game)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	createdServerID := server.ID
	defer func() {
		if createdServerID != 0 {
			cleanupCtx, cancel := cleanupCtxFor(ctx)
			defer cancel()
			clog := logging.FromContext(cleanupCtx)
			if delErr := p.PteroClient.DeleteServer(cleanupCtx, createdServerID); delErr != nil {
				clog.Error("CRITICAL: failed to clean up orphaned pterodactyl server", "ptero_server_id", createdServerID, "err", delErr)
			} else {
				clog.Info("cleaned up orphaned pterodactyl server", "ptero_server_id", createdServerID)
			}
		}
	}()

	_, err = p.DB.CreateServer(database.CreateServerParams{
		SubscriptionID:      sub.ID,
		CustomerID:          customer.ID,
		PterodactylServerID: server.ID,
		NodeID:              chosenNodeID,
		Port:                allocPort,
		ServerName:          serverName,
		Game:                req.Game,
		Variant:             resolvedVariantKey,
		Version:             resolvedVersion,
		Subdomain:           req.Subdomain,
		PlanName:            plan.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to store server record: %w", err)
	}

	// DNS Automation: Create CNAME record if subdomain is requested
	if req.Subdomain != "" && p.DNSClient != nil {
		if err := p.DNSClient.CreateCNAME(ctx, req.Subdomain, nodeAddress); err != nil {
			logger.Error("failed to create dns record", "subdomain", req.Subdomain, "err", err)
			// We don't roll back the whole provision if DNS fails — the server is up
			// and the user can still connect via the IP/node address. We just log
			// the error so operators can fix it manually or trigger a retry.
		} else {
			logger.Info("dns record created", "subdomain", req.Subdomain, "target", nodeAddress)
		}
	}

	pterodactyl.InvalidateNodeCache(chosenNodeID)
	metrics.ProvisionNodeChosen.WithLabelValues(strconv.Itoa(chosenNodeID)).Inc()

	createdPteroUser = false
	createdServerID = 0
	createdCustomerID = 0
	createdSubscriptionID = 0

	if p.EmailQueue != nil && newUser {
		address := nodeAddress
		if req.Subdomain != "" {
			address = fmt.Sprintf("%s.kuu.gg", req.Subdomain)
		}
		subject, body := email.RenderWelcome(email.WelcomeData{
			Username:    pteroUsername,
			Password:    password,
			ServerName:  serverName,
			Game:        req.Game,
			Port:        allocPort,
			NodeAddress: address,
		})
		if err := p.EmailQueue.Enqueue(req.CustomerEmail, subject, body); err != nil {
			logger.Warn("failed to enqueue welcome email", "err", err)
		}
	}

	return &ProvisionResult{
		UserID:     pteroUserID,
		Username:   pteroUsername,
		Email:      req.CustomerEmail,
		Password:   password,
		ServerID:   server.ID,
		ServerName: serverName,
		Port:       allocPort,
		Game:       req.Game,
		Variant:    resolvedVariantKey,
		Version:    resolvedVersion,
		Subdomain:  req.Subdomain,
		PlanName:   plan.Name,
		NewUser:    newUser,
	}, nil
}

func (p *Provisioner) createServerWithRetry(ctx context.Context, serverName string, pteroUserID int, variant plans.GameVariant, requestedVersion string, plan *plans.Plan, gameKey string) (*pterodactyl.ServerAttributes, int, int, string, string, error) {
	logger := logging.FromContext(ctx)

	gameConfig, ok := plans.Games[gameKey]
	if !ok {
		return nil, 0, 0, "", "", fmt.Errorf("unknown game: %s", gameKey)
	}

	var (
		lastErr        error
		appliedVersion string
		nodeAddress    string
	)
	for attempt := 1; attempt <= allocationRetryAttempts; attempt++ {
		if attempt > 1 {
			backoff := allocationRetryBackoff[min(attempt-2, len(allocationRetryBackoff)-1)]
			select {
			case <-ctx.Done():
				return nil, 0, 0, "", "", ctx.Err()
			case <-time.After(backoff):
			}
		}

		// Re-pick the node every attempt — a 422 collision often means
		// the node we picked just filled up; staying on it would burn
		// every retry on the same hot node.
		nodeID, util, pickErr := p.PteroClient.PickLeastLoadedNode(ctx, p.NodeIDs, plan.RAM)
		if pickErr != nil {
			lastErr = pickErr
			if errors.Is(pickErr, pterodactyl.ErrNoCapacity) {
				break
			}
			continue
		}

		if util != nil {
			nodeAddress = util.FQDN
		}

		alloc, err := p.PteroClient.FindFreeAllocation(ctx, nodeID, gameConfig.MinPort, gameConfig.MaxPort)
		if err != nil {
			lastErr = err
			pterodactyl.InvalidateNodeCache(nodeID)
			continue
		}

		// Build the env once we know the allocation port — Rust's
		// ConfigureEnv hook derives QUERY_PORT/RCON_PORT/APP_PORT from
		// the main game port. Per-server secrets (RCON_PASS) are
		// regenerated on every retry, which is wasteful but harmless.
		env, version, err := plans.BuildEnvironment(variant, requestedVersion, alloc.Port)
		if err != nil {
			lastErr = fmt.Errorf("build environment: %w", err)
			break
		}
		appliedVersion = version

		startup := variant.Startup
		if variant.TuningHook != nil {
			startup = variant.TuningHook(variant, plan.RAM)
		}

		logger.Info("allocation attempt",
			"attempt", attempt,
			"node_id", nodeID,
			"allocation_id", alloc.ID,
			"port", alloc.Port,
			"version", version,
		)

		server, err := p.PteroClient.CreateServer(ctx, pterodactyl.CreateServerParams{
			Name:            serverName,
			UserID:          pteroUserID,
			NestID:          variant.NestID,
			EggID:           variant.EggID,
			DockerImage:     variant.ResolveDockerImage(appliedVersion),
			Startup:         startup,
			Environment:     env,
			AllocationID:    alloc.ID,
			RAM:             plan.RAM,
			CPU:             plan.CPU,
			Disk:            plan.Disk,
			DatabaseLimit:   plan.DatabaseLimit,
			BackupLimit:     plan.BackupLimit,
			AllocationLimit: plan.AllocationLimit,
		})
		if err == nil {
			logger.Info("created server",
				"ptero_server_id", server.ID,
				"server_name", server.Name,
				"node_id", nodeID,
				"version", version,
			)
			return server, alloc.Port, nodeID, nodeAddress, version, nil
		}

		lastErr = err
		var apiErr *pterodactyl.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 422 {
			logger.Warn("allocation collided, retrying", "node_id", nodeID, "allocation_id", alloc.ID)
			pterodactyl.InvalidateNodeCache(nodeID)
			continue
		}
		break
	}
	return nil, 0, 0, nodeAddress, appliedVersion, lastErr
}

func (p *Provisioner) SuspendServer(ctx context.Context, stripeSubID string) error {
	logger := logging.FromContext(ctx).With("stripe_subscription_id", stripeSubID)
	logger.Info("suspending server for subscription")

	sub, err := p.DB.GetSubscriptionByStripeID(stripeSubID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	server, err := p.DB.GetServerBySubscription(sub.ID)
	if err != nil {
		logger.Warn("server record not found for subscription", "subscription_id", sub.ID, "err", err)
	} else if server.PterodactylServerID > 0 {
		if err := p.PteroClient.SuspendServer(ctx, server.PterodactylServerID); err != nil {
			return fmt.Errorf("failed to suspend pterodactyl server %d: %w", server.PterodactylServerID, err)
		}
	}

	if err := p.DB.SuspendSubscription(stripeSubID); err != nil {
		return fmt.Errorf("failed to suspend subscription record: %w", err)
	}

	if err := p.DB.SuspendServer(sub.ID); err != nil {
		return fmt.Errorf("failed to suspend server record: %w", err)
	}

	logger.Info("server suspended for subscription")
	return nil
}
