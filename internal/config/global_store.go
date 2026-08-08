package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/spf13/viper"
)

type globalConfigStore struct {
	path string
}

var beforeGlobalConfigPublish = func(string, string) {}

func (s globalConfigStore) withLock(change func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create global config directory: %w", err)
	}
	lock := flock.New(s.path+".lock", flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock global config: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	return change()
}

func (s globalConfigStore) mutate(
	change func(*viper.Viper) (bool, error),
) (bool, error) {
	changed := false
	err := s.withLock(func() error {
		current, err := readGlobalViper(s.path)
		if err != nil {
			return err
		}
		changed, err = change(current)
		if err != nil || !changed {
			return err
		}
		return writeGlobalViperAtomically(s.path, current)
	})
	return changed && err == nil, err
}

func (s globalConfigStore) ensure(contents string) (bool, error) {
	created := false
	err := s.withLock(func() error {
		targetPath, err := globalConfigWriteTarget(s.path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(targetPath); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat global config: %w", err)
		}
		if err := writeGlobalConfigAtomically(targetPath, 0o600, func(file *os.File) error {
			_, err := file.WriteString(contents)
			return err
		}); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

func readGlobalViper(path string) (*viper.Viper, error) {
	current := viper.New()
	current.SetConfigFile(path)
	current.SetConfigType(configType)
	if err := current.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok || os.IsNotExist(err) {
			return current, nil
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	return current, nil
}

func writeGlobalViperAtomically(path string, current *viper.Viper) (err error) {
	targetPath, err := globalConfigWriteTarget(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(targetPath); statErr == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat global config: %w", statErr)
	}
	return writeGlobalConfigAtomically(targetPath, mode, func(temp *os.File) error {
		if err := current.WriteConfigTo(temp); err != nil {
			return fmt.Errorf("write global config: %w", err)
		}
		return nil
	})
}

func globalConfigWriteTarget(path string) (string, error) {
	targetPath := filepath.Clean(path)
	seen := make(map[string]struct{})
	for {
		if _, ok := seen[targetPath]; ok {
			return "", fmt.Errorf("resolve global config symlink: cycle at %s", targetPath)
		}
		seen[targetPath] = struct{}{}
		info, err := os.Lstat(targetPath)
		if err != nil {
			if os.IsNotExist(err) {
				return targetPath, nil
			}
			return "", fmt.Errorf("inspect global config path: %w", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return targetPath, nil
		}
		linkTarget, err := os.Readlink(targetPath)
		if err != nil {
			return "", fmt.Errorf("read global config symlink: %w", err)
		}
		if !filepath.IsAbs(linkTarget) {
			linkTarget = filepath.Join(filepath.Dir(targetPath), linkTarget)
		}
		targetPath = filepath.Clean(linkTarget)
	}
}

func writeGlobalConfigAtomically(
	path string,
	mode os.FileMode,
	write func(*os.File) error,
) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create global config temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := write(temp); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set global config permissions: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync global config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close global config: %w", err)
	}
	beforeGlobalConfigPublish(tempPath, path)
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace global config: %w", err)
	}
	return nil
}
