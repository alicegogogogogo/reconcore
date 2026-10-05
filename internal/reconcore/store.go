package reconcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// State is the complete database. It is small enough to live in memory and to
// be rewritten atomically as one JSON document after every mutation, which is
// what replaces a real database driver in this standard-library-only build.
type State struct {
	Accounts              map[string]*Account              `json:"accounts"`
	Journals              map[string]*Journal              `json:"journals"`
	JournalBatches        map[string]*JournalBatch         `json:"journal_batches"`
	Rates                 []*Rate                          `json:"rates"`
	Statements            map[string]*Statement            `json:"statements"`
	Reconciliations       map[string]*Reconciliation       `json:"reconciliations"`
	Resolutions           map[string]*Resolution           `json:"resolutions"`
	ReconciliationReports map[string]*ReconciliationReport `json:"reconciliation_reports"`
	PeriodCloses          map[string]*PeriodClose          `json:"period_closes"`
	Idempotency           map[string]*IdempotencyRecord    `json:"idempotency"`
}

func newState() *State {
	return &State{
		Accounts:              map[string]*Account{},
		Journals:              map[string]*Journal{},
		JournalBatches:        map[string]*JournalBatch{},
		Rates:                 []*Rate{},
		Statements:            map[string]*Statement{},
		Reconciliations:       map[string]*Reconciliation{},
		Resolutions:           map[string]*Resolution{},
		ReconciliationReports: map[string]*ReconciliationReport{},
		PeriodCloses:          map[string]*PeriodClose{},
		Idempotency:           map[string]*IdempotencyRecord{},
	}
}

// Store owns the in-memory state and its file. Every mutation runs against a
// copy that is persisted first, so a failed write never changes the process
// state and no reader ever sees a half-applied transaction.
type Store struct {
	path  string
	mutex sync.Mutex
	state *State
}

// OpenStore loads the database at path, creating it on first write.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = "reconcore.db"
	}
	state := newState()
	contents, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(contents, state); err != nil {
			return nil, InternalError("database %s is not readable JSON: %s", path, err)
		}
		state.repair()
	case os.IsNotExist(err):
	default:
		return nil, InternalError("database %s could not be read: %s", path, err)
	}
	return &Store{path: path, state: state}, nil
}

func (s *State) repair() {
	if s.Accounts == nil {
		s.Accounts = map[string]*Account{}
	}
	if s.Journals == nil {
		s.Journals = map[string]*Journal{}
	}
	if s.JournalBatches == nil {
		s.JournalBatches = map[string]*JournalBatch{}
	}
	if s.Rates == nil {
		s.Rates = []*Rate{}
	}
	if s.Statements == nil {
		s.Statements = map[string]*Statement{}
	}
	if s.Reconciliations == nil {
		s.Reconciliations = map[string]*Reconciliation{}
	}
	if s.Resolutions == nil {
		s.Resolutions = map[string]*Resolution{}
	}
	if s.ReconciliationReports == nil {
		s.ReconciliationReports = map[string]*ReconciliationReport{}
	}
	if s.PeriodCloses == nil {
		s.PeriodCloses = map[string]*PeriodClose{}
	}
	if s.Idempotency == nil {
		s.Idempotency = map[string]*IdempotencyRecord{}
	}
}

// Update runs mutate against a copy of the state and commits it on success.
func (s *Store) Update(mutate func(state *State) error) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	clone, err := cloneState(s.state)
	if err != nil {
		return err
	}
	if err := mutate(clone); err != nil {
		return err
	}
	if err := writeState(s.path, clone); err != nil {
		return err
	}
	s.state = clone
	return nil
}

// View runs read against the committed state under the store lock.
func (s *Store) View(read func(state *State) (any, error)) (any, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return read(s.state)
}

func cloneState(state *State) (*State, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, InternalError("database could not be serialised: %s", err)
	}
	clone := newState()
	if err := json.Unmarshal(encoded, clone); err != nil {
		return nil, InternalError("database could not be restored: %s", err)
	}
	return clone, nil
}

func writeState(path string, state *State) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return InternalError("database could not be serialised: %s", err)
	}
	encoded = append(encoded, '\n')
	if directory := filepath.Dir(path); directory != "" && directory != "." {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return InternalError("database directory could not be created: %s", err)
		}
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o644); err != nil {
		return InternalError("database could not be written: %s", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return InternalError("database could not be committed: %s", err)
	}
	return nil
}
