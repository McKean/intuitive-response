// Package config loads proxy settings. Precedence: env > ~/.config/irp/config.json > default.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                int
	Upstream            string
	TypeSafeAPIKey      string
	JevURL              string
	JevModel            string
	JevTimeout          time.Duration
	MinConfidence       float64
	FullAnswerThreshold float64
	MaxPresets          int
	MatchMode           string // "live" | "shadow"
	ReplaceMode         string // "ring" | "all"
	Continuation        string // "note" | "prefill"
	ContinuationTimeout time.Duration
	Inject              bool
	HiddenMemory        bool // restore stripped blocks into the model's view of history
	TrafficLog          bool
	StateDir            string // logs and traffic captures
}

// fileConfig mirrors config.json; pointers distinguish "unset" from zero values.
type fileConfig struct {
	Port                *int     `json:"port"`
	Upstream            *string  `json:"upstream"`
	TypeSafeAPIKey      *string  `json:"typesafe_api_key"`
	JevURL              *string  `json:"jev_url"`
	JevModel            *string  `json:"jev_model"`
	JevTimeoutMS        *int     `json:"jev_timeout_ms"`
	MinConfidence       *float64 `json:"min_confidence"`
	FullAnswerThreshold *float64 `json:"full_answer_threshold"`
	MaxPresets          *int     `json:"max_presets"`
	MatchMode           *string  `json:"match_mode"`
	ReplaceMode         *string  `json:"replace_mode"`
	Continuation        *string  `json:"continuation"`
	ContinuationTimeout *int     `json:"continuation_timeout_s"`
	Inject              *bool    `json:"inject"`
	HiddenMemory        *bool    `json:"hidden_memory"`
	TrafficLog          *bool    `json:"traffic_log"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Port:     18766,
		Upstream: "https://api.anthropic.com",
		JevURL:   "https://api.typesafe.ai",
		// Pinned rather than jev-latest: thresholds are tuned against a specific version.
		JevModel: "jev-1.13.0",
		// Measured ~250-380 ms per call from Basel. The upstream request starts in parallel
		// with matching, so a miss only costs the time Jev takes beyond upstream's first byte.
		JevTimeout:          800 * time.Millisecond,
		MinConfidence:       0.8,
		FullAnswerThreshold: 0.8,
		MaxPresets:          10,
		MatchMode:           "live",
		// Each new presets block replaces the session's presets. With "ring", presets from
		// earlier answers linger: they go stale and near-duplicates split Jev's vote.
		ReplaceMode: "all",
		// Assistant prefill returns 400 on every current Claude model, so "note" is the only
		// strategy that works today. "prefill" is kept for older models.
		Continuation:        "note",
		ContinuationTimeout: 20 * time.Second,
		Inject:              true,
		HiddenMemory:        true,
		StateDir:            filepath.Join(home, ".local", "state", "irp"),
	}
}

// Load reads the config file (if present) and environment on top of the defaults.
// A .env file in the working directory fills in variables not already set.
func Load() (Config, error) {
	c := Default()
	if err := loadDotEnv(".env"); err != nil {
		return c, err
	}
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".config", "irp", "config.json")
	if err := c.applyFile(path); err != nil {
		return c, err
	}
	if err := c.applyEnv(os.Getenv); err != nil {
		return c, err
	}
	return c, c.validate()
}

func (c *Config) applyFile(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f fileConfig
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	set(&c.Port, f.Port)
	set(&c.Upstream, f.Upstream)
	set(&c.TypeSafeAPIKey, f.TypeSafeAPIKey)
	set(&c.JevURL, f.JevURL)
	set(&c.JevModel, f.JevModel)
	if f.JevTimeoutMS != nil {
		c.JevTimeout = time.Duration(*f.JevTimeoutMS) * time.Millisecond
	}
	set(&c.MinConfidence, f.MinConfidence)
	set(&c.FullAnswerThreshold, f.FullAnswerThreshold)
	set(&c.MaxPresets, f.MaxPresets)
	set(&c.MatchMode, f.MatchMode)
	set(&c.ReplaceMode, f.ReplaceMode)
	set(&c.Continuation, f.Continuation)
	if f.ContinuationTimeout != nil {
		c.ContinuationTimeout = time.Duration(*f.ContinuationTimeout) * time.Second
	}
	set(&c.Inject, f.Inject)
	set(&c.HiddenMemory, f.HiddenMemory)
	set(&c.TrafficLog, f.TrafficLog)
	return nil
}

func (c *Config) applyEnv(getenv func(string) string) error {
	var errs []error
	str := func(key string, dst *string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	num := func(key string, dst *int) {
		if v := getenv(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				return
			}
			*dst = n
		}
	}
	flt := func(key string, dst *float64) {
		if v := getenv(key); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				return
			}
			*dst = f
		}
	}
	boolean := func(key string, dst *bool) {
		if v := getenv(key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				return
			}
			*dst = b
		}
	}

	num("IRP_PORT", &c.Port)
	str("IRP_UPSTREAM", &c.Upstream)
	str("JEV_SECRET", &c.TypeSafeAPIKey)
	str("TYPESAFE_API_KEY", &c.TypeSafeAPIKey) // takes precedence over JEV_SECRET
	str("IRP_JEV_URL", &c.JevURL)
	str("IRP_JEV_MODEL", &c.JevModel)
	jevMS := int(c.JevTimeout / time.Millisecond)
	num("IRP_JEV_TIMEOUT_MS", &jevMS)
	c.JevTimeout = time.Duration(jevMS) * time.Millisecond
	flt("IRP_MIN_CONFIDENCE", &c.MinConfidence)
	flt("IRP_FULL_ANSWER_THRESHOLD", &c.FullAnswerThreshold)
	num("IRP_MAX_PRESETS", &c.MaxPresets)
	str("IRP_MATCH_MODE", &c.MatchMode)
	str("IRP_REPLACE_MODE", &c.ReplaceMode)
	str("IRP_CONTINUATION", &c.Continuation)
	contS := int(c.ContinuationTimeout / time.Second)
	num("IRP_CONTINUATION_TIMEOUT_S", &contS)
	c.ContinuationTimeout = time.Duration(contS) * time.Second
	boolean("IRP_INJECT", &c.Inject)
	boolean("IRP_HIDDEN_MEMORY", &c.HiddenMemory)
	boolean("IRP_TRAFFIC_LOG", &c.TrafficLog)
	str("IRP_STATE_DIR", &c.StateDir)
	return errors.Join(errs...)
}

func (c *Config) validate() error {
	switch {
	case c.ReplaceMode != "ring" && c.ReplaceMode != "all":
		return fmt.Errorf("replace_mode must be ring or all, got %q", c.ReplaceMode)
	case c.Continuation != "note" && c.Continuation != "prefill":
		return fmt.Errorf("continuation must be note or prefill, got %q", c.Continuation)
	case c.MatchMode != "live" && c.MatchMode != "shadow":
		return fmt.Errorf("match_mode must be live or shadow, got %q", c.MatchMode)
	case c.MaxPresets < 1:
		return fmt.Errorf("max_presets must be >= 1")
	}
	return nil
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// loadDotEnv sets KEY=VALUE lines from path into the environment, without overriding
// variables that are already set. A missing file is not an error.
func loadDotEnv(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return nil
}
