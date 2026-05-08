package plans

import (
	"fmt"
	"os"
)

type Plan struct {
	Name    string
	Price   int
	CPU     int
	Disk    int
	PriceID string
	RAM     int
}

type Game struct {
	Name        string
	EggID       int
	NestID      int
	DockerImage string
	Startup     string
	Environment map[string]string
}

func (g Game) Configured() bool {
	return g.EggID != 0 && g.NestID != 0 && g.DockerImage != "" && g.Startup != ""
}

var Games = map[string]Game{
	"minecraft": {
		Name:        "Minecraft",
		EggID:       1,
		NestID:      1,
		DockerImage: "ghcr.io/pterodactyl/yolks:java_21",
		Startup:     "java -Xms128M -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}}",
		Environment: map[string]string{
			"SERVER_JARFILE":  "server.jar",
			"VANILLA_VERSION": "latest",
			"BUILD_NUMBER":    "latest",
		},
	},
	"rust": {
		Name: "Rust",
	},
	"fivem": {
		Name: "FiveM",
	},
}

func LoadPlans() []Plan {
	return []Plan{
		{
			Name:    "2GB",
			RAM:     2048,
			CPU:     100,
			Disk:    10240,
			PriceID: os.Getenv("STRIPE_PRICE_2GB"),
		},
		{
			Name:    "4GB",
			RAM:     4096,
			CPU:     150,
			Disk:    20480,
			PriceID: os.Getenv("STRIPE_PRICE_4GB"),
		},
		{
			Name:    "8GB",
			RAM:     8192,
			CPU:     200,
			Disk:    40960,
			PriceID: os.Getenv("STRIPE_PRICE_8GB"),
		},
		{
			Name:    "16GB",
			RAM:     16384,
			CPU:     300,
			Disk:    81920,
			PriceID: os.Getenv("STRIPE_PRICE_16GB"),
		},
		{
			Name:    "24GB",
			RAM:     24576,
			CPU:     400,
			Disk:    122880,
			PriceID: os.Getenv("STRIPE_PRICE_24GB"),
		},
		{
			Name:    "32GB",
			RAM:     32768,
			CPU:     500,
			Disk:    163840,
			PriceID: os.Getenv("STRIPE_PRICE_32GB"),
		},
	}
}

func ValidateConfigured() error {
	var missing []string
	for _, p := range LoadPlans() {
		if p.PriceID == "" {
			missing = append(missing, fmt.Sprintf("STRIPE_PRICE_%s", p.Name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing Stripe price env vars: %v", missing)
	}
	return nil
}

func FindByPriceID(priceID string) *Plan {
	if priceID == "" {
		return nil
	}
	for _, plan := range LoadPlans() {
		if plan.PriceID == "" {
			continue
		}
		if plan.PriceID == priceID {
			return &plan
		}
	}
	return nil
}
