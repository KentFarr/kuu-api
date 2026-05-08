package provisioner

import (
	"fmt"
	"log"

	"github.com/KentFarr/kuu-api/internal/plans"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

type Provisioner struct {
	PteroClient *pterodactyl.Client
	NodeID      int
}

func New(pteroClient *pterodactyl.Client, nodeID int) *Provisioner {
	return &Provisioner{
		PteroClient: pteroClient,
		NodeID:      nodeID,
	}
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
	PlanName   string
}

func (p *Provisioner) ProvisionServer(email, game, priceID string) (*ProvisionResult, error) {
	plan := plans.FindByPriceID(priceID)
	if plan == nil {
		return nil, fmt.Errorf("unknown price ID: %s", priceID)
	}

	log.Printf("provisioning %s server (%s) for %s", game, plan.Name, email)

	gameConfig, ok := plans.Games[game]
	if !ok {
		return nil, fmt.Errorf("unknown game: %s", game)
	}

	user, password, err := p.PteroClient.CreateUser(email)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	log.Printf("created pterodactyl user: id=%d username=%s", user.ID, user.Username)

	alloc, err := p.PteroClient.FindFreeAllocation(p.NodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to find free allocation: %w", err)
	}

	log.Printf("found free allocation: id=%d port=%d", alloc.ID, alloc.Port)

	serverName := fmt.Sprintf("%s-%s-%d", game, plan.Name, user.ID)

	server, err := p.PteroClient.CreateServer(pterodactyl.CreateServerParams{
		Name:         serverName,
		UserID:       user.ID,
		NestID:       gameConfig.NestID,
		EggID:        gameConfig.EggID,
		DockerImage:  gameConfig.DockerImage,
		AllocationID: alloc.ID,
		RAM:          plan.RAM,
		CPU:          plan.CPU,
		Disk:         plan.Disk,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	log.Printf("created server: id=%d name=%s", server.ID, server.Name)

	return &ProvisionResult{
		UserID:     user.ID,
		Username:   user.Username,
		Email:      email,
		Password:   password,
		ServerID:   server.ID,
		ServerName: serverName,
		Port:       alloc.Port,
		Game:       game,
		PlanName:   plan.Name,
	}, nil
}