package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"go.kenn.io/kit/safefileio"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/service"
)

const (
	backgroundLogName      = "daemon.log"
	backgroundLogSize      = int64(10 << 20)
	backgroundBackups      = 3
	maximumDiagnosticBytes = 1024
)

var (
	sensitiveAssignmentPattern = regexp.MustCompile(
		`(?i)\b([a-z_][a-z0-9_]*(?:token|secret|password|passwd|credential|api_key)[a-z0-9_]*)=("[^"]*"|'[^']*'|[^\s]+)`,
	)
	authorizationHeaderPattern = regexp.MustCompile(
		`(?i)(\b(?:proxy-)?authorization\s*:\s*)[^\r\n]*`,
	)
	credentialURLPattern = regexp.MustCompile(
		`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@`,
	)
)

type rotatingLog struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	size    int64
	maxSize int64
	backups int
}

func logServiceFailure(
	logger *slog.Logger,
	route string,
	failure *service.Error,
	sensitiveValues []string,
) {
	if logger == nil || failure == nil || failure.Err == nil {
		return
	}
	logger.Warn(
		"daemon request failed",
		"route", route,
		"code", failure.Code,
		"error", privateDiagnostic(failure.Err, sensitiveValues),
	)
}

func privateDiagnostic(err error, sensitiveValues []string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	message = sensitiveAssignmentPattern.ReplaceAllString(message, "$1=[redacted]")
	message = authorizationHeaderPattern.ReplaceAllString(message, "$1[redacted]")
	message = credentialURLPattern.ReplaceAllString(message, "$1[redacted]@")
	message = redactSensitiveValues(message, sensitiveValues)
	if len(message) > maximumDiagnosticBytes {
		message = message[:maximumDiagnosticBytes]
	}
	return message
}

func redactSensitiveValues(message string, sensitiveValues []string) string {
	unique := make(map[string]struct{}, len(sensitiveValues))
	for _, value := range sensitiveValues {
		if value != "" {
			unique[value] = struct{}{}
		}
	}
	values := make([]string, 0, len(unique))
	for value := range unique {
		values = append(values, value)
	}
	sort.Slice(values, func(left, right int) bool {
		if len(values[left]) == len(values[right]) {
			return values[left] < values[right]
		}
		return len(values[left]) > len(values[right])
	})
	replacements := make([]string, 0, len(values)*2)
	for _, value := range values {
		replacements = append(replacements, value, "[redacted]")
	}
	if len(replacements) == 0 {
		return message
	}
	return strings.NewReplacer(replacements...).Replace(message)
}

func processDiagnosticSecrets(bearer string, fleetTokenEnvironment string) []string {
	values := make([]string, 0, 4)
	if bearer != "" {
		values = append(values, bearer)
	}
	if name := strings.TrimSpace(fleetTokenEnvironment); name != "" {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			values = append(values, value)
		}
	}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || !sensitiveEnvironmentName(name) {
			continue
		}
		values = append(values, value)
	}
	return values
}

func invocationDiagnosticSecrets(home string, expansion kwt.ExpansionContext) []string {
	values := make([]string, 0, 4)
	for name, value := range expansion.Environment {
		if value != "" && sensitiveEnvironmentName(name) {
			values = append(values, value)
		}
	}
	fleetTokenEnvironment := configuredFleetTokenEnvironment(home)
	if fleetTokenEnvironment == "" {
		return values
	}
	if value := environmentValue(
		expansion.Environment,
		fleetTokenEnvironment,
	); value != "" {
		values = append(values, value)
	}
	return values
}

func configuredFleetTokenEnvironment(home string) string {
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(
		home,
		func(path string) (string, error) { return path, nil },
	)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(snapshot.Config.Fleet.TokenEnv)
}

func environmentValue(environment map[string]string, name string) string {
	if runtime.GOOS != "windows" {
		return environment[name]
	}
	for candidate, value := range environment {
		if strings.EqualFold(candidate, name) {
			return value
		}
	}
	return ""
}

func sensitiveEnvironmentName(name string) bool {
	name = strings.ToUpper(name)
	for _, marker := range []string{
		"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
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
	dir := filepath.Dir(path)
	if filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("daemon log directory must not be a filesystem root")
	}
	// The private, current-user-owned parent makes the validation/open gap
	// accessible only to the operating-system account that is already the
	// daemon trust boundary. Created files inherit its Windows DACL.
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("secure daemon log directory: %w", err)
	}
	for index := 0; index <= backups; index++ {
		candidate := path
		if index > 0 {
			candidate += fmt.Sprintf(".%d", index)
		}
		if err := validateExistingLogPath(candidate); err != nil {
			return nil, err
		}
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
	if err := safefileio.ValidateCurrentUserFile(file); err != nil {
		_ = file.Close()
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

func validateExistingLogPath(path string) error {
	file, err := safefileio.OpenCurrentUserFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("validate daemon log %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close daemon log %s after validation: %w", path, err)
	}
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
