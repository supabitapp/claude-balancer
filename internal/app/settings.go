package app

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type fastMode string

const (
	fastModeDefault  fastMode = "default"
	fastModeOff      fastMode = "off"
	fastModeLegacyOn fastMode = "on"
)

func (m fastMode) valid() bool {
	return m == fastModeDefault || m == fastModeOff
}

func storedFastMode(value string) (fastMode, error) {
	mode := fastMode(value)
	if mode == fastModeLegacyOn {
		return fastModeDefault, nil
	}
	if !mode.valid() {
		return "", fmt.Errorf("invalid stored fast mode %q", value)
	}
	return mode, nil
}

func (m fastMode) label() string {
	if m == fastModeOff {
		return "force standard"
	}
	return "default"
}

type fastModePolicy struct {
	mu   sync.Mutex
	mode fastMode
}

func (p *fastModePolicy) snapshot() fastMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == "" {
		return fastModeDefault
	}
	return p.mode
}

func (p *fastModePolicy) set(mode fastMode) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == mode {
		return false
	}
	p.mode = mode
	return true
}

func (s *server) reloadSettings() error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	value, err := s.pool.store.raw.FastMode()
	if err != nil {
		return err
	}
	mode, err := storedFastMode(value)
	if err != nil {
		return err
	}
	if s.fastMode.set(mode) {
		s.log.Info("fast mode applied", "fast_mode", mode)
	}
	return nil
}

func (s *server) watchSettings(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reloadSettings(); err != nil {
				s.log.Warn("settings watch failed", "error", err)
			}
		}
	}
}

func (s *server) saveFastMode(mode fastMode) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if !mode.valid() {
		return fmt.Errorf("invalid fast mode %q", mode)
	}
	if err := s.pool.store.raw.SetFastMode(string(mode)); err != nil {
		return err
	}
	s.fastMode.set(mode)
	return nil
}
