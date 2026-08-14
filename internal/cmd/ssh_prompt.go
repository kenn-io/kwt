package cmd

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"

	"github.com/muesli/cancelreader"
)

func readTerminalPassword(
	ctx context.Context,
	terminal *os.File,
) (value string, returnErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	reader, err := newTerminalPasswordReader(terminal)
	if err != nil {
		return "", err
	}
	defer func() {
		returnErr = errors.Join(returnErr, reader.Close())
	}()

	result := make(chan sshPromptRead, 1)
	go func() {
		line, readErr := bufio.NewReader(reader).ReadString('\n')
		result <- sshPromptRead{
			value: strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"),
			err:   readErr,
		}
	}()
	select {
	case response := <-result:
		return response.value, response.err
	case <-ctx.Done():
		if reader.Cancel() {
			<-result
		}
		return "", ctx.Err()
	}
}

type terminalPasswordReader interface {
	cancelreader.CancelReader
}
