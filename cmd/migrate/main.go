package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/KentFarr/kuu-api/internal/config"
	"github.com/KentFarr/kuu-api/internal/database"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ctx := context.Background()

	switch os.Args[1] {
	case "up":
		if err := database.MigrateUp(ctx, cfg.DatabaseURL); err != nil {
			log.Fatalf("migrate up failed: %v", err)
		}
		fmt.Println("migrations up to date")
	case "down":
		if err := database.MigrateDown(ctx, cfg.DatabaseURL); err != nil {
			log.Fatalf("migrate down failed: %v", err)
		}
	case "status":
		if err := database.MigrateStatus(ctx, cfg.DatabaseURL); err != nil {
			log.Fatalf("migrate status failed: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kuu-migrate <up|down|status>")
}
