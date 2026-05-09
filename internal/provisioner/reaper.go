package provisioner

import (
	"context"
	"log"
	"time"

	"github.com/KentFarr/kuu-api/internal/logging"
)

const (
	defaultReapInterval     = 24 * time.Hour
	defaultReapGracePeriod  = 14 * 24 * time.Hour
	reapInitialDelayJitter  = 5 * time.Minute
	reapPerServerTimeout    = 30 * time.Second
)

// Reaper hard-deletes servers whose subscription has been suspended longer
// than GracePeriod. Subscription and customer rows are left in place so
// billing history remains intact; the Pterodactyl user account is also
// preserved so a returning customer keeps their login.
type Reaper struct {
	Provisioner *Provisioner
	Interval    time.Duration
	GracePeriod time.Duration
}

func NewReaper(p *Provisioner) *Reaper {
	return &Reaper{
		Provisioner: p,
		Interval:    defaultReapInterval,
		GracePeriod: defaultReapGracePeriod,
	}
}

// Run blocks until ctx is cancelled. Designed to be launched in its own
// goroutine from main; the parent should cancel ctx during shutdown.
func (r *Reaper) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(reapInitialDelayJitter):
	}

	r.runOnce(ctx)

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("server reaper stopping")
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *Reaper) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	cutoff := time.Now().Add(-r.GracePeriod)
	servers, err := r.Provisioner.DB.ListReapableServers(cutoff)
	if err != nil {
		log.Printf("server reaper: list failed: %v", err)
		return
	}
	if len(servers) == 0 {
		return
	}

	log.Printf("server reaper: %d server(s) past %s grace period", len(servers), r.GracePeriod)

	for _, s := range servers {
		if ctx.Err() != nil {
			return
		}
		r.reapOne(ctx, s.ID, s.PterodactylServerID, s.ServerName)
	}
}

func (r *Reaper) reapOne(ctx context.Context, serverID, pteroServerID int, name string) {
	logger := logging.FromContext(ctx).With(
		"server_id", serverID,
		"pterodactyl_server_id", pteroServerID,
		"server_name", name,
	)

	pteroCtx, cancel := context.WithTimeout(ctx, reapPerServerTimeout)
	defer cancel()

	if err := r.Provisioner.PteroClient.DeleteServer(pteroCtx, pteroServerID); err != nil {
		logger.Error("reaper: pterodactyl delete failed, leaving DB row in place", "error", err)
		return
	}

	if err := r.Provisioner.DB.DeleteServerByID(serverID); err != nil {
		logger.Error("reaper: db delete failed after pterodactyl delete succeeded — manual cleanup needed", "error", err)
		return
	}

	logger.Info("reaper: server deleted after grace period")
}
