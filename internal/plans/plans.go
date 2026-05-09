package plans

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
)

type Plan struct {
	Name          string
	Price         int
	CPU           int
	Disk          int
	PriceID       string
	RAM           int
	BackupLimit   int
	DatabaseLimit int
}

// Game is a top-level user-facing game choice (e.g. "minecraft", "rust").
// Each game has one or more variants the user can pick between.
type Game struct {
	Name           string                 // user-facing label, e.g. "Minecraft"
	Variants       map[string]GameVariant // variant key → config
	DefaultVariant string                 // variant to use when the request omits one
	MinPort        int
	MaxPort        int
}

// GameVariant binds a Pterodactyl egg to a specific configuration. We model
// variants explicitly because a single game may support different stacks
// (Paper vs Forge for Minecraft, vanilla vs Oxide vs Carbon for Rust) and
// each one has a distinct egg + env-var contract on the panel.
type GameVariant struct {
	Name        string
	EggID       int
	NestID      int
	DockerImage string
	Startup     string

	// TuningHook allows variants to customize the Pterodactyl Startup string
	// dynamically based on the plan (e.g. injecting Aikar's Flags for large
	// RAM allocations).
	TuningHook func(v GameVariant, ramMB int) string

	// Environment is the static base env. ConfigureEnv layers per-server
	// values (version selection, RCON password, port-derived vars) on top
	// at provision time.
	Environment map[string]string

	// VersionField is the env-var name that should receive the user-chosen
	// version string ("MINECRAFT_VERSION" for Paper, "MC_VERSION" for
	// Forge). Empty means version is not user-selectable for this variant
	// (e.g. Rust — there's no MC-style version dropdown).
	VersionField string

	// VersionDefault is what we send when the user doesn't supply one.
	VersionDefault string

	// ConfigureEnv runs at provision time after the version overlay. It
	// receives a writable copy of the env so callers can inject per-server
	// secrets (RCON passwords) or port-derived defaults (RCON_PORT etc.).
	// May return an error if secret generation fails — provisioning will
	// roll back and the webhook will retry.
	ConfigureEnv func(env map[string]string, version string, allocPort int) error
}

// Configured reports whether this variant has the minimum fields filled in
// to actually create a server. Stub variants with EggID=0 fail this check.
func (v GameVariant) Configured() bool {
	return v.EggID != 0 && v.NestID != 0 && v.DockerImage != "" && v.Startup != ""
}

// Configured reports whether the game has at least one fully-configured
// variant. Used by the checkout endpoint to reject games that are merely
// stubbed (FiveM, until you import an egg).
func (g Game) Configured() bool {
	for _, v := range g.Variants {
		if v.Configured() {
			return true
		}
	}
	return false
}

// ResolveVariant returns the variant for the given game/variant key pair.
// Empty variant key means "use the game's default". The returned bool is
// false when the game or variant is unknown.
func ResolveVariant(game, variant string) (GameVariant, bool) {
	g, ok := Games[game]
	if !ok {
		return GameVariant{}, false
	}
	if variant == "" {
		variant = g.DefaultVariant
	}
	v, ok := g.Variants[variant]
	return v, ok
}

// BuildEnvironment produces the final env map for a provision request:
// base env + version overlay + ConfigureEnv hook. The result is safe to
// pass directly to Pterodactyl. Returns the resolved version (which may
// differ from the input — e.g. blank → VersionDefault).
func BuildEnvironment(v GameVariant, version string, allocPort int) (map[string]string, string, error) {
	env := make(map[string]string, len(v.Environment))
	for k, val := range v.Environment {
		env[k] = val
	}
	if v.VersionField != "" {
		if version == "" {
			version = v.VersionDefault
		}
		env[v.VersionField] = version
	}
	if v.ConfigureEnv != nil {
		if err := v.ConfigureEnv(env, version, allocPort); err != nil {
			return nil, "", err
		}
	}
	return env, version, nil
}

