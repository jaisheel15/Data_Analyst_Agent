// Package cost turns token usage into dollars and enforces the daily chat cap.
package budget

import (
	"context"
	"fmt"
	"os"
	"time"

	"dataanalyst/api/internal/agent"

	"gopkg.in/yaml.v3"
)

// Price is USD per million tokens.
type Price struct {
	InputPerMTok      float64 `yaml:"input_per_mtok"`
	OutputPerMTok     float64 `yaml:"output_per_mtok"`
	CacheReadPerMTok  float64 `yaml:"cache_read_per_mtok"`
	CacheWritePerMTok float64 `yaml:"cache_write_per_mtok"`
}

// Pricing maps a model ID to its price.
type Pricing map[string]Price

func LoadPricing(path string) (Pricing, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models Pricing `yaml:"models"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Models, nil
}

func (p Pricing) Cost(u agent.Usage) (float64, error) {
	price, ok := p[u.Model]
	if !ok {
		return 0, fmt.Errorf("model %q is not in pricing.yaml", u.Model)
	}
	const m = 1_000_000.0
	return float64(u.InputTokens)/m*price.InputPerMTok +
		float64(u.OutputTokens)/m*price.OutputPerMTok +
		float64(u.CachedTokens)/m*price.CacheReadPerMTok +
		float64(u.CacheWriteTokens)/m*price.CacheWritePerMTok, nil
}

// SpendStore reports chat spend recorded since a point in time.
type SpendStore interface {
	SpentSince(ctx context.Context, since time.Time) (float64, error)
}

// Budget is the daily chat cap. The day resets at midnight UTC.
type Budget struct {
	Limit float64
	Store SpendStore
	Now   func() time.Time
}

// Allow reports whether today's spend is still under the limit.
func (b Budget) Allow(ctx context.Context) (bool, error) {
	now := b.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	spent, err := b.Store.SpentSince(ctx, midnight)
	if err != nil {
		return false, err
	}
	return spent < b.Limit, nil
}
