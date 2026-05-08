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
	mc, ok := Games["minecraft"]
	if !ok {
		t.Fatalf("minecraft game missing from Games map")
	}
	if !mc.Configured() {
		t.Fatalf("minecraft should be Configured(), got false")
	}
	rust, ok := Games["rust"]
	if !ok {
		t.Fatalf("rust game missing from Games map")
	}
	if rust.Configured() {
		t.Fatalf("rust is a stub (no egg/nest), Configured() must return false")
	}
}
