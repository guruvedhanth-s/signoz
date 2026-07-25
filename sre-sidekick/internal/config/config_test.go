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
	t.Setenv(DefaultSigningSecretEnv, "test-signing-secret")
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
	if slack.SigningSecretEnv != DefaultSigningSecretEnv {
		t.Errorf("SigningSecretEnv = %q, want %q", slack.SigningSecretEnv, DefaultSigningSecretEnv)
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
	t.Setenv("CUSTOM_SECRET", "custom-secret")

	yaml := `
notify:
  slack:
    bot_token_env: CUSTOM_TOKEN
    signing_secret_env: CUSTOM_SECRET
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
	if slack.SigningSecretEnv != "CUSTOM_SECRET" {
		t.Errorf("SigningSecretEnv = %q", slack.SigningSecretEnv)
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
	t.Setenv(DefaultSigningSecretEnv, "secret")

	_, err := Parse([]byte(minimalYAML))
	if err == nil {
		t.Fatal("Parse() error = nil, want missing-credential error")
	}
	if !strings.Contains(err.Error(), DefaultBotTokenEnv) {
		t.Errorf("Parse() error = %q, want it to name %s", err, DefaultBotTokenEnv)
	}

	t.Setenv(DefaultBotTokenEnv, "xoxb-test-token")
	t.Setenv(DefaultSigningSecretEnv, "")
	if _, err := Parse([]byte(minimalYAML)); err == nil {
		t.Fatal("Parse() error = nil, want missing signing secret error")
	} else if !strings.Contains(err.Error(), DefaultSigningSecretEnv) {
		t.Errorf("Parse() error = %q, want it to name %s", err, DefaultSigningSecretEnv)
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

	t.Setenv(DefaultSigningSecretEnv, "  s3cr3t  ")
	secret, err := slack.SigningSecret()
	if err != nil {
		t.Fatalf("SigningSecret() error = %v", err)
	}
	if secret != "s3cr3t" {
		t.Errorf("SigningSecret() = %q, want trimmed value", secret)
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
	b.WriteString(cfg.Notify.Slack.SigningSecretEnv)
	b.WriteString(cfg.Notify.Slack.DefaultChannel)
	b.WriteString(cfg.Notify.Slack.SessionTTL)
	return b.String()
}
