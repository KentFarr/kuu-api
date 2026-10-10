package provisioner

import (
	"context"
	"time"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

// The interfaces below list only the methods the provisioner and reaper
// call. Production wires in *pterodactyl.Client, *database.DB,
// *dns.CloudflareClient and *email.Queue; tests substitute in-memory fakes.

// PteroClient is the subset of the Pterodactyl Application API we use.
type PteroClient interface {
	CreateUser(ctx context.Context, email, requestedUsername string) (*pterodactyl.UserAttributes, pterodactyl.UsernameSource, error)
	DeleteUser(ctx context.Context, userID int) error
	PickLeastLoadedNode(ctx context.Context, nodeIDs []int, requiredMB int) (int, *pterodactyl.NodeUtilization, error)
	FindFreeAllocation(ctx context.Context, nodeID int, minPort, maxPort int) (*pterodactyl.AllocationAttributes, error)
	CreateServer(ctx context.Context, params pterodactyl.CreateServerParams) (*pterodactyl.ServerAttributes, error)
	SuspendServer(ctx context.Context, serverID int) error
	DeleteServer(ctx context.Context, serverID int) error
}

// Store is the subset of the database layer we use.
type Store interface {
	GetCustomerByEmail(email string) (*database.Customer, error)
	CreateCustomer(email string, pteroUserID int, pteroUsername string) (*database.Customer, error)
	UpdateStripeCustomerID(customerID int, stripeCustomerID string) error
	DeleteCustomer(customerID int) error

	CreateSubscription(customerID int, stripeSubID, stripePriceID, game string) (*database.Subscription, error)
	GetSubscriptionByStripeID(stripeSubID string) (*database.Subscription, error)
	SuspendSubscription(stripeSubID string) error
	DeleteSubscription(subscriptionID int) error

	CreateServer(p database.CreateServerParams) (*database.Server, error)
	GetServerBySubscription(subscriptionID int) (*database.Server, error)
	SuspendServer(subscriptionID int) error
	ListReapableServers(cutoff time.Time) ([]database.Server, error)
	DeleteServerByID(id int) error
}

// DNSClient manages the per-server CNAME records.
type DNSClient interface {
	CreateCNAME(ctx context.Context, subdomain, target string) error
	DeleteCNAME(ctx context.Context, subdomain string) error
}

// EmailQueue accepts fully rendered emails for background delivery.
type EmailQueue interface {
	Enqueue(toAddr, subject, body string) error
}
