package versioning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type LockFile struct {
	Version          string   `json:"version"`
	UsedSystemBinary bool     `json:"used_system_binary"`
	BinaryPath       string   `json:"binary_path,omitempty"`
	DetectedFrom     []string `json:"detected_from"`
}

func ReadLockFile(path string) (*LockFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lock file: %w", err)
	}

	var lock LockFile
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse lock file: %w", err)
	}

	lock.normalize()
	return &lock, nil
}

func WriteLockFile(path string, lock LockFile) error {
	lock.normalize()

	if lock.Version == "" {
		return errors.New("lock file version cannot be empty")
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create lock file directory: %w", err)
		}
	}

	contents, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lock file: %w", err)
	}
	contents = append(contents, byte('\n'))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, contents, 0o644); err != nil {
		return fmt.Errorf("write temp lock file: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("persist lock file: %w", err)
	}

	return nil
}

func (l *LockFile) normalize() {
	if l == nil {
		return
	}
	if len(l.DetectedFrom) == 0 {
		return
	}
	unique := make(map[string]struct{}, len(l.DetectedFrom))
	for _, stack := range l.DetectedFrom {
		stack = filepath.ToSlash(stack)
		stack = filepath.Clean(stack)
		if stack == "." {
			stack = ""
		}
		stack = strings.TrimPrefix(stack, "./")
		if stack == "" {
			continue
		}
		unique[stack] = struct{}{}
	}
	if len(unique) == 0 {
		l.DetectedFrom = nil
		return
	}
	cleaned := make([]string, 0, len(unique))
	for stack := range unique {
		cleaned = append(cleaned, stack)
	}
	sort.Strings(cleaned)
	l.DetectedFrom = cleaned
}
