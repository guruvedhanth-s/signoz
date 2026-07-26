package session

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

type AuditEvent struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Service       string    `json:"service,omitempty"`
	Environment   string    `json:"environment,omitempty"`
	Actor         string    `json:"actor,omitempty"`
	At            time.Time `json:"at"`
	Payload       string    `json:"payload,omitempty"`
}
type Auditor interface{ Append(AuditEvent) error }

// FileAuditor is an append-only JSONL audit log. Payload is base64 encoded to
// keep each record one line even when it contains structured JSON.
type FileAuditor struct {
	mu   sync.Mutex
	path string
}

func NewFileAuditor(path string) (*FileAuditor, error) {
	if path == "" {
		return nil, errors.New("session: audit path is required")
	}
	return &FileAuditor{path: path}, nil
}
func (a *FileAuditor) Append(event AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if event.ID == "" {
		event.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload != "" {
		event.Payload = base64.StdEncoding.EncodeToString([]byte(event.Payload))
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Store persists session snapshots. Implementations must replace a snapshot
// atomically; callers never mutate a stored value in place.
type Store interface {
	Save(View) error
	Load(channelID, threadTS string) (View, error)
	Delete(channelID, threadTS string) error
}

var ErrNotFound = errors.New("session: not found")

// MemoryStore is the default store and keeps the current in-memory behavior.
type MemoryStore struct {
	mu     sync.RWMutex
	values map[string]View
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{values: make(map[string]View)} }

func (s *MemoryStore) Save(v View) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]View)
	}
	s.values[storeKey(v.ChannelID, v.ThreadTS)] = cloneView(v)
	return nil
}
func (s *MemoryStore) Load(channelID, threadTS string) (View, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[storeKey(channelID, threadTS)]
	if !ok {
		return View{}, ErrNotFound
	}
	return cloneView(v), nil
}
func (s *MemoryStore) Delete(channelID, threadTS string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, storeKey(channelID, threadTS))
	return nil
}

// FileStore provides durable local persistence without requiring another
// service. The file is rewritten through a temporary file and rename, so a
// process crash cannot leave a partially-written session database.
type FileStore struct {
	mu   sync.Mutex
	path string
}

func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("session: file store path is required")
	}
	return &FileStore{path: path}, nil
}

func (s *FileStore) Save(v View) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return err
	}
	values[storeKey(v.ChannelID, v.ThreadTS)] = cloneView(v)
	return s.write(values)
}
func (s *FileStore) Load(channelID, threadTS string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return View{}, err
	}
	v, ok := values[storeKey(channelID, threadTS)]
	if !ok {
		return View{}, ErrNotFound
	}
	return cloneView(v), nil
}
func (s *FileStore) Delete(channelID, threadTS string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return err
	}
	delete(values, storeKey(channelID, threadTS))
	return s.write(values)
}

func (s *FileStore) read() (map[string]View, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]View{}, nil
	}
	if err != nil {
		return nil, err
	}
	var values map[string]View
	if len(b) == 0 {
		return map[string]View{}, nil
	}
	if err := json.Unmarshal(b, &values); err != nil {
		return nil, err
	}
	if values == nil {
		values = make(map[string]View)
	}
	return values, nil
}
func (s *FileStore) write(values map[string]View) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(values)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sessions-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

func storeKey(channelID, threadTS string) string { return channelID + "\x00" + threadTS }
func cloneView(v View) View {
	v.ExtraEvidence = append([]notify.Evidence(nil), v.ExtraEvidence...)
	v.History = append([]Turn(nil), v.History...)
	v.Participants = append([]string(nil), v.Participants...)
	if v.Decision != nil {
		d := *v.Decision
		v.Decision = &d
	}
	return v
}
