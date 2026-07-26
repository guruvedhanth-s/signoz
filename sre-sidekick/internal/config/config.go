// Package config loads the sidekick's own configuration file
// (`configs/sidekick.yaml`, PRD section 18) into typed Go structs.
//
// Secrets are never stored in this file and never held in the Config struct.
// The YAML only names the *environment variables* that carry the Slack bot
// token and app-level token (`bot_token_env`, `app_token_env`); the values are
// read from the process environment via BotToken() and AppToken(). That way a
// Config value can be logged or marshalled without leaking credentials.
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
	DefaultAppTokenEnv      = "SLACK_APP_TOKEN"
	DefaultSessionTTL       = "30m"
	DefaultMaxConcurrentRCA = 5
	DefaultEnvironment      = "prod"

	DefaultLLMProvider  = "openrouter"
	DefaultLLMModel     = "deepseek/deepseek-chat"
	DefaultLLMAPIKeyEnv = "OPENROUTER_API_KEY"
)

// Config is the root of `sidekick.yaml` (PRD section 18): one file covering
// the Slack adapter, the RCA reasoner's LLM provider, and the section 13.4
// presentation-rule thresholds. Earlier drafts of this project split these
// across two files named `sidekick.yaml` with disjoint schemas and separate
// loaders (`configs/sidekick.yaml` for Slack, a root `sidekick.yaml` for
// presentation); that split is gone; this is the only sidekick.yaml.
type Config struct {
	Notify  NotifyConfig  `yaml:"notify" json:"notify"`
	Storage StorageConfig `yaml:"storage" json:"storage"`
	// LLM configures the provider driving the RCA reasoner (PRD section 21
	// decision 3: OpenRouter/DeepSeek, kept swappable behind one interface).
	LLM LLMConfig `yaml:"llm" json:"llm"`
	// Presentation configures the section 13.4 rule engine deciding whether
	// a diagnosis is shown as a conclusion, ranked hypotheses, or "could not
	// determine". internal/rca only consumes this struct - converted to
	// rca.PresentationConfig at the composition root (cmd/reliability-agent)
	// - it does not load it from a file itself. Defined here rather than as
	// an alias of rca.PresentationConfig because internal/notify/slack
	// already imports this package, and internal/rca imports
	// internal/notify/slack, so config importing rca would cycle. The field
	// shapes are kept identical so the conversion is a plain type
	// conversion, not a field-by-field copy.
	Presentation PresentationConfig `yaml:"presentation" json:"presentation"`
}

type StorageConfig struct {
	Driver string `yaml:"driver" json:"driver"`
	URLEnv string `yaml:"url_env,omitempty" json:"url_env,omitempty"`
}

// PresentationConfig mirrors rca.PresentationConfig field-for-field (see
// the Presentation field's comment for why this isn't a direct type
// reference). rca.PresentationConfig(cfg.Presentation) converts between
// them - Go conversion between named struct types only requires identical
// underlying fields (tags are ignored), which this satisfies by
// construction.
type PresentationConfig struct {
	MinErrorSamples            int     `yaml:"min_error_samples" json:"min_error_samples,omitempty"`
	ConcentrationThreshold     float64 `yaml:"concentration_threshold" json:"concentration_threshold,omitempty"`
	LatencyShiftThreshold      float64 `yaml:"latency_shift_threshold" json:"latency_shift_threshold,omitempty"`
	ErrorRateDeltaThreshold    float64 `yaml:"error_rate_delta_threshold" json:"error_rate_delta_threshold,omitempty"`
	GoldenSignalsForConclusion int     `yaml:"golden_signals_for_conclusion" json:"golden_signals_for_conclusion,omitempty"`
}

