package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

func PlanDir(root, env, stackRel string) string {
	return filepath.Join(root, ".terraform-wrapper", "cache", env, stackRel)
}

func PlanFiles(root, env, stackRel string) (planPath, hashPath string) {
	dir := PlanDir(root, env, stackRel)
	return filepath.Join(dir, "plan.tfplan"), filepath.Join(dir, "plan.hash")
}

func SaveHash(path string, hash []byte) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(hash)), 0o644)
}

func LoadHash(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded := make([]byte, hex.DecodedLen(len(data)))
	n, err := hex.Decode(decoded, data)
	if err != nil {
		return nil, err
	}
	return decoded[:n], nil
}

func ComputeHash(files []string) ([]byte, error) {
	h := sha256.New()
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	for _, path := range sorted {
		if err := hashFile(h, path); err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func hashFile(h hash.Hash, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if closeErr != nil {
			err = closeErr
		}
	}()

	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return nil
}

func StackContentFiles(stackDir string, extras []string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(stackDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".tf" || ext == ".tfvars" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	files = append(files, extras...)
	return files, nil
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
