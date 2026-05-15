package plans

import (
	"testing"
)

func TestFindByPriceID(t *testing.T) {
	t.Setenv("STRIPE_PRICE_2GB", "price_2gb_xxx")
	t.Setenv("STRIPE_PRICE_4GB", "price_4gb_xxx")
	t.Setenv("STRIPE_PRICE_8GB", "price_8gb_xxx")
	t.Setenv("STRIPE_PRICE_16GB", "price_16gb_xxx")
	t.Setenv("STRIPE_PRICE_24GB", "price_24gb_xxx")
	t.Setenv("STRIPE_PRICE_32GB", "price_32gb_xxx")

	cases := []struct {
		name     string
		priceID  string
		wantName string
		wantNil  bool
	}{
		{"empty price id returns nil", "", "", true},
		{"unknown price id returns nil", "price_unknown", "", true},
		{"matches 2GB plan", "price_2gb_xxx", "2GB", false},
		{"matches 32GB plan", "price_32gb_xxx", "32GB", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FindByPriceID(tc.priceID)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected plan %q, got nil", tc.wantName)
			}
			if got.Name != tc.wantName {
				t.Fatalf("expected plan %q, got %q", tc.wantName, got.Name)
			}
		})
	}
}

func TestFindByPriceIDIgnoresEmptyPlanIDs(t *testing.T) {
	// Only configure two plans; the others have empty PriceID and must not
	// match a query of "" — that would let an unconfigured price ID
	// silently match the first unconfigured plan.
	t.Setenv("STRIPE_PRICE_2GB", "price_2gb")
	t.Setenv("STRIPE_PRICE_4GB", "")
	t.Setenv("STRIPE_PRICE_8GB", "")
	t.Setenv("STRIPE_PRICE_16GB", "")
	t.Setenv("STRIPE_PRICE_24GB", "")
	t.Setenv("STRIPE_PRICE_32GB", "")

	if got := FindByPriceID(""); got != nil {
		t.Fatalf("FindByPriceID(\"\") must return nil even when plans have empty PriceID, got %+v", got)
	}
}

func TestValidateConfigured(t *testing.T) {
	t.Setenv("STRIPE_PRICE_2GB", "p1")
	t.Setenv("STRIPE_PRICE_4GB", "p2")
	t.Setenv("STRIPE_PRICE_8GB", "p3")
	t.Setenv("STRIPE_PRICE_16GB", "p4")
	t.Setenv("STRIPE_PRICE_24GB", "p5")
	t.Setenv("STRIPE_PRICE_32GB", "p6")
	if err := ValidateConfigured(); err != nil {
		t.Fatalf("expected nil with all plans configured, got %v", err)
	}

	t.Setenv("STRIPE_PRICE_8GB", "")
	if err := ValidateConfigured(); err == nil {
		t.Fatalf("expected error when STRIPE_PRICE_8GB is missing, got nil")
	}
}

func TestGameConfigured(t *testing.T) {
	for _, key := range []string{"minecraft", "rust", "fivem", "gmod"} {
		g, ok := Games[key]
		if !ok {
			t.Fatalf("%s game missing from Games map", key)
		}
		if !g.Configured() {
			t.Fatalf("%s should be Configured(), got false", key)
		}
	}
}

func TestResolveVariantDefaults(t *testing.T) {
	v, ok := ResolveVariant("minecraft", "")
	if !ok {
		t.Fatal("expected minecraft default variant to resolve")
	}
	if v.Name != "Paper" {
		t.Fatalf("expected Paper as minecraft default variant, got %q", v.Name)
	}
	v, ok = ResolveVariant("rust", "")
	if !ok {
		t.Fatal("expected rust default variant to resolve")
	}
	if v.Name != "Vanilla" {
		t.Fatalf("expected Vanilla as rust default variant, got %q", v.Name)
	}
}

func TestResolveVariantExplicit(t *testing.T) {
	cases := []struct {
		game, variant, want string
	}{
		{"minecraft", "paper", "Paper"},
		{"minecraft", "forge", "Forge"},
		{"rust", "vanilla", "Vanilla"},
		{"rust", "oxide", "Oxide"},
		{"rust", "carbon", "Carbon"},
		{"fivem", "vanilla", "FiveM"},
		{"gmod", "vanilla", "Garry's Mod"},
	}
	for _, tc := range cases {
		t.Run(tc.game+"/"+tc.variant, func(t *testing.T) {
			v, ok := ResolveVariant(tc.game, tc.variant)
			if !ok {
				t.Fatalf("expected variant to resolve")
			}
			if v.Name != tc.want {
				t.Fatalf("got %q, want %q", v.Name, tc.want)
			}
		})
	}
}

func TestResolveVariantUnknown(t *testing.T) {
	if _, ok := ResolveVariant("doom", ""); ok {
		t.Fatal("expected unknown game to fail to resolve")
	}
	if _, ok := ResolveVariant("minecraft", "vanilla"); ok {
		t.Fatal("vanilla was intentionally excluded; it must not resolve")
	}
}

