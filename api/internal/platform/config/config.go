// Package config reads the service's settings from environment variables.
package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL string
	PricingFile string
	Model       string
	Provider    string

	DataDir     string
	HostDataDir string

	AgentURL      string
	InternalToken string
	EvalToken     string

	PublicAddr   string
	InternalAddr string

	MaxUploadBytes     int64
	DailyChatBudgetUSD float64
	MaxSteps           int
	MaxTokens          int

	SandboxImage   string
	SandboxRuntime string
	MaxSandboxes   int
	SandboxTimeout time.Duration
	SandboxMemory  int64 // bytes

	TracingEndpoint     string // full OTLP/HTTP URL; empty turns tracing off
	TracingHidePayloads bool   // keep prompts, code and outputs off spans
}

// Load reads the environment. Missing required variables end the process.
func Load() Config {
	dataDir := String("DATA_DIR", "/data")
	return Config{
		DatabaseURL: must("DATABASE_URL"),
		PricingFile: String("PRICING_FILE", "/config/pricing.yaml"),
		Model:       must("LLM_MODEL"),
		Provider:    String("LLM_PROVIDER", "anthropic"),

		DataDir:     dataDir,
		HostDataDir: String("HOST_DATA_DIR", dataDir),

		AgentURL:      String("AGENT_URL", "http://agent:8000"),
		InternalToken: must("INTERNAL_TOKEN"),
		EvalToken:     os.Getenv("EVAL_TOKEN"),

		PublicAddr:   String("PUBLIC_ADDR", ":8080"),
		InternalAddr: String("INTERNAL_ADDR", ":8081"),

		MaxUploadBytes:     int64(Int("MAX_UPLOAD_MB", 25)) << 20,
		DailyChatBudgetUSD: Float("DAILY_CHAT_BUDGET_USD", 0.50),
		MaxSteps:           Int("MAX_STEPS", 10),
		MaxTokens:          Int("MAX_TOKENS", 4096),

		SandboxImage:   String("SANDBOX_IMAGE", "daa-sandbox:latest"),
		SandboxRuntime: String("SANDBOX_RUNTIME", "runc"),
		MaxSandboxes:   Int("MAX_SANDBOXES", 3),
		SandboxTimeout: time.Duration(Int("SANDBOX_TIMEOUT_SECONDS", 30)) * time.Second,
		SandboxMemory:  int64(Int("SANDBOX_MEMORY_MB", 1024)) << 20,

		TracingEndpoint:     os.Getenv("TRACING_OTLP_ENDPOINT"),
		TracingHidePayloads: Bool("TRACING_HIDE_PAYLOADS"),
	}
}

func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func must(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required environment variable", "key", key)
		os.Exit(1)
	}
	return v
}

func Int(key string, def int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return def
	}
	return n
}

func Float(key string, def float64) float64 {
	f, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil {
		return def
	}
	return f
}

// Bool is true for "1", "true", "yes" or "on" (any case).
func Bool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
