package whatsapp

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FileSessionStorage implements SessionStorage using local files
type FileSessionStorage struct {
	Path string
}

// NewFileSessionStorage creates a new file-based session storage
func NewFileSessionStorage(path string) *FileSessionStorage {
	return &FileSessionStorage{Path: path}
}

// Save persists session data to file
func (s *FileSessionStorage) Save(session *SessionData) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return os.WriteFile(s.Path, data, 0600)
}

// Load retrieves session data from file
func (s *FileSessionStorage) Load() (*SessionData, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	var session SessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}