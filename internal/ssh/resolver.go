package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/internal/credentials"
)

const resolverOutputLimit = 1 << 20

var errResolverOutputLimit = errors.New("SSH resolver output exceeds limit")

type OutputRunner func(
	ctx context.Context,
	argv []string,
	workingDirectory string,
	environment []string,
	standardInput []byte,
) (stdout, stderr []byte, exitCode int, err error)

type ResolverOptions struct {
	Executable       string
	Run              OutputRunner
	LoginShell       func() (string, error)
	Nonce            func() (string, error)
	WorkingDirectory string
	Environment      []string
	// ProtectedNames extends kwt's built-in credential environment names.
	ProtectedNames []string
}

type Resolver struct {
	executable       string
	run              OutputRunner
	loginShell       func() (string, error)
	nonce            func() (string, error)
	workingDirectory string
	environment      []string
}

func NewResolver(options ResolverOptions) *Resolver {
	executable := options.Executable
	if executable == "" {
		executable = "ssh"
	}
	run := options.Run
	if run == nil {
		run = runOutput
	}
	nonce := options.Nonce
	if nonce == nil {
		nonce = randomNonce
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	protectedNames := append(credentials.ProtectedNames(nil), options.ProtectedNames...)
	environment = credentials.StripEnvironment(environment, protectedNames)
	workingDirectory := options.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory, _ = os.Getwd()
	}
	return &Resolver{
		executable:       executable,
		run:              run,
		loginShell:       options.LoginShell,
		nonce:            nonce,
		workingDirectory: workingDirectory,
		environment:      append([]string(nil), environment...),
	}
}

func (r *Resolver) Resolve(ctx context.Context, request ResolveRequest) (routeObservation, error) {
	endpoint := request.Target.openSSH()
	if err := openssh.ValidateTarget(endpoint); err != nil {
		return routeObservation{}, invalidTarget(err)
	}
	identityFiles := make(map[openssh.Target][]string)
	route, err := openssh.ResolveRoute(
		ctx,
		endpoint,
		func(ctx context.Context, target openssh.Target) (openssh.EffectiveConfig, error) {
			config, err := r.resolveConfig(ctx, target, nil)
			if err != nil {
				return openssh.EffectiveConfig{}, err
			}
			sentinel, err := r.identitySentinel()
			if err != nil {
				return openssh.EffectiveConfig{}, err
			}
			identityConfig, err := r.resolveConfig(
				ctx,
				target,
				[]string{"-o", "IdentityFile=" + sentinel},
			)
			if err != nil {
				return openssh.EffectiveConfig{}, err
			}
			identityFiles[target], err = configuredIdentityFiles(identityConfig, sentinel)
			if err != nil {
				return openssh.EffectiveConfig{}, err
			}
			return config, nil
		},
	)
	if err != nil {
		var routeErr *openssh.RouteError
		if errors.As(err, &routeErr) {
			return routeObservation{}, routeUnreviewable(err)
		}
		return routeObservation{}, resolutionFailed(err)
	}
	return routeObservation{route: route, identityFiles: identityFiles}, nil
}

func (r *Resolver) identitySentinel() (string, error) {
	nonce, err := r.nonce()
	if err != nil {
		return "", err
	}
	if nonce == "" || strings.ContainsAny(nonce, "\x00\r\n") {
		return "", errors.New("invalid SSH identity probe nonce")
	}
	return "kwt-identity-probe-" + nonce, nil
}

func configuredIdentityFiles(config openssh.EffectiveConfig, sentinel string) ([]string, error) {
	result := make([]string, 0)
	foundSentinel := false
	foundIdentity := false
	for _, option := range config.Options {
		if !strings.EqualFold(option.Name, "identityfile") {
			continue
		}
		foundIdentity = true
		if !foundSentinel && option.Value == sentinel {
			foundSentinel = true
			continue
		}
		result = append(result, option.Value)
	}
	if foundIdentity && !foundSentinel {
		return nil, errors.New("SSH configuration omitted identity probe sentinel")
	}
	return result, nil
}

func randomNonce() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func runOutput(
	ctx context.Context,
	argv []string,
	workingDirectory string,
	environment []string,
	standardInput []byte,
) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, -1, errors.New("empty process arguments")
	}
	executable, err := resolveExecutable(argv[0], environment, workingDirectory)
	if err != nil {
		return nil, nil, -1, err
	}
	processContext, cancelProcess := context.WithCancelCause(ctx)
	defer cancelProcess(nil)
	command := exec.CommandContext(processContext, executable, argv[1:]...)
	command.Dir = workingDirectory
	command.Env = environment
	command.Stdin = bytes.NewReader(standardInput)
	var stdout, stderr []byte
	command.Stdout = byteSliceWriter{
		target: &stdout, limit: resolverOutputLimit, cancel: cancelProcess,
	}
	command.Stderr = byteSliceWriter{
		target: &stderr, limit: resolverOutputLimit, cancel: cancelProcess,
	}
	err = runResolverCommand(command)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	} else if processErr := context.Cause(processContext); processErr != nil {
		err = errors.Join(processErr, err)
	}
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	return stdout, stderr, exitCode, err
}

type byteSliceWriter struct {
	target *[]byte
	limit  int
	cancel context.CancelCauseFunc
}

func (w byteSliceWriter) Write(value []byte) (int, error) {
	remaining := w.limit - len(*w.target)
	if remaining < len(value) {
		if remaining > 0 {
			*w.target = append(*w.target, value[:remaining]...)
		}
		w.cancel(errResolverOutputLimit)
		return max(remaining, 0), errResolverOutputLimit
	}
	*w.target = append(*w.target, value...)
	return len(value), nil
}
