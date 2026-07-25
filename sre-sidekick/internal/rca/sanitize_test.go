package rca

import (
	"strings"
	"testing"
)

func TestSanitizeFields_Allowlist(t *testing.T) {
	record := map[string]any{
		"body":             "hello world",
		"severity_text":    "ERROR",
		"unexpected_field": "should be dropped",
	}
	got := SanitizeFields(record, []string{"body", "severity_text"})
	if len(got) != 2 {
		t.Fatalf("got %d fields, want 2: %+v", len(got), got)
	}
	if got["body"] != "hello world" {
		t.Errorf("body = %q, want %q", got["body"], "hello world")
	}
	if got["severity_text"] != "ERROR" {
		t.Errorf("severity_text = %q, want %q", got["severity_text"], "ERROR")
	}
	if _, ok := got["unexpected_field"]; ok {
		t.Errorf("unexpected_field should have been dropped by the allowlist, got %+v", got)
	}
}

func TestSanitizeFields_RedactsSecrets(t *testing.T) {
	cases := []string{
		"password", "api_key", "apiKey", "Authorization", "SESSION_ID",
		"access_token", "private_key", "cookie", "credential",
	}
	for _, field := range cases {
		record := map[string]any{field: "super-secret-value"}
		got := SanitizeFields(record, []string{field})
		if got[field] != redacted {
			t.Errorf("field %q = %q, want redacted", field, got[field])
		}
	}
}

func TestSanitizeFields_DoesNotRedactUnrelatedFields(t *testing.T) {
	record := map[string]any{"body": "the token bucket refilled"}
	got := SanitizeFields(record, []string{"body"})
	if got["body"] != "the token bucket refilled" {
		t.Errorf("body = %q, want unredacted (field name is body, not token)", got["body"])
	}
}

func TestSanitizeFields_StripsControlChars(t *testing.T) {
	record := map[string]any{"body": "line one\x1bline two\x00\x07"}
	got := SanitizeFields(record, []string{"body"})
	if strings.ContainsAny(got["body"], "\x00\x07\x1b") {
		t.Errorf("body retained control characters: %q", got["body"])
	}
}

func TestSanitizeFields_TruncatesLongValues(t *testing.T) {
	record := map[string]any{"body": strings.Repeat("a", maxFieldLen*3)}
	got := SanitizeFields(record, []string{"body"})
	if len(got["body"]) > maxFieldLen {
		t.Errorf("body length = %d, want <= %d", len(got["body"]), maxFieldLen)
	}
	if !strings.HasSuffix(got["body"], truncatedSuffix) {
		t.Errorf("body = %q, want a truncated suffix marker", got["body"])
	}
}

func TestSanitizeFields_OmitsMissingAndEmptyFields(t *testing.T) {
	record := map[string]any{"body": "", "other": "present"}
	got := SanitizeFields(record, []string{"body", "missing", "other"})
	if _, ok := got["body"]; ok {
		t.Errorf("empty body should be omitted, got %+v", got)
	}
	if _, ok := got["missing"]; ok {
		t.Errorf("missing field should be omitted, got %+v", got)
	}
	if got["other"] != "present" {
		t.Errorf("other = %q, want %q", got["other"], "present")
	}
}

func TestSanitizeNote_StripsControlCharsAndTruncates(t *testing.T) {
	note := strings.Repeat("x", maxNoteLen*2) + "\x00\x1b[31mFAKE ALERT\x1b[0m"
	got := SanitizeNote(note)
	if strings.ContainsAny(got, "\x00\x1b") {
		t.Errorf("note retained control characters: %q", got)
	}
	if len(got) > maxNoteLen {
		t.Errorf("note length = %d, want <= %d", len(got), maxNoteLen)
	}
}

func TestSanitizeNote_ShortNoteUnchanged(t *testing.T) {
	note := "operation tool.search_kb errored: timed out after 5s"
	if got := SanitizeNote(note); got != note {
		t.Errorf("SanitizeNote(%q) = %q, want unchanged", note, got)
	}
}
