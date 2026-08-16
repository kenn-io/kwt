package ssh

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/service"
)

const (
	askpassHandleEnvironment  = "KWT_SSH_ASKPASS_HANDLE"
	defaultPromptTimeout      = 2 * time.Minute
	defaultMaxPromptRounds    = 32
	askpassIOTimeout          = 5 * time.Second
	askpassHelperTimeout      = 5 * time.Minute
	maxAskpassHandleBytes     = 4 << 10
	maxAskpassPromptHintBytes = 64
	maxAskpassConnections     = 8
)

var askpassProtocolMagic = [6]byte{'K', 'W', 'T', 'A', 'P', '2'} //nolint:gochecknoglobals

type InteractiveVersionPolicy interface {
	RequireInteractive(context.Context) error
}

type AskpassOptions struct {
	Version        InteractiveVersionPolicy
	Executable     string
	Environment    []string
	ProtectedNames []string
	PromptTimeout  time.Duration
	MaxRounds      int
	Describe       func(string, string) (service.OperationPrompt, error)
	Prompt         func(context.Context, service.OperationPrompt) (string, error)
}

type Askpass struct {
	directory       string
	environment     []string
	listener        net.Listener
	cleanupEndpoint func() error
	handle          askpassHandle
	options         AskpassOptions
	context         context.Context
	cancel          context.CancelFunc

	mu     sync.Mutex
	rounds int
	err    error
	once   sync.Once
	wg     sync.WaitGroup
	limit  chan struct{}
}

type askpassHandle struct {
	Network string `json:"network"`
	Address string `json:"address"`
	Token   string `json:"token"`
}

func NewAskpass(
	ctx context.Context,
	privateRoot string,
	options AskpassOptions,
) (*Askpass, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Version == nil {
		return nil, errors.New("OpenSSH version policy is unavailable")
	}
	if err := options.Version.RequireInteractive(ctx); err != nil {
		return nil, err
	}
	if options.Executable == "" {
		var err error
		options.Executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(options.Executable) {
		return nil, errors.New("askpass executable must be absolute")
	}
	if options.PromptTimeout <= 0 {
		options.PromptTimeout = defaultPromptTimeout
	}
	if options.MaxRounds <= 0 {
		options.MaxRounds = defaultMaxPromptRounds
	}
	if err := safefileio.EnsurePrivateDir(privateRoot); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(privateRoot, "prompt-")
	if err != nil {
		return nil, err
	}
	cleanupDirectory := func() { _ = os.RemoveAll(directory) }
	if err := safefileio.EnsurePrivateDir(directory); err != nil {
		cleanupDirectory()
		return nil, err
	}
	listener, endpoint, cleanupEndpoint, err := listenAskpass(directory)
	if err != nil {
		cleanupDirectory()
		return nil, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = listener.Close()
		_ = cleanupEndpoint()
		cleanupDirectory()
		return nil, err
	}
	handle := askpassHandle{
		Network: endpoint.Network,
		Address: endpoint.Address,
		Token:   base64.RawURLEncoding.EncodeToString(tokenBytes),
	}
	encodedHandle, err := encodeAskpassHandle(handle)
	if err != nil {
		_ = listener.Close()
		_ = cleanupEndpoint()
		cleanupDirectory()
		return nil, err
	}
	childContext, cancel := context.WithCancel(ctx)
	transport := &Askpass{
		directory:       directory,
		listener:        listener,
		cleanupEndpoint: cleanupEndpoint,
		handle:          handle,
		options:         options,
		context:         childContext,
		cancel:          cancel,
		limit:           make(chan struct{}, maxAskpassConnections),
	}
	transport.environment = askpassEnvironment(options, encodedHandle)
	transport.wg.Add(1)
	go transport.serve()
	go func() {
		<-childContext.Done()
		_ = listener.Close()
	}()
	return transport, nil
}

func askpassEnvironment(options AskpassOptions, handle string) []string {
	protected := append([]string(nil), options.ProtectedNames...)
	protected = append(protected,
		"SSH_ASKPASS",
		"SSH_ASKPASS_PROMPT",
		"SSH_ASKPASS_REQUIRE",
		"DISPLAY",
		askpassHandleEnvironment,
	)
	environment := credentials.StripEnvironment(options.Environment, protected)
	return append(environment,
		"SSH_ASKPASS="+options.Executable,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=kwt-askpass",
		askpassHandleEnvironment+"="+handle,
	)
}

func (a *Askpass) Environment() []string {
	return append([]string(nil), a.environment...)
}

func (a *Askpass) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *Askpass) Close() error {
	var cleanupErr error
	a.once.Do(func() {
		a.cancel()
		cleanupErr = a.listener.Close()
		if errors.Is(cleanupErr, net.ErrClosed) {
			cleanupErr = nil
		}
		a.wg.Wait()
		cleanupErr = errors.Join(
			cleanupErr,
			a.cleanupEndpoint(),
			os.RemoveAll(a.directory),
		)
	})
	return cleanupErr
}

