// Package config loads runtime configuration from environment variables.
//
// godotenv.Load() picks up a .env file in dev; in production no .env is
// present and env vars come from the orchestrator's secret store. The Load
// function fails fast if any required variable is missing or malformed.
package config
