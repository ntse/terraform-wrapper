package output

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type State string

const (
	StatePending   State = "pending"
	StateWaiting   State = "waiting"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateSkipped   State = "skipped"
)

type Manager struct {
	mu     sync.Mutex
	states map[string]State
	start  map[string]time.Time
}

func NewManager() *Manager {
	return &Manager{
		states: make(map[string]State),
		start:  make(map[string]time.Time),
	}
}

func (m *Manager) Register(stack string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[stack] = StatePending
}

func (m *Manager) Waiting(stack string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[stack] = StateWaiting
	fmt.Fprintf(os.Stdout, "[wait] %s (%s)\n", stack, reason)
}

func (m *Manager) Start(stack string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[stack] = StateRunning
	m.start[stack] = time.Now()
	fmt.Fprintf(os.Stdout, "[run] %s\n", stack)
}

func (m *Manager) Skip(stack string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[stack] = StateSkipped
	fmt.Fprintf(os.Stdout, "[skip] %s (%s)\n", stack, reason)
}

func (m *Manager) Succeed(stack string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[stack] = StateSucceeded
	dur := time.Since(m.start[stack])
	fmt.Fprintf(os.Stdout, "[done] %s (%.1fs)\n", stack, dur.Seconds())
}

func (m *Manager) Fail(stack string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[stack] = StateFailed
	dur := time.Since(m.start[stack])
	fmt.Fprintf(os.Stdout, "[fail] %s (%.1fs): %v\n", stack, dur.Seconds(), err)
}
