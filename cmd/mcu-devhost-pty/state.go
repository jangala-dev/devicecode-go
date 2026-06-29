//go:build !tinygo

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"devicecode-go/services/updater"
)

type imageState struct {
	ImageID       string `json:"image_id"`
	Version       string `json:"version"`
	BuildID       string `json:"build_id"`
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
}

type devhostStateFile struct {
	BootSeq int                       `json:"boot_seq"`
	Running imageState                `json:"running"`
	Staged  *updater.StagedDescriptor `json:"staged,omitempty"`
}

type stateStore struct {
	mu   sync.Mutex
	dir  string
	path string
	data devhostStateFile
}

func openStateStore(dir string, initial imageState) (*stateStore, error) {
	if dir == "" {
		return nil, errors.New("state-dir required")
	}
	if initial.ImageID == "" {
		return nil, errors.New("initial image id required")
	}
	if initial.Version == "" {
		initial.Version = "0.0.0-devhost"
	}
	if initial.BuildID == "" {
		initial.BuildID = "devhost-initial"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &stateStore{dir: dir, path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, err
		}
		if s.data.Running.ImageID == "" {
			return nil, errors.New("state running image_id missing")
		}
		return s, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.data = devhostStateFile{BootSeq: 1, Running: initial}
	return s, s.saveLocked()
}

func (s *stateStore) identity() updater.Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return updater.Identity{Version: s.data.Running.Version, Build: s.data.Running.BuildID, ImageID: s.data.Running.ImageID}
}

func (s *stateStore) PayloadSHA256() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Running.PayloadSHA256
}

func (s *stateStore) StagedDescriptor() (updater.StagedDescriptor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Staged == nil {
		return updater.StagedDescriptor{}, false
	}
	return *s.data.Staged, true
}

func (s *stateStore) WriteStagedDescriptor(d updater.StagedDescriptor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Staged = &d
	return s.saveLocked()
}

func (s *stateStore) ClearStagedDescriptor() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Staged = nil
	return s.saveLocked()
}

func (s *stateStore) MarkRunningFromStaged(d updater.StagedDescriptor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.BootSeq++
	s.data.Running = imageState{ImageID: d.ImageID, Version: d.Version, BuildID: d.BuildID, PayloadSHA256: d.PayloadSHA256}
	s.data.Staged = nil
	return s.saveLocked()
}

func (s *stateStore) saveLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
