package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
notify:
  slack:
    default_channel: "#sre-sidekick"
`

// setCredentials populates the credential environment variables the strict
// validator requires. t.Setenv restores the previous values automatically.
func setCredentials(t *testing.T) {
	t.Helper()
	t.Setenv(DefaultBotTokenEnv, "xoxb-test-token")
	t.Setenv(DefaultAppTokenEnv, "xapp-test-token")
	t.Setenv(DefaultLLMAPIKeyEnv, "test-openrouter-key")
}

func TestParseAppliesDefaults(t *testing.T) {
	setCredentials(t)

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	slack := cfg.Notify.Slack
	if slack.BotTokenEnv != DefaultBotTokenEnv {
		t.Errorf("BotTokenEnv = %q, want %q", slack.BotTokenEnv, DefaultBotTokenEnv)
	}
	if slack.AppTokenEnv != DefaultAppTokenEnv {
		t.Errorf("AppTokenEnv = %q, want %q", slack.AppTokenEnv, DefaultAppTokenEnv)
	}
	if slack.MaxConcurrentRCA != DefaultMaxConcurrentRCA {
		t.Errorf("MaxConcurrentRCA = %d, want %d", slack.MaxConcurrentRCA, DefaultMaxConcurrentRCA)
	}
	ttl, err := slack.SessionTTLDuration()
	if err != nil {
		t.Fatalf("SessionTTLDuration() error = %v", err)
	}
	if ttl != 30*time.Minute {
		t.Errorf("SessionTTL = %v, want 30m", ttl)
	}
}

func TestParseFullConfig(t *testing.T) {
	t.Setenv("CUSTOM_TOKEN", "xoxb-custom")
	t.Setenv("CUSTOM_APP_TOKEN", "xapp-custom")
	t.Setenv(DefaultLLMAPIKeyEnv, "test-openrouter-key")

	yaml := `
notify:
  slack:
    bot_token_env: CUSTOM_TOKEN
    app_token_env: CUSTOM_APP_TOKEN
    default_channel: "C0123456789"
    session_ttl: 90s
    max_concurrent_rca: 2
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	slack := cfg.Notify.Slack
	if slack.BotTokenEnv != "CUSTOM_TOKEN" {
		t.Errorf("BotTokenEnv = %q", slack.BotTokenEnv)
	}
	if slack.AppTokenEnv != "CUSTOM_APP_TOKEN" {
		t.Errorf("AppTokenEnv = %q", slack.AppTokenEnv)
	}
	if slack.DefaultChannel != "C0123456789" {
		t.Errorf("DefaultChannel = %q", slack.DefaultChannel)
	}
	if slack.MaxConcurrentRCA != 2 {
		t.Errorf("MaxConcurrentRCA = %d", slack.MaxConcurrentRCA)
	}
	ttl, err := slack.SessionTTLDuration()
	if err != nil {
		t.Fatalf("SessionTTLDuration() error = %v", err)
	}
	if ttl != 90*time.Second {
		t.Errorf("SessionTTL = %v, want 90s", ttl)
	}
}

