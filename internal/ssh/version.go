package ssh

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"go.kenn.io/kwt/service"
)

var openSSHVersionPattern = regexp.MustCompile( //nolint:gochecknoglobals
	`OpenSSH(?:_for_Windows)?[_ ]([0-9]+)\.([0-9]+)`,
)

type OpenSSHVersion struct {
	Major int
	Minor int
}

func parseOpenSSHVersion(output string) (OpenSSHVersion, error) {
	match := openSSHVersionPattern.FindStringSubmatch(output)
	if len(match) != 3 {
		return OpenSSHVersion{}, errors.New("authoritative OpenSSH version not found")
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return OpenSSHVersion{}, fmt.Errorf("parse OpenSSH major version: %w", err)
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return OpenSSHVersion{}, fmt.Errorf("parse OpenSSH minor version: %w", err)
	}
	return OpenSSHVersion{Major: major, Minor: minor}, nil
}

type VersionRunner func(context.Context) (string, error)

type VersionPolicy struct {
	run VersionRunner

	mu       sync.Mutex
	running  chan struct{}
	checked  bool
	version  OpenSSHVersion
	checkErr error
}

func NewVersionPolicy(run VersionRunner) *VersionPolicy {
	return &VersionPolicy{run: run}
}

func (p *VersionPolicy) RequireInteractive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	version, err := p.current(ctx)
	if err != nil {
		return err
	}
	if version.Major < 8 || version.Major == 8 && version.Minor < 4 {
		return service.NewError(
			service.SSHUnsupportedVersion,
			"system OpenSSH 8.4 or newer is required",
			false,
			nil,
			nil,
		)
	}
	return nil
}

func (p *VersionPolicy) current(ctx context.Context) (OpenSSHVersion, error) {
	for {
		p.mu.Lock()
		if p.checked {
			version, err := p.version, p.checkErr
			p.mu.Unlock()
			return version, err
		}
		if p.running != nil {
			done := p.running
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return OpenSSHVersion{}, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		p.running = done
		p.mu.Unlock()

		version, err := p.runVersion(ctx)

		p.mu.Lock()
		p.running = nil
		if ctx.Err() == nil {
			p.checked = true
			p.version = version
			p.checkErr = err
		}
		close(done)
		p.mu.Unlock()
		return version, err
	}
}

func (p *VersionPolicy) runVersion(ctx context.Context) (OpenSSHVersion, error) {
	if p.run == nil {
		return OpenSSHVersion{}, errors.New("OpenSSH version runner is unavailable")
	}
	output, err := p.run(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return OpenSSHVersion{}, errors.Join(ctx.Err(), err)
		}
		return OpenSSHVersion{}, err
	}
	return parseOpenSSHVersion(output)
}
