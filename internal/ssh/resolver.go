package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"

	"go.kenn.io/kit/openssh"
	"go.kenn.io/kwt/internal/credentials"
)

type OutputRunner func(
	ctx context.Context,
	argv []string,
	environment []string,
	standardInput []byte,
) (stdout, stderr []byte, exitCode int, err error)

type ResolverOptions struct {
	Executable  string
	Run         OutputRunner
	LoginShell  func() (string, error)
	Nonce       func() (string, error)
	Environment []string
	// ProtectedNames extends kwt's built-in credential environment names.
	ProtectedNames []string
}

type Resolver struct {
	executable  string
	run         OutputRunner
	loginShell  func() (string, error)
	nonce       func() (string, error)
	environment []string
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
	return &Resolver{
		executable:  executable,
		run:         run,
		loginShell:  options.LoginShell,
		nonce:       nonce,
		environment: append([]string(nil), environment...),
	}
}

func (r *Resolver) Resolve(ctx context.Context, request ResolveRequest) (routeObservation, error) {
	endpoint := request.Target.openSSH()
	if err := openssh.ValidateTarget(endpoint); err != nil {
		return routeObservation{}, invalidTarget(err)
	}
	route, err := openssh.ResolveRoute(ctx, endpoint, r.resolveConfig)
	if err != nil {
		var routeErr *openssh.RouteError
		if errors.As(err, &routeErr) {
			return routeObservation{}, routeUnreviewable(err)
		}
		return routeObservation{}, resolutionFailed(err)
	}
	return routeObservation{route: route}, nil
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
	environment []string,
	standardInput []byte,
) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, -1, errors.New("empty process arguments")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureResolverCommand(command)
	command.Env = environment
	command.Stdin = bytes.NewReader(standardInput)
	var stdout, stderr []byte
	command.Stdout = byteSliceWriter{target: &stdout}
	command.Stderr = byteSliceWriter{target: &stderr}
	err := command.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
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
}

func (w byteSliceWriter) Write(value []byte) (int, error) {
	*w.target = append(*w.target, value...)
	return len(value), nil
}