// minecraftTuning injects Aikar's Flags (highly optimized GC) for servers
// with 16GB (16384MB) of RAM or more. Smaller servers get the standard
// optimized JVM flags.
func minecraftTuning(v GameVariant, ramMB int) string {
	if ramMB < 16384 {
		return v.Startup
	}

	// Aikar's Flags for Paper/Forge (GC optimization for high memory)
	aikarsFlags := "-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 -XX:+UnlockExperimentalVMOptions -XX:+DisableExplicitGC -XX:+AlwaysPreTouch -XX:G1NewSizePercent=30 -XX:G1MaxNewSizePercent=40 -XX:G1HeapRegionSize=8M -XX:G1ReservePercent=20 -XX:G1HeapWastePercent=5 -XX:G1MixedGCCountTarget=4 -XX:InitiatingHeapOccupancyPercent=15 -XX:G1MixedGCLiveThresholdPercent=90 -XX:G1RSetUpdatingPauseTimePercent=5 -XX:SurvivorRatio=32 -XX:+PerfDisableSharedMem -XX:MaxTenuringThreshold=1"

	if v.Name == "Paper" {
		return "java -Xms128M -Xmx{{SERVER_MEMORY}}M " + aikarsFlags + " -Dterminal.jline=false -Dterminal.ansi=true -jar {{SERVER_JARFILE}}"
	}
	// Forge/Generic
	return "java -Xms128M -Xmx{{SERVER_MEMORY}}M " + aikarsFlags + " -jar {{SERVER_JARFILE}}"
}

// rconPassword returns a 16-byte hex string suitable for RCON_PASS. We use
// crypto/rand because RCON gives the holder full server control — anything
// less than CSPRNG would be a foot-gun.
func rconPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rcon password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// rustEnvOverlay generates per-server values for Rust: a random RCON_PASS
// and port-offset env vars (QUERY/RCON/APP_PORT) derived from the main
// game allocation. These extra ports won't be reachable from outside the
// host's firewall unless the operator adds additional allocations through
// the panel — fine for an MVP since most users administer Rust via the
// in-game RCON, not external tools.
func rustEnvOverlay(env map[string]string, _ string, allocPort int) error {
	pass, err := rconPassword()
	if err != nil {
		return err
	}
	env["RCON_PASS"] = pass
	env["QUERY_PORT"] = strconv.Itoa(allocPort + 1)
	env["RCON_PORT"] = strconv.Itoa(allocPort + 2)
	env["APP_PORT"] = strconv.Itoa(allocPort + 3)
	return nil
}