// LLMConfig configures the LLM provider behind the RCA reasoner (PRD
// section 21 decision 3). The API key itself is never stored here or in
// YAML: APIKeyEnv only names the environment variable that holds it,
// mirroring how SlackConfig handles the Slack tokens.
type LLMConfig struct {
	// Provider names the LLM provider. Only "openrouter" is supported today;
	// this stays a named field (rather than being implied) so a future
	// provider is a config change, not a code change.
	Provider string `yaml:"provider" json:"provider"`
	// Model is the provider's model slug, e.g. "deepseek/deepseek-chat".
	Model string `yaml:"model" json:"model"`
	// APIKeyEnv names the environment variable holding the provider API key.
	APIKeyEnv string `yaml:"api_key_env" json:"api_key_env"`
	// BaseURL overrides the provider's OpenAI-compatible chat-completions
	// endpoint. Empty uses the provider's default.
	BaseURL string          `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Limits  LLMLimitsConfig `yaml:"limits" json:"limits"`
}

type LLMLimitsConfig struct {
	MaxTokensPerRequest int    `yaml:"max_tokens_per_request" json:"max_tokens_per_request"`
	HourlyRequests      int    `yaml:"hourly_requests" json:"hourly_requests"`
	HourlyTokens        int    `yaml:"hourly_tokens" json:"hourly_tokens"`
	DailyRequests       int    `yaml:"daily_requests" json:"daily_requests"`
	DailyTokens         int    `yaml:"daily_tokens" json:"daily_tokens"`
	PerUserRequests     int    `yaml:"per_user_requests" json:"per_user_requests"`
	PerChannelRequests  int    `yaml:"per_channel_requests" json:"per_channel_requests"`
	RateWindow          string `yaml:"rate_window" json:"rate_window"`
	FailureThreshold    int    `yaml:"failure_threshold" json:"failure_threshold"`
	Cooldown            string `yaml:"cooldown" json:"cooldown"`
}

// NotifyConfig groups every outbound/inbound chat adapter. Only Slack exists
// today; Telegram/voice adapters (PRD section 14) would be siblings here.
type NotifyConfig struct {
	Slack SlackConfig `yaml:"slack" json:"slack"`
}

// SlackConfig configures the Slack adapter (Track D).
type SlackConfig struct {
	// BotTokenEnv names the env var holding the Slack bot token (xoxb-...),
	// used for outbound calls such as chat.postMessage. The token itself is
	// never read from YAML.
	BotTokenEnv string `yaml:"bot_token_env" json:"bot_token_env"`
	// AppTokenEnv names the env var holding the Slack app-level token
	// (xapp-...), used to open the Socket Mode connection.
	//
	// Socket Mode is how this adapter receives events: the sidekick dials out
	// to Slack over a WebSocket, so there is no public HTTP endpoint to
	// expose, authenticate or attack. That is also why there is no signing
	// secret here - request signatures exist to authenticate an inbound
	// endpoint, and there isn't one.
	AppTokenEnv string `yaml:"app_token_env" json:"app_token_env"`
	// DefaultChannel is the on-call channel diagnoses are posted to. Sessions
	// default to a channel thread, not a DM, so the whole team shares context
	// (session design edge case E14).
	DefaultChannel string `yaml:"default_channel" json:"default_channel"`
	// DefaultEnvironment is the environment assumed when a slash command does
	// not name one, e.g. `/diagnose support-agent`. Required because an SLO is
	// always scoped to an environment: diagnosing the wrong one would report
	// facts about the wrong system.
	DefaultEnvironment string `yaml:"default_environment" json:"default_environment"`
	// SessionTTL is how long a session may sit idle before the reaper closes
	// it (session design edge case E4). Go duration string, e.g. "30m".
	SessionTTL string `yaml:"session_ttl" json:"session_ttl"`
	// MaxConcurrentRCA caps how many diagnose runs may be in flight at once so
	// an alert storm cannot launch unbounded paid LLM/MCP work (session design
	// edge case E8).
	MaxConcurrentRCA int                 `yaml:"max_concurrent_rca" json:"max_concurrent_rca"`
	SessionStorePath string              `yaml:"session_store_path" json:"session_store_path"`
	AuditLogPath     string              `yaml:"audit_log_path" json:"audit_log_path"`
	Authorization    AuthorizationConfig `yaml:"authorization" json:"authorization"`
	EnableMutations  bool                `yaml:"enable_mutations" json:"enable_mutations"`
}

// AuthorizationConfig defines the Slack users and user groups allowed to
// make incident decisions. Empty rosters preserve the advisory MVP's
// permissive behavior; once either roster is configured, users must match a
// role to decide.
type AuthorizationConfig struct {
	Approvers AuthorizationRoleConfig `yaml:"approvers" json:"approvers"`
	Operators AuthorizationRoleConfig `yaml:"operators" json:"operators"`
}

type AuthorizationRoleConfig struct {
	Users  []string `yaml:"users" json:"users"`
	Groups []string `yaml:"groups" json:"groups"`
}

// Load reads, parses, defaults, and fully validates the config file at
// path, including the Slack adapter's credentials. This is what `watch`
// uses: it needs Slack.
//
// Unknown YAML fields are rejected so a typo in a key is a loud error
// rather than a silently ignored setting.
func Load(path string) (Config, error) {
	return load(path, true)
}

// LoadForRCA reads, parses, defaults, and validates the config file at
// path, but does not require the Slack adapter's credentials to be
// configured or present. This is what `diagnose` uses: it is a one-shot
// debug CLI that runs the RCA pipeline standalone and never touches Slack,
// so requiring SLACK_BOT_TOKEN/SLACK_APP_TOKEN just to read the LLM and
// presentation settings out of the same file would be a needless coupling.
func LoadForRCA(path string) (Config, error) {
	return load(path, false)
}

func load(path string, requireSlack bool) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read sidekick config %q: %w", path, err)
	}
	cfg, err := parse(data, requireSlack)
	if err != nil {
		return Config{}, fmt.Errorf("sidekick config %q: %w", path, err)
	}
	return cfg, nil
}

// Parse is Load without the file read, so callers (and tests) can supply
// YAML bytes directly. It always validates Slack; use ParseForRCA to skip
// that the same way LoadForRCA does.
func Parse(data []byte) (Config, error) {
	return parse(data, true)
}

// ParseForRCA is LoadForRCA without the file read.
func ParseForRCA(data []byte) (Config, error) {
	return parse(data, false)
}

func parse(data []byte, requireSlack bool) (Config, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(requireSlack); err != nil {
		return Config{}, fmt.Errorf("validate: %w", err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	slack := &c.Notify.Slack
	if strings.TrimSpace(slack.BotTokenEnv) == "" {
		slack.BotTokenEnv = DefaultBotTokenEnv
	}
	if strings.TrimSpace(slack.AppTokenEnv) == "" {
		slack.AppTokenEnv = DefaultAppTokenEnv
	}
	if strings.TrimSpace(slack.SessionTTL) == "" {
		slack.SessionTTL = DefaultSessionTTL
	}
	if strings.TrimSpace(slack.DefaultEnvironment) == "" {
		slack.DefaultEnvironment = DefaultEnvironment
	}
	if slack.MaxConcurrentRCA == 0 {
		slack.MaxConcurrentRCA = DefaultMaxConcurrentRCA
	}

	llm := &c.LLM
	if strings.TrimSpace(llm.Provider) == "" {
		llm.Provider = DefaultLLMProvider
	}
	if strings.TrimSpace(llm.Model) == "" {
		llm.Model = DefaultLLMModel
	}
	if strings.TrimSpace(llm.APIKeyEnv) == "" {
		llm.APIKeyEnv = DefaultLLMAPIKeyEnv
	}

	// Presentation needs no defaulting here: rca.Decide/Present already call
	// rca.PresentationConfig.WithDefaults() internally on every use, so an
	// omitted (zero-valued) presentation: section in the file behaves
	// exactly like DefaultPresentationConfig() without this package needing
	// to know those defaults itself.
}

// Validate fully validates the config, including Slack credentials. Kept
// for callers that already have a Config value (e.g. from Parse) and want
// the same strictness Load applies.
func (c Config) Validate() error {
	return c.validate(true)
}

// validate checks the config, including that the named credential
// environment variables are actually populated. Credentials are resolved
// eagerly so a misconfigured deployment fails at startup rather than at the
// first Slack or LLM call, in the middle of an incident. requireSlack skips
// the Slack section's own validation (and its credential lookups) for
// callers that do not use it - see LoadForRCA.
func (c Config) validate(requireSlack bool) error {
	switch driver := strings.ToLower(strings.TrimSpace(c.Storage.Driver)); driver {
	case "", "memory":
	case "file":
	case "postgres":
		if strings.TrimSpace(c.Storage.URLEnv) == "" {
			return fmt.Errorf("storage: url_env is required for postgres")
		}
		if strings.TrimSpace(os.Getenv(c.Storage.URLEnv)) == "" {
			return fmt.Errorf("storage: environment variable %s is required for postgres", c.Storage.URLEnv)
		}
	default:
		return fmt.Errorf("storage: unsupported driver %q (want memory, file, or postgres)", driver)
	}
	if requireSlack {
		if err := c.Notify.Slack.Validate(); err != nil {
			return fmt.Errorf("notify.slack: %w", err)
		}
	}
	if err := c.LLM.Validate(); err != nil {
		return fmt.Errorf("llm: %w", err)
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
	if strings.ContainsAny(s.DefaultEnvironment, " \t\n") {
		return fmt.Errorf("default_environment %q must not contain whitespace", s.DefaultEnvironment)
	}
	if err := validateEnvName("bot_token_env", s.BotTokenEnv); err != nil {
		return err
	}
	if err := validateEnvName("app_token_env", s.AppTokenEnv); err != nil {
		return err
	}
	if _, err := s.SessionTTLDuration(); err != nil {
		return err
	}
	if s.MaxConcurrentRCA < 1 {
		return fmt.Errorf("max_concurrent_rca must be >= 1, got %d", s.MaxConcurrentRCA)
	}
	if err := validateAuthorization(s.Authorization); err != nil {
		return err
	}
	if _, err := s.BotToken(); err != nil {
		return err
	}
	if _, err := s.AppToken(); err != nil {
		return err
	}
	return nil
}

func validateAuthorization(a AuthorizationConfig) error {
	seen := make(map[string]struct{})
	for role, roster := range map[string]AuthorizationRoleConfig{
		"approvers": a.Approvers,
		"operators": a.Operators,
	} {
		for _, user := range roster.Users {
			user = strings.TrimSpace(user)
			if user == "" {
				return fmt.Errorf("authorization.%s.users must not contain empty values", role)
			}
			key := role + ":user:" + user
			if _, ok := seen[key]; ok {
				return fmt.Errorf("authorization.%s.users contains duplicate user %q", role, user)
			}
			seen[key] = struct{}{}
		}
		for _, group := range roster.Groups {
			group = strings.TrimSpace(strings.TrimPrefix(group, "@"))
			if group == "" {
				return fmt.Errorf("authorization.%s.groups must not contain empty values", role)
			}
			key := role + ":group:" + group
			if _, ok := seen[key]; ok {
				return fmt.Errorf("authorization.%s.groups contains duplicate group %q", role, group)
			}
			seen[key] = struct{}{}
		}
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
	token, err := lookupSecret(s.BotTokenEnv)
	if err != nil {
		return "", err
	}
	// Pasting the app-level token into the bot token slot is the most common
	// setup mistake, and Slack's error for it is unhelpful.
	if err := requirePrefix(s.BotTokenEnv, token, "xoxb-", "bot token"); err != nil {
		return "", err
	}
	return token, nil
}

// AppToken reads the Slack app-level token used to open the Socket Mode
// connection. The error names the variable, never the value.
func (s SlackConfig) AppToken() (string, error) {
	token, err := lookupSecret(s.AppTokenEnv)
	if err != nil {
		return "", err
	}
	if err := requirePrefix(s.AppTokenEnv, token, "xapp-", "app-level token"); err != nil {
		return "", err
	}
	return token, nil
}

// Validate checks the LLM provider settings and requires the API key
// environment variable to be set. Only the presence of each value is
// checked; the key itself is not retained on the struct.
func (l LLMConfig) Validate() error {
	if strings.TrimSpace(l.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(l.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if err := validateEnvName("api_key_env", l.APIKeyEnv); err != nil {
		return err
	}
	if _, err := l.APIKey(); err != nil {
		return err
	}
	return nil
}

// APIKey reads the LLM provider API key from the environment variable
// named by APIKeyEnv. The error names the variable, never the value.
func (l LLMConfig) APIKey() (string, error) {
	return lookupSecret(l.APIKeyEnv)
}

func lookupSecret(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return value, nil
}

// requirePrefix checks a token's type without ever putting its value in the
// error message.
func requirePrefix(envName, token, prefix, kind string) error {
	if strings.HasPrefix(token, prefix) {
		return nil
	}
	return fmt.Errorf(
		"environment variable %s does not hold a Slack %s: expected a value starting with %q",
		envName, kind, prefix,
	)
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
