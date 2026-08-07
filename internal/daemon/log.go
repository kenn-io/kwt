package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	backgroundLogName = "daemon.log"
	backgroundLogSize = int64(10 << 20)
	backgroundBackups = 3
)

type rotatingLog struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	size    int64
	maxSize int64
	backups int
}

func openBackgroundLog(home string) (*rotatingLog, error) {
	return openRotatingLog(
		filepath.Join(home, backgroundLogName),
		backgroundLogSize,
		backgroundBackups,
	)
}

func openRotatingLog(path string, maxSize int64, backups int) (*rotatingLog, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("daemon log path must be absolute")
	}
	if maxSize <= 0 || backups <= 0 {
		return nil, fmt.Errorf("invalid daemon log limits")
	}
	log := &rotatingLog{path: path, maxSize: maxSize, backups: backups}
	if info, err := os.Stat(path); err == nil && info.Size() >= maxSize {
		if err := log.rotateFiles(); err != nil {
			return nil, err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := log.open(); err != nil {
		return nil, err
	}
	return log, nil
}

func (l *rotatingLog) open() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	l.file, l.size = file, info.Size()
	return nil
}

func (l *rotatingLog) Write(body []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(body)) > l.maxSize {
		if err := l.file.Close(); err != nil {
			return 0, err
		}
		l.file = nil
		if err := l.rotateFiles(); err != nil {
			return 0, err
		}
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	written, err := l.file.Write(body)
	l.size += int64(written)
	return written, err
}

func (l *rotatingLog) rotateFiles() error {
	if err := os.Remove(l.path + fmt.Sprintf(".%d", l.backups)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for index := l.backups - 1; index >= 1; index-- {
		from := l.path + fmt.Sprintf(".%d", index)
		to := l.path + fmt.Sprintf(".%d", index+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