var Games = map[string]Game{
	// Nest 1 / Egg 4 is Paper, Egg 2 is Forge on panel.kuu.gg. We never
	// expose Vanilla (egg 5) or Bungeecord (egg 1). Verify egg IDs with:
	//   GET /api/application/nests/1/eggs
	"minecraft": {
		Name:           "Minecraft",
		DefaultVariant: "paper",
		MinPort:        25565,
		MaxPort:        25700,
		Variants: map[string]GameVariant{
			"paper": {
				Name:        "Paper",
				EggID:       4,
				NestID:      1,
				DockerImage: "ghcr.io/pterodactyl/yolks:java_21",
				Startup:     "java -Xms128M -Xmx{{SERVER_MEMORY}}M -Dterminal.jline=false -Dterminal.ansi=true -jar {{SERVER_JARFILE}}",
				TuningHook:  minecraftTuning,
				Environment: map[string]string{
					"SERVER_JARFILE": "server.jar",
					"BUILD_NUMBER":   "latest",
				},
				VersionField:   "MINECRAFT_VERSION",
				VersionDefault: "latest",
			},
			"forge": {
				Name:        "Forge",
				EggID:       2,
				NestID:      1,
				DockerImage: "ghcr.io/pterodactyl/yolks:java_21",
				Startup:     "java -Xms128M -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}}",
				TuningHook:  minecraftTuning,
				Environment: map[string]string{
					"SERVER_JARFILE": "server.jar",
					"BUILD_TYPE":     "recommended",
				},
				VersionField:   "MC_VERSION",
				VersionDefault: "latest",
			},
		},
	},

	// Nest 4 / Egg 14 is Rust on panel.kuu.gg. The FRAMEWORK env switches
	// between vanilla and modded stacks; the egg's installer pulls the
	// matching binaries.
	"rust": {
		Name:           "Rust",
		DefaultVariant: "vanilla",
		MinPort:        28015,
		MaxPort:        28200,
		Variants: map[string]GameVariant{
			"vanilla": rustVariant("Vanilla", "vanilla"),
			"oxide":   rustVariant("Oxide", "oxide"),
			"carbon":  rustVariant("Carbon", "carbon"),
		},
	},

	// Nest 5 / Egg 15 is FiveM (parkervcp/eggs imported into its own
	// "GTAV" nest). The egg's startup uses {{server.build.default.port}}
	// from Pterodactyl's allocation, so no port-offset overlay is
	// needed. STEAM_WEBAPIKEY is intentionally the literal string "none"
	// — the egg accepts that as "auth disabled" and the schema rejects
	// empty strings. FIVEM_LICENSE is per-customer and must be replaced
	// in the panel before first start; we ship a placeholder so panel
	// validation accepts the create call.
	"fivem": {
		Name:           "FiveM",
		DefaultVariant: "vanilla",
		MinPort:        30120,
		MaxPort:        30200,
		Variants: map[string]GameVariant{
			"vanilla": {
				Name:        "FiveM",
				EggID:       15,
				NestID:      5,
				DockerImage: "ghcr.io/ptero-eggs/yolks:debian",
				Startup:     `$(pwd)/alpine/opt/cfx-server/ld-musl-x86_64.so.1 --library-path "$(pwd)/alpine/usr/lib/v8/:$(pwd)/alpine/lib/:$(pwd)/alpine/usr/lib/" -- $(pwd)/alpine/opt/cfx-server/FXServer +set citizen_dir $(pwd)/alpine/opt/cfx-server/citizen/ +set sv_licenseKey {{FIVEM_LICENSE}} +set steam_webApiKey {{STEAM_WEBAPIKEY}} +set sv_maxplayers {{MAX_PLAYERS}} $( [ "$TXADMIN_ENABLE" == "1" ] || printf %s '+exec server.cfg' )`,
				Environment: map[string]string{
					"FIVEM_LICENSE":    "set-in-panel-before-start",
					"STEAM_WEBAPIKEY":  "none",
					"SERVER_HOSTNAME":  "Kuu FiveM Server",
					"MAX_PLAYERS":      "48",
					"TXHOST_TXA_PORT":  "40120",
					"TXADMIN_ENABLE":   "0",
					"TXHOST_GAME_NAME": "fivem",
					"GAME_TYPE":        "",
					"TXHOST_DATA_PATH": "/home/container/txData",
					"DOWNLOAD_URL":     "",
				},
				VersionField:   "FIVEM_VERSION",
				VersionDefault: "recommended",
			},
		},
	},

	// Nest 6 / Egg 16 is Garry's Mod (parkervcp/eggs imported into its
	// own "Gmod" nest, distinct from the legacy nest 2 / egg 8). One
	// variant for now — gamemodes (sandbox / TTT / DarkRP / etc.) ride
	// on the GAMEMODE env which the customer can change in the panel.
	// Promote any of those to a top-level variant if we see real demand.
	"gmod": {
		Name:           "Garry's Mod",
		DefaultVariant: "vanilla",
		MinPort:        27015,
		MaxPort:        27200,
		Variants: map[string]GameVariant{
			"vanilla": {
				Name:        "Garry's Mod",
				EggID:       16,
				NestID:      6,
				DockerImage: "ghcr.io/ptero-eggs/games:source",
				Startup:     `./srcds_run -game garrysmod -console -port {{SERVER_PORT}} +ip 0.0.0.0 +host_workshop_collection {{WORKSHOP_ID}} +map {{SRCDS_MAP}} +gamemode {{GAMEMODE}} -strictportbind -norestart +sv_setsteamaccount {{STEAM_TOKEN}} +maxplayers {{MAX_PLAYERS}}  -tickrate {{TICKRATE}}  $( [ "$LUA_REFRESH" == "1" ] || printf %s '-disableluarefresh' )`,
				Environment: map[string]string{
					"SRCDS_MAP":   "gm_flatgrass",
					"STEAM_TOKEN": "",
					"SRCDS_APPID": "4020",
					"WORKSHOP_ID": "",
					"GAMEMODE":    "sandbox",
					"MAX_PLAYERS": "32",
					"TICKRATE":    "22",
					"LUA_REFRESH": "0",
				},
			},
		},
	},
}