func (a *Askpass) serve() {
	defer a.wg.Done()
	for {
		connection, err := a.listener.Accept()
		if err != nil {
			return
		}
		select {
		case a.limit <- struct{}{}:
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				defer func() { <-a.limit }()
				a.handleConnection(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (a *Askpass) handleConnection(connection net.Conn) {
	defer connection.Close() //nolint:errcheck // The protocol result is already complete.
	_ = connection.SetReadDeadline(time.Now().Add(askpassIOTimeout))
	magic := make([]byte, len(askpassProtocolMagic))
	if _, err := io.ReadFull(connection, magic); err != nil || !bytesEqual(magic, askpassProtocolMagic[:]) {
		return
	}
	token, err := readAskpassFrame(connection, maxAskpassHandleBytes)
	if err != nil || !bytesEqual([]byte(token), []byte(a.handle.Token)) {
		return
	}
	promptMessage, err := readAskpassFrame(connection, service.MaxOperationMessageBytes)
	if err != nil {
		return
	}
	promptHint, err := readAskpassFrame(connection, maxAskpassPromptHintBytes)
	if err != nil {
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	response, promptErr := a.prompt(promptMessage, promptHint)
	_ = connection.SetWriteDeadline(time.Now().Add(askpassIOTimeout))
	if promptErr != nil {
		a.setError(promptErr)
		_ = writeAskpassResponse(connection, 1, "")
		return
	}
	if err := writeAskpassResponse(connection, 0, response); err != nil {
		return
	}
}

func (a *Askpass) prompt(message, hint string) (string, error) {
	a.mu.Lock()
	a.rounds++
	round := a.rounds
	a.mu.Unlock()
	if round > a.options.MaxRounds {
		return "", connectionFailed(errors.New("SSH prompt round limit exceeded"))
	}
	prompt := describeSSHPrompt(message, hint)
	prompt.Message = message
	if a.options.Describe != nil {
		described, err := a.options.Describe(message, hint)
		if err != nil {
			return "", err
		}
		prompt = described
		prompt.Message = message
	}
	if a.options.Prompt == nil {
		return "", service.NewError(
			service.SSHInteractionRequired,
			"SSH interaction is required",
			false,
			nil,
			nil,
		)
	}
	promptContext, cancel := context.WithTimeout(a.context, a.options.PromptTimeout)
	defer cancel()
	deadline, _ := promptContext.Deadline()
	prompt.Deadline = &deadline
	response, err := a.options.Prompt(promptContext, prompt)
	if errors.Is(promptContext.Err(), context.DeadlineExceeded) {
		return "", service.NewError(
			service.SSHPromptTimedOut,
			"SSH prompt timed out",
			false,
			nil,
			promptContext.Err(),
		)
	}
	if err != nil {
		return "", err
	}
	if len(response) > service.MaxOperationResponseBytes {
		return "", service.NewError(
			service.InvalidRequest,
			"SSH prompt response is too large",
			false,
			nil,
			nil,
		)
	}
	return response, nil
}

func (a *Askpass) setError(err error) {
	a.mu.Lock()
	if a.err == nil {
		a.err = err
	}
	a.mu.Unlock()
}

func RunAskpassHelper(arguments, environment []string, output io.Writer) (int, bool) {
	encodedHandle, ok := environmentValue(environment, askpassHandleEnvironment)
	if !ok {
		return 0, false
	}
	if len(arguments) != 2 || len(arguments[1]) > service.MaxOperationMessageBytes {
		return 1, true
	}
	handle, err := decodeAskpassHandle(encodedHandle)
	if err != nil {
		return 1, true
	}
	connection, err := dialAskpass(handle, askpassIOTimeout)
	if err != nil {
		return 1, true
	}
	defer connection.Close() //nolint:errcheck // Helper is exiting.
	_ = connection.SetDeadline(time.Now().Add(askpassHelperTimeout))
	if _, err := connection.Write(askpassProtocolMagic[:]); err != nil {
		return 1, true
	}
	if err := writeAskpassFrame(connection, handle.Token); err != nil {
		return 1, true
	}
	if err := writeAskpassFrame(connection, arguments[1]); err != nil {
		return 1, true
	}
	promptHint, _ := environmentValue(environment, "SSH_ASKPASS_PROMPT")
	if len(promptHint) > maxAskpassPromptHintBytes {
		return 1, true
	}
	if err := writeAskpassFrame(connection, promptHint); err != nil {
		return 1, true
	}
	status := []byte{0}
	if _, err := io.ReadFull(connection, status); err != nil {
		return 1, true
	}
	response, err := readAskpassFrame(connection, service.MaxOperationResponseBytes)
	if err != nil || status[0] != 0 {
		return 1, true
	}
	if _, err := io.WriteString(output, response+"\n"); err != nil {
		return 1, true
	}
	return 0, true
}

func describeSSHPrompt(_ string, hint string) service.OperationPrompt {
	if hint == "confirm" {
		return service.OperationPrompt{Kind: "ssh_host_key", Sensitive: false}
	}
	return service.OperationPrompt{Kind: "ssh_authentication", Sensitive: true}
}

func encodeAskpassHandle(handle askpassHandle) (string, error) {
	encoded, err := json.Marshal(handle)
	if err != nil {
		return "", err
	}
	if len(encoded) > maxAskpassHandleBytes {
		return "", errors.New("askpass handle is too large")
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeAskpassHandle(encoded string) (askpassHandle, error) {
	if len(encoded) > maxAskpassHandleBytes*2 {
		return askpassHandle{}, errors.New("askpass handle is too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return askpassHandle{}, err
	}
	var handle askpassHandle
	if err := json.Unmarshal(data, &handle); err != nil {
		return askpassHandle{}, err
	}
	if handle.Token == "" || handle.Address == "" ||
		handle.Network != "unix" && handle.Network != "npipe" {
		return askpassHandle{}, errors.New("invalid askpass handle")
	}
	return handle, nil
}

func writeAskpassResponse(writer io.Writer, status byte, value string) error {
	if _, err := writer.Write([]byte{status}); err != nil {
		return err
	}
	return writeAskpassFrame(writer, value)
}

func writeAskpassFrame(writer io.Writer, value string) error {
	if len(value) > service.MaxOperationResponseBytes {
		return errors.New("askpass frame is too large")
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(value)))
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := io.WriteString(writer, value)
	return err
}

func readAskpassFrame(reader io.Reader, maximum int) (string, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", err
	}
	size := int(binary.BigEndian.Uint32(header))
	if size < 0 || size > maximum {
		return "", errors.New("askpass frame is too large")
	}
	value := make([]byte, size)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}

func environmentValue(environment []string, name string) (string, bool) {
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}

type askpassEndpoint struct {
	Network string
	Address string
}