func TestParseErrors(t *testing.T) {
	setCredentials(t)

	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing default channel",
			yaml:    "notify:\n  slack:\n    session_ttl: 30m\n",
			wantErr: "default_channel is required",
		},
		{
			name:    "channel with whitespace",
			yaml:    "notify:\n  slack:\n    default_channel: \"#sre sidekick\"\n",
			wantErr: "must not contain whitespace",
		},
		{
			name:    "unknown field",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    defualt_channel: \"#typo\"\n",
			wantErr: "field defualt_channel not found",
		},
		{
			name:    "invalid session ttl",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    session_ttl: soon\n",
			wantErr: "invalid session_ttl",
		},
		{
			name:    "negative session ttl",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    session_ttl: -5m\n",
			wantErr: "session_ttl must be positive",
		},
		{
			name:    "non positive concurrency",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    max_concurrent_rca: -1\n",
			wantErr: "max_concurrent_rca must be >= 1",
		},
		{
			name:    "invalid env var name",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    bot_token_env: \"not a var\"\n",
			wantErr: "is not a valid environment variable name",
		},
		{
			name:    "malformed yaml",
			yaml:    "notify: [oops\n",
			wantErr: "parse:",
		},
		{
			name:    "credential env var not set",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    bot_token_env: MISSING_TOKEN_VAR\n",
			wantErr: "environment variable MISSING_TOKEN_VAR is not set",
		},
		{
			name:    "app token env var not set",
			yaml:    "notify:\n  slack:\n    default_channel: \"#x\"\n    app_token_env: MISSING_APP_TOKEN_VAR\n",
			wantErr: "environment variable MISSING_APP_TOKEN_VAR is not set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse() error = nil, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Parse() error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// The strict validator must reject a config whose credentials are absent,
// even though the file itself is well formed.
func TestParseFailsWithoutCredentials(t *testing.T) {
	t.Setenv(DefaultBotTokenEnv, "")
	t.Setenv(DefaultAppTokenEnv, "xapp-test-token")

	_, err := Parse([]byte(minimalYAML))
	if err == nil {
		t.Fatal("Parse() error = nil, want missing-credential error")
	}
	if !strings.Contains(err.Error(), DefaultBotTokenEnv) {
		t.Errorf("Parse() error = %q, want it to name %s", err, DefaultBotTokenEnv)
	}

	t.Setenv(DefaultBotTokenEnv, "xoxb-test-token")
	t.Setenv(DefaultAppTokenEnv, "")
	if _, err := Parse([]byte(minimalYAML)); err == nil {
		t.Fatal("Parse() error = nil, want missing app token error")
	} else if !strings.Contains(err.Error(), DefaultAppTokenEnv) {
		t.Errorf("Parse() error = %q, want it to name %s", err, DefaultAppTokenEnv)
	}
}

// Pasting the bot token into the app token slot (or the reverse) is the most
// common setup mistake; it must be caught at load with a clear message rather
// than surfacing later as an opaque Slack connection error.
func TestTokenPrefixesAreChecked(t *testing.T) {
	t.Setenv(DefaultBotTokenEnv, "xapp-wrong-slot")
	t.Setenv(DefaultAppTokenEnv, "xapp-test-token")

	_, err := Parse([]byte(minimalYAML))
	if err == nil {
		t.Fatal("Parse() error = nil, want the swapped bot token rejected")
	}
	if !strings.Contains(err.Error(), "xoxb-") {
		t.Errorf("error = %q, want it to state the expected prefix", err)
	}
	if strings.Contains(err.Error(), "wrong-slot") {
		t.Errorf("error = %q, want it to not echo the token value", err)
	}

	t.Setenv(DefaultBotTokenEnv, "xoxb-test-token")
	t.Setenv(DefaultAppTokenEnv, "xoxb-wrong-slot")
	if _, err := Parse([]byte(minimalYAML)); err == nil {
		t.Fatal("Parse() error = nil, want the swapped app token rejected")
	} else if !strings.Contains(err.Error(), "xapp-") {
		t.Errorf("error = %q, want it to state the expected prefix", err)
	}
}

func TestLoadReadsFile(t *testing.T) {
	setCredentials(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "sidekick.yaml")
	if err := os.WriteFile(path, []byte(minimalYAML), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notify.Slack.DefaultChannel != "#sre-sidekick" {
		t.Errorf("DefaultChannel = %q", cfg.Notify.Slack.DefaultChannel)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("Load() error = nil, want a read error")
	}
	if !strings.Contains(err.Error(), "read sidekick config") {
		t.Errorf("Load() error = %q", err)
	}
}

func TestLoadShippedExampleConfig(t *testing.T) {
	setCredentials(t)

	cfg, err := Load(filepath.Join("..", "..", "configs", "sidekick.yaml"))
	if err != nil {
		t.Fatalf("Load(configs/sidekick.yaml) error = %v", err)
	}
	if cfg.Notify.Slack.DefaultChannel == "" {
		t.Error("shipped config has no default_channel")
	}
}

func TestSecretsComeFromEnvironment(t *testing.T) {
	setCredentials(t)

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	slack := cfg.Notify.Slack

	// Unset: an error that names the variable but carries no value.
	t.Setenv(DefaultBotTokenEnv, "")
	if _, err := slack.BotToken(); err == nil {
		t.Fatal("BotToken() error = nil, want missing-env error")
	} else if !strings.Contains(err.Error(), DefaultBotTokenEnv) {
		t.Errorf("BotToken() error = %q, want it to name %s", err, DefaultBotTokenEnv)
	}

	t.Setenv(DefaultBotTokenEnv, "xoxb-test-token")
	token, err := slack.BotToken()
	if err != nil {
		t.Fatalf("BotToken() error = %v", err)
	}
	if token != "xoxb-test-token" {
		t.Errorf("BotToken() = %q", token)
	}

	t.Setenv(DefaultAppTokenEnv, "  xapp-s3cr3t  ")
	appToken, err := slack.AppToken()
	if err != nil {
		t.Fatalf("AppToken() error = %v", err)
	}
	if appToken != "xapp-s3cr3t" {
		t.Errorf("AppToken() = %q, want trimmed value", appToken)
	}
}

// A Config value must be safe to log: it holds env var NAMES, never secrets.
func TestConfigNeverHoldsSecretValues(t *testing.T) {
	setCredentials(t)
	t.Setenv(DefaultBotTokenEnv, "xoxb-super-secret")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if strings.Contains(strings.ToLower(sprint(cfg)), "xoxb-super-secret") {
		t.Error("Config value contains the secret token")
	}
}

func sprint(cfg Config) string {
	var b strings.Builder
	b.WriteString(cfg.Notify.Slack.BotTokenEnv)
	b.WriteString(cfg.Notify.Slack.AppTokenEnv)
	b.WriteString(cfg.Notify.Slack.DefaultChannel)
	b.WriteString(cfg.Notify.Slack.SessionTTL)
	return b.String()
}

// diagnose (via LoadForRCA) never touches Slack, so it must not need
// SLACK_BOT_TOKEN/SLACK_APP_TOKEN just to read the LLM and presentation
// settings out of the same sidekick.yaml watch uses.
func TestParseForRCA_DoesNotRequireSlackCredentials(t *testing.T) {
	t.Setenv(DefaultLLMAPIKeyEnv, "test-openrouter-key")

	cfg, err := ParseForRCA([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("ParseForRCA() error = %v", err)
	}
	if cfg.LLM.Model != DefaultLLMModel {
		t.Errorf("LLM.Model = %q, want the default", cfg.LLM.Model)
	}
}

// ParseForRCA still requires the LLM API key: every RCA entry point needs
// the reasoner, unlike Slack which only `watch` uses.
func TestParseForRCA_StillRequiresTheLLMAPIKey(t *testing.T) {
	t.Setenv(DefaultLLMAPIKeyEnv, "")
	if _, err := ParseForRCA([]byte(minimalYAML)); err == nil {
		t.Fatal("ParseForRCA() error = nil, want the missing LLM API key reported")
	}
}

func TestParse_LLMDefaults(t *testing.T) {
	setCredentials(t)

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.LLM.Provider != DefaultLLMProvider {
		t.Errorf("LLM.Provider = %q, want %q", cfg.LLM.Provider, DefaultLLMProvider)
	}
	if cfg.LLM.Model != DefaultLLMModel {
		t.Errorf("LLM.Model = %q, want %q", cfg.LLM.Model, DefaultLLMModel)
	}
	if cfg.LLM.APIKeyEnv != DefaultLLMAPIKeyEnv {
		t.Errorf("LLM.APIKeyEnv = %q, want %q", cfg.LLM.APIKeyEnv, DefaultLLMAPIKeyEnv)
	}
	if key, err := cfg.LLM.APIKey(); err != nil || key != "test-openrouter-key" {
		t.Errorf("LLM.APIKey() = (%q, %v), want the value from the default env var", key, err)
	}
}

func TestParse_LLMOverrides(t *testing.T) {
	t.Setenv("CUSTOM_LLM_KEY", "custom-key")
	setCredentials(t)

	yaml := minimalYAML + `
llm:
  provider: openrouter
  model: some/other-model
  api_key_env: CUSTOM_LLM_KEY
  base_url: https://example.test/v1/chat/completions
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.LLM.Model != "some/other-model" {
		t.Errorf("LLM.Model = %q", cfg.LLM.Model)
	}
	if cfg.LLM.BaseURL != "https://example.test/v1/chat/completions" {
		t.Errorf("LLM.BaseURL = %q", cfg.LLM.BaseURL)
	}
	key, err := cfg.LLM.APIKey()
	if err != nil || key != "custom-key" {
		t.Errorf("LLM.APIKey() = (%q, %v), want the overridden env var's value", key, err)
	}
}

// A Config value must never hold the LLM API key itself, the same
// guarantee TestConfigNeverHoldsSecretValues gives the Slack tokens.
func TestConfigNeverHoldsLLMAPIKeyValue(t *testing.T) {
	setCredentials(t)
	t.Setenv(DefaultLLMAPIKeyEnv, "sk-super-secret-openrouter-key")

	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.LLM.APIKeyEnv == "sk-super-secret-openrouter-key" {
		t.Error("Config.LLM holds the secret value instead of the env var name")
	}
}

func TestParse_PresentationOverride(t *testing.T) {
	setCredentials(t)

	yaml := minimalYAML + `
presentation:
  min_error_samples: 10
  concentration_threshold: 0.75
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Presentation.MinErrorSamples != 10 {
		t.Errorf("Presentation.MinErrorSamples = %d, want 10", cfg.Presentation.MinErrorSamples)
	}
	if cfg.Presentation.ConcentrationThreshold != 0.75 {
		t.Errorf("Presentation.ConcentrationThreshold = %v, want 0.75", cfg.Presentation.ConcentrationThreshold)
	}
	// A field left out of the file is zero here (this package applies no
	// presentation defaults of its own - see applyDefaults); rca.Decide and
	// rca.Present fill it in via PresentationConfig.WithDefaults() when the
	// converted value is actually used.
	if cfg.Presentation.LatencyShiftThreshold != 0 {
		t.Errorf("Presentation.LatencyShiftThreshold = %v, want the zero value (rca defaults it on use)", cfg.Presentation.LatencyShiftThreshold)
	}
}

func TestParse_RejectsUnknownStorageDriver(t *testing.T) {
	setCredentials(t)
	_, err := Parse([]byte(minimalYAML + "\nstorage:\n  driver: postgress\n"))
	if err == nil || !strings.Contains(err.Error(), "unsupported driver") {
		t.Fatalf("Parse() error = %v, want unsupported storage driver", err)
	}
}
