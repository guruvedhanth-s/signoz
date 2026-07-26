package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func TestFileStoreRoundTripsSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := View{CorrelationID: "c1", ChannelID: "C1", ThreadTS: "T1", Fingerprint: "svc|prod", Service: "svc", Environment: "prod", Window: "1h", Status: StatusOpen, Diagnosis: notify.Diagnosis{CorrelationID: "c1"}, CreatedAt: time.Now().UTC(), LastActivity: time.Now().UTC(), History: []Turn{{Actor: "U1", Text: "hello", At: time.Now().UTC()}}}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("C1", "T1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CorrelationID != want.CorrelationID || len(got.History) != 1 || got.History[0].Text != "hello" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestManagerRehydratesFromFileStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, _ := NewFileStore(path)
	first := NewManager(WithStore(store))
	diagnosis := notify.Diagnosis{CorrelationID: "c1", Service: "svc", Environment: "prod", Window: "1h"}
	created, _, err := first.Open(OpenRequest{ChannelID: "C1", ThreadTS: "T1", Diagnosis: diagnosis})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendTurn(created, Turn{Actor: "U1", Text: "question"}); err != nil {
		t.Fatal(err)
	}
	second := NewManager(WithStore(store))
	rehydrated, ok := second.ByThread("C1", "T1")
	if !ok {
		t.Fatal("session was not rehydrated")
	}
	if rehydrated.CorrelationID != "c1" || len(rehydrated.History()) != 1 {
		t.Fatalf("rehydrated = %+v", rehydrated.View())
	}
}

func TestManagerPersistsTouchAndFingerprintAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, _ := NewFileStore(path)
	now := time.Now().UTC()
	first := NewManager(WithStore(store), WithClock(func() time.Time { return now }))
	diagnosis := notify.Diagnosis{CorrelationID: "c1", Service: "svc", Environment: "prod", Window: "1h", Grounding: notify.Grounding{SLO: "slo"}}
	created, _, err := first.Open(OpenRequest{ChannelID: "C1", ThreadTS: "T1", Diagnosis: diagnosis})
	if err != nil {
		t.Fatal(err)
	}
	first.Touch(created)
	second := NewManager(WithStore(store))
	rehydrated, ok := second.ByFingerprint(Fingerprint("svc", "prod", "slo"))
	if !ok || rehydrated.ThreadTS != "T1" {
		t.Fatalf("fingerprint rehydration = %v, %+v", ok, rehydrated)
	}
}

func TestManagerDeletesExpiredPersistentSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, _ := NewFileStore(path)
	now := time.Now().UTC()
	first := NewManager(WithStore(store), WithClosedRetention(time.Nanosecond), WithClock(func() time.Time { return now }))
	diagnosis := notify.Diagnosis{CorrelationID: "c1", Service: "svc", Environment: "prod"}
	created, _, err := first.Open(OpenRequest{ChannelID: "C1", ThreadTS: "T1", Diagnosis: diagnosis})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(created, ReasonClosedByUser); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	first.ReapIdle()
	second := NewManager(WithStore(store))
	if _, ok := second.ByThread("C1", "T1"); ok {
		t.Fatal("expired persistent session was not deleted")
	}
}

func TestFileAuditorIsAppendOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	auditor, err := NewFileAuditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := auditor.Append(AuditEvent{Type: "session_opened", CorrelationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := auditor.Append(AuditEvent{Type: "decision_recorded", CorrelationID: "c1", Actor: "U1"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(b, []byte("\n")); lines != 2 {
		t.Fatalf("audit lines = %d", lines)
	}
}