// rustVariant centralizes the Rust egg config so the three framework
// variants only differ in their FRAMEWORK env value.
func rustVariant(displayName, framework string) GameVariant {
	return GameVariant{
		Name:        displayName,
		EggID:       14,
		NestID:      4,
		DockerImage: "ghcr.io/pterodactyl/games:rust",
		Startup:     `./RustDedicated -batchmode +server.port {{SERVER_PORT}} +server.queryport {{QUERY_PORT}} +server.identity "rust" +rcon.port {{RCON_PORT}} +rcon.web true +rcon.password "{{RCON_PASS}}" +server.hostname "{{HOSTNAME}}" +server.level "{{LEVEL}}" +server.description "{{DESCRIPTION}}" +server.url "{{SERVER_URL}}" +server.headerimage "{{SERVER_IMG}}" +server.logoimage "{{SERVER_LOGO}}" +server.maxplayers {{MAX_PLAYERS}} +server.worldsize "{{WORLD_SIZE}}" +server.seed "{{WORLD_SEED}}" +server.saveinterval {{SAVEINTERVAL}} +app.port {{APP_PORT}} {{ADDITIONAL_ARGS}}`,
		Environment: map[string]string{
			"FRAMEWORK":       framework,
			"HOSTNAME":        "Kuu Rust Server",
			"LEVEL":           "Procedural Map",
			"DESCRIPTION":     "Hosted by Kuu",
			"SERVER_URL":      "https://kuu.gg",
			"WORLD_SIZE":      "3000",
			"WORLD_SEED":      "",
			"MAX_PLAYERS":     "40",
			"SERVER_IMG":      "",
			"SAVEINTERVAL":    "60",
			"ADDITIONAL_ARGS": "",
			"SERVER_LOGO":     "",
			"MAP_URL":         "",
		},
		ConfigureEnv: rustEnvOverlay,
	}
}

func LoadPlans() []Plan {
	return []Plan{
		{
			Name:          "2GB",
			RAM:           2048,
			CPU:           100,
			Disk:          20480,
			PriceID:       os.Getenv("STRIPE_PRICE_2GB"),
			BackupLimit:   1,
			DatabaseLimit: 0,
		},
		{
			Name:          "4GB",
			RAM:           4096,
			CPU:           150,
			Disk:          40960,
			PriceID:       os.Getenv("STRIPE_PRICE_4GB"),
			BackupLimit:   1,
			DatabaseLimit: 0,
		},
		{
			Name:          "8GB",
			RAM:           8192,
			CPU:           200,
			Disk:          81920,
			PriceID:       os.Getenv("STRIPE_PRICE_8GB"),
			BackupLimit:   3,
			DatabaseLimit: 1,
		},
		{
			Name:          "16GB",
			RAM:           16384,
			CPU:           300,
			Disk:          163840,
			PriceID:       os.Getenv("STRIPE_PRICE_16GB"),
			BackupLimit:   3,
			DatabaseLimit: 1,
		},
		{
			Name:          "24GB",
			RAM:           24576,
			CPU:           400,
			Disk:          245760,
			PriceID:       os.Getenv("STRIPE_PRICE_24GB"),
			BackupLimit:   7,
			DatabaseLimit: 2,
		},
		{
			Name:          "32GB",
			RAM:           32768,
			CPU:           500,
			Disk:          327680,
			PriceID:       os.Getenv("STRIPE_PRICE_32GB"),
			BackupLimit:   7,
			DatabaseLimit: 2,
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
