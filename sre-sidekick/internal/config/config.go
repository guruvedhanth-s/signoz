// Package config loads the sidekick's own configuration file
// (`configs/sidekick.yaml`, PRD section 18) into typed Go structs.
//
// Secrets are never stored in this file and never held in the Config struct.
// The YAML only names the *environment variables* that carry the Slack bot
// token and signing secret (`bot_token_env`, `signing_secret_env`); the values
// are read from the process environment via BotToken() and SigningSecret().
// That way a Config value can be logged or marshalled without leaking
// credentials.
//
// Validation is strict about credentials: loading fails when a named
// environment variable is missing, so a misconfigured deployment is caught at
// startup rather than at the first Slack call during an incident.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied when a field is omitted from the YAML file.
const (
	DefaultBotTokenEnv      = "SLACK_BOT_TOKEN"
	DefaultSigningSecretEnv = "SLACK_SIGNING_SECRET"
	DefaultSessionTTL       = "30m"
	DefaultMaxConcurrentRCA = 5
)

// Config is the root of `sidekick.yaml`.
type Config struct {
	Notify NotifyConfig `yaml:"notify" json:"notify"`
}

// NotifyConfig groups every outbound/inbound chat adapter. Only Slack exists
// today; Telegram/voice adapters (PRD section 14) would be siblings here.
type NotifyConfig struct {
	Slack SlackConfig `yaml:"slack" json:"slack"`
}

// SlackConfig configures the Slack adapter (Track D).
type SlackConfig struct {
	// BotTokenEnv names the env var holding the Slack bot token (xoxb-...).
	// The token itself is never read from YAML.
	BotTokenEnv string `yaml:"bot_token_env" json:"bot_token_env"`
	// SigningSecretEnv names the env var holding the Slack signing secret,
	// used to verify every inbound request (session design section 3.1:
	// signature verification is non-negotiable).
	SigningSecretEnv string `yaml:"signing_secret_env" json:"signing_secret_env"`
	// DefaultChannel is the on-call channel diagnoses are posted to. Sessions
	// default to a channel thread, not a DM, so the whole team shares context
	// (session design edge case E14).
	DefaultChannel string `yaml:"default_channel" json:"default_channel"`
	// SessionTTL is how long a session may sit idle before the reaper closes
	// it (session design edge case E4). Go duration string, e.g. "30m".
	SessionTTL string `yaml:"session_ttl" json:"session_ttl"`
	// MaxConcurrentRCA caps how many diagnose runs may be in flight at once so
	// an alert storm cannot launch unbounded paid LLM/MCP work (session design
	// edge case E8).
	MaxConcurrentRCA int `yaml:"max_concurrent_rca" json:"max_concurrent_rca"`
}

// Load reads, parses, defaults and validates the config file at path.
// Unknown YAML fields are rejected so a typo in a key is a loud error rather
// than a silently ignored setting.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read sidekick config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("sidekick config %q: %w", path, err)
	}
	return cfg, nil
}

// Parse is Load without the file read, so callers (and tests) can supply YAML
// bytes directly.
func Parse(data []byte) (Config, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate: %w", err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	slack := &c.Notify.Slack
	if strings.TrimSpace(slack.BotTokenEnv) == "" {
		slack.BotTokenEnv = DefaultBotTokenEnv
	}
	if strings.TrimSpace(slack.SigningSecretEnv) == "" {
		slack.SigningSecretEnv = DefaultSigningSecretEnv
	}
	if strings.TrimSpace(slack.SessionTTL) == "" {
		slack.SessionTTL = DefaultSessionTTL
	}
	if slack.MaxConcurrentRCA == 0 {
		slack.MaxConcurrentRCA = DefaultMaxConcurrentRCA
	}
}

// Validate checks the config, including that the named credential
// environment variables are actually populated. Credentials are resolved
// eagerly so a misconfigured deployment fails at startup rather than at the
// first Slack call, in the middle of an incident.
func (c Config) Validate() error {
	if err := c.Notify.Slack.Validate(); err != nil {
		return fmt.Errorf("notify.slack: %w", err)
	}
	return nil
}

// Validate checks the Slack adapter settings and requires the credential
// environment variables to be set. Only the presence of each value is
// checked; the value itself is not retained on the struct.
func (s SlackConfig) Validate() error {
	if strings.TrimSpace(s.DefaultChannel) == "" {
		return fmt.Errorf("default_channel is required")
	}
	if strings.ContainsAny(s.DefaultChannel, " \t\n") {
		return fmt.Errorf("default_channel %q must not contain whitespace", s.DefaultChannel)
	}
	if err := validateEnvName("bot_token_env", s.BotTokenEnv); err != nil {
		return err
	}
	if err := validateEnvName("signing_secret_env", s.SigningSecretEnv); err != nil {
		return err
	}
	if _, err := s.SessionTTLDuration(); err != nil {
		return err
	}
	if s.MaxConcurrentRCA < 1 {
		return fmt.Errorf("max_concurrent_rca must be >= 1, got %d", s.MaxConcurrentRCA)
	}
	if _, err := s.BotToken(); err != nil {
		return err
	}
	if _, err := s.SigningSecret(); err != nil {
		return err
	}
	return nil
}

// SessionTTLDuration parses SessionTTL into a duration.
func (s SlackConfig) SessionTTLDuration() (time.Duration, error) {
	raw := strings.TrimSpace(s.SessionTTL)
	if raw == "" {
		return 0, fmt.Errorf("session_ttl is required")
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid session_ttl %q: %w", raw, err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("session_ttl must be positive, got %q", raw)
	}
	return ttl, nil
}

// BotToken reads the Slack bot token from the environment variable named by
// BotTokenEnv. The error names the variable, never the value.
func (s SlackConfig) BotToken() (string, error) {
	return lookupSecret(s.BotTokenEnv)
}

// SigningSecret reads the Slack signing secret from the environment variable
// named by SigningSecretEnv. The error names the variable, never the value.
func (s SlackConfig) SigningSecret() (string, error) {
	return lookupSecret(s.SigningSecretEnv)
}

func lookupSecret(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return value, nil
}

func validateEnvName(field, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%s is required", field)
	}
	for _, r := range name {
		isUpper := r >= 'A' && r <= 'Z'
		isLower := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isUpper && !isLower && !isDigit && r != '_' {
			return fmt.Errorf("%s %q is not a valid environment variable name", field, name)
		}
	}
	return nil
}
