package cost_test

// Seam 4: cost.Pricing.Cost and cost.Budget.Allow.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/cost"
)

const pricingYAML = `
models:
  test-model:
    input_per_mtok: 3.00
    output_per_mtok: 15.00
    cache_read_per_mtok: 0.30
    cache_write_per_mtok: 3.75
`

func loadPricing(t *testing.T) cost.Pricing {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(path, []byte(pricingYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := cost.LoadPricing(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCostCountsEachTokenKindAtItsOwnPrice(t *testing.T) {
	p := loadPricing(t)
	got, err := p.Cost(contract.Usage{
		Model:            "test-model",
		InputTokens:      100_000,   // $0.30
		OutputTokens:     10_000,    // $0.15
		CachedTokens:     1_000_000, // $0.30
		CacheWriteTokens: 200_000,   // $0.75
	})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-1.50) > 1e-9 {
		t.Fatalf("cost = %v, want 1.50", got)
	}
}

func TestUnknownModelIsAnError(t *testing.T) {
	if _, err := loadPricing(t).Cost(contract.Usage{Model: "mystery"}); err == nil {
		t.Fatal("expected error for a model missing from pricing.yaml")
	}
}

type fakeSpend struct {
	spent float64
	since time.Time
}

func (f *fakeSpend) SpentSince(_ context.Context, since time.Time) (float64, error) {
	f.since = since
	return f.spent, nil
}

func TestBudgetCountsSpendSinceUTCMidnight(t *testing.T) {
	store := &fakeSpend{spent: 0.10}
	loc := time.FixedZone("IST", 5*3600+1800)
	b := cost.Budget{Limit: 0.50, Store: store,
		Now: func() time.Time { return time.Date(2026, 11, 20, 3, 0, 0, 0, loc) }}
	if _, err := b.Allow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 03:00 IST on the 20th is 21:30 UTC on the 19th.
	want := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	if !store.since.Equal(want) {
		t.Fatalf("since = %v, want %v", store.since, want)
	}
}

func TestBudgetAllowsUnderTheLimitAndRefusesAtIt(t *testing.T) {
	for _, tc := range []struct {
		spent float64
		allow bool
	}{{0.0, true}, {0.49, true}, {0.50, false}, {0.73, false}} {
		b := cost.Budget{Limit: 0.50, Store: &fakeSpend{spent: tc.spent}, Now: time.Now}
		got, err := b.Allow(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.allow {
			t.Errorf("spent %.2f: allow = %v, want %v", tc.spent, got, tc.allow)
		}
	}
}