func TestBuildEnvironmentMinecraftVersion(t *testing.T) {
	v, _ := ResolveVariant("minecraft", "paper")
	env, version, err := BuildEnvironment(v, "1.21.1", 25565)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "1.21.1" {
		t.Fatalf("expected 1.21.1, got %q", version)
	}
	if env["MINECRAFT_VERSION"] != "1.21.1" {
		t.Fatalf("MINECRAFT_VERSION not set: %v", env)
	}
	if env["SERVER_JARFILE"] != "server.jar" {
		t.Fatalf("base env not preserved: %v", env)
	}
}

func TestBuildEnvironmentMinecraftDefaultVersion(t *testing.T) {
	v, _ := ResolveVariant("minecraft", "paper")
	_, version, err := BuildEnvironment(v, "", 25565)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != v.VersionDefault {
		t.Fatalf("expected default %q, got %q", v.VersionDefault, version)
	}
}

func TestBuildEnvironmentMinecraftBadVersionFallsBackToDefault(t *testing.T) {
	v, _ := ResolveVariant("minecraft", "paper")
	for _, bad := range []string{"latest", "abcdef", "1.x.y", "99.99.99", "2.0.0"} {
		_, version, err := BuildEnvironment(v, bad, 25565)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", bad, err)
		}
		if version != v.VersionDefault {
			t.Fatalf("input %q: expected fallback to %q, got %q", bad, v.VersionDefault, version)
		}
	}
}

func TestBuildEnvironmentMinecraftValidVersionPassesThrough(t *testing.T) {
	v, _ := ResolveVariant("minecraft", "paper")
	for _, good := range []string{"1.8.8", "1.20", "1.21.4"} {
		_, version, err := BuildEnvironment(v, good, 25565)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", good, err)
		}
		if version != good {
			t.Fatalf("input %q: expected pass-through, got %q", good, version)
		}
	}
}

func TestBuildEnvironmentRustGeneratesRconAndPorts(t *testing.T) {
	v, _ := ResolveVariant("rust", "oxide")
	env, _, err := BuildEnvironment(v, "", 28015)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env["FRAMEWORK"] != "oxide" {
		t.Fatalf("FRAMEWORK should be oxide, got %q", env["FRAMEWORK"])
	}
	if got := env["RCON_PASS"]; len(got) != 32 {
		t.Fatalf("RCON_PASS should be 32-char hex, got %q (len=%d)", got, len(got))
	}
	if env["QUERY_PORT"] != "28016" || env["RCON_PORT"] != "28017" || env["APP_PORT"] != "28018" {
		t.Fatalf("port-derived env wrong: %v", env)
	}
}

func TestBuildEnvironmentFiveMVersion(t *testing.T) {
	v, _ := ResolveVariant("fivem", "vanilla")
	env, version, err := BuildEnvironment(v, "6013-d8ae399d", 30120)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "6013-d8ae399d" {
		t.Fatalf("expected explicit version, got %q", version)
	}
	if env["FIVEM_VERSION"] != "6013-d8ae399d" {
		t.Fatalf("FIVEM_VERSION not set: %v", env)
	}
	if env["STEAM_WEBAPIKEY"] != "none" {
		t.Fatalf("STEAM_WEBAPIKEY must default to 'none' (egg requires non-empty), got %q", env["STEAM_WEBAPIKEY"])
	}
	if env["FIVEM_LICENSE"] == "" {
		t.Fatalf("FIVEM_LICENSE must be a non-empty placeholder so panel validation accepts the create")
	}
}

func TestBuildEnvironmentGmodDefaults(t *testing.T) {
	v, _ := ResolveVariant("gmod", "")
	env, version, err := BuildEnvironment(v, "", 27015)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "" {
		t.Fatalf("gmod has no VersionField; expected empty version, got %q", version)
	}
	if env["GAMEMODE"] != "sandbox" || env["SRCDS_MAP"] != "gm_flatgrass" {
		t.Fatalf("gmod default env wrong: %v", env)
	}
	if env["SRCDS_APPID"] != "4020" {
		t.Fatalf("SRCDS_APPID must stay 4020 (egg requires it): %v", env)
	}
}

func TestBuildEnvironmentDoesNotMutateBase(t *testing.T) {
	// BuildEnvironment must copy the variant's Environment map, not return
	// it directly — otherwise per-server values like RCON_PASS would leak
	// across provisions.
	v, _ := ResolveVariant("rust", "vanilla")
	env1, _, err := BuildEnvironment(v, "", 28015)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env2, _, err := BuildEnvironment(v, "", 28015)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1["RCON_PASS"] == env2["RCON_PASS"] {
		t.Fatalf("RCON_PASS must be regenerated per call, got identical %q", env1["RCON_PASS"])
	}
	if _, baseLeak := v.Environment["RCON_PASS"]; baseLeak {
		t.Fatalf("base Environment was mutated; RCON_PASS leaked into shared map")
	}
}
