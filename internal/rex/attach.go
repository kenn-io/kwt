package rex

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Data connection handshake: HELLO is 0x01 and the ticket. WELCOME is 0x81,
// a 16-byte resume token, 3 zero bytes, a u32 grace period, and a u64 nonce
// (32 bytes, little endian). REJECT is 0x82 and a reason byte.
const (
	dataHello   = 0x01
	dataWelcome = 0x81
	dataReject  = 0x82
)

// clearScreen homes the cursor and clears the screen and scrollback before
// the first paint.
const clearScreen = "\x1b[H\x1b[2J\x1b[3J"

// Rex sizes a block for its owner, and another viewer such as Rex.app may
// take it. Typing takes the block back at most this often.
const retakeInterval = 500 * time.Millisecond

// Size is a terminal size in character cells.
type Size struct {
	Columns int
	Rows    int
}

// AttachOptions selects the terminal and the local terminal that shows it.
type AttachOptions struct {
	SessionID string
	BlockID   string
	In        io.Reader
	Out       io.Writer
	// Size is the local terminal's size when the attach starts.
	Size Size
	// Sizes delivers later sizes of the local terminal.
	Sizes <-chan Size
}

// Attach shows one Rex terminal on opts.Out and types opts.In into it. It
// returns the program's exit status when the program ends, or nil when the
// attach ends first: opts.In closed or ctx was canceled. Detaching never
// stops the program.
func Attach(ctx context.Context, socket string, opts AttachOptions) (*int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, err := Dial(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	a := &attachment{client: client, session: opts.SessionID, block: opts.BlockID}
	err = client.Call(ctx, "session.attach", map[string]string{"session_id": a.session}, nil)
	if err != nil && !IsCode(err, "conflict") {
		return nil, err
	}
	// Take the block and set its size first, so the snapshot and the first
	// paint already have this terminal's size.
	if err := a.resize(ctx, opts.Size); err != nil {
		return nil, err
	}
	data, nonce, err := a.openData(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = data.CloseNow() }()

	type result struct {
		exit *int
		err  error
	}
	done := make(chan result, 3)
	finish := func(exit *int, err error) {
		done <- result{exit: exit, err: err}
	}
	go func() { finish(a.stream(ctx, data, nonce, opts.Out)) }()
	go func() { finish(nil, a.typeInput(ctx, opts.In)) }()
	if opts.Sizes != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case size := <-opts.Sizes:
					if err := a.resize(ctx, size); err != nil {
						finish(nil, err)
						return
					}
				}
			}
		}()
	}
	select {
	case r := <-done:
		if errors.Is(r.err, context.Canceled) && ctx.Err() != nil {
			return r.exit, nil
		}
		return r.exit, r.err
	case <-ctx.Done():
		return nil, nil
	}
}

type attachment struct {
	client  *Client
	session string
	block   string

	mu      sync.Mutex
	takenAt time.Time
}

func (a *attachment) take(ctx context.Context) error {
	a.mu.Lock()
	a.takenAt = time.Now()
	a.mu.Unlock()
	return a.client.Call(ctx, "session.own_block", map[string]any{
		"session_id": a.session, "block_ids": []string{a.block}, "mode": "take",
	}, nil)
}

func (a *attachment) resize(ctx context.Context, size Size) error {
	if size.Columns < 1 || size.Rows < 1 {
		return nil
	}
	if err := a.take(ctx); err != nil {
		return err
	}
	return a.client.BlockCall(ctx, a.session, a.block, "resize",
		map[string]int{"columns": size.Columns, "rows": size.Rows}, nil)
}

func (a *attachment) openData(ctx context.Context, socket string) (*websocket.Conn, uint64, error) {
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	err := a.client.Call(ctx, "data.connect", map[string]string{
		"session_id": a.session, "block_id": a.block,
		"data_ref_id": "terminal", "subprotocol": dataProtocol,
	}, &ticket)
	if err != nil {
		return nil, 0, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(ticket.Ticket)
	if err != nil {
		return nil, 0, fmt.Errorf("rex sent an unreadable data ticket: %w", err)
	}
	conn, err := dial(ctx, socket, "/api/data", dataProtocol)
	if err != nil {
		return nil, 0, err
	}
	if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{dataHello}, raw...)); err != nil {
		_ = conn.CloseNow()
		return nil, 0, fmt.Errorf("rex terminal stream: %w", err)
	}
	_, welcome, err := conn.Read(ctx)
	switch {
	case err != nil:
		_ = conn.CloseNow()
		return nil, 0, fmt.Errorf("rex terminal stream: %w", err)
	case len(welcome) == 32 && welcome[0] == dataWelcome:
		return conn, binary.LittleEndian.Uint64(welcome[24:32]), nil
	case len(welcome) >= 1 && welcome[0] == dataReject:
		_ = conn.CloseNow()
		reason := byte(0)
		if len(welcome) > 1 {
			reason = welcome[1]
		}
		return nil, 0, fmt.Errorf("rex refused the terminal stream (reason %#02x)", reason)
	default:
		_ = conn.CloseNow()
		return nil, 0, errors.New("unexpected Rex terminal stream handshake")
	}
}

// stream copies the terminal's output to out. The stream's Ghostty snapshot
// is binary, so the first paint comes from the server's own VT rendering of
// the screen instead.
func (a *attachment) stream(ctx context.Context, conn *websocket.Conn, nonce uint64, out io.Writer) (*int, error) {
	filter := newStreamFilter(nonce)
	painted := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil, nil
			}
			return nil, fmt.Errorf("rex terminal stream: %w", err)
		}
		for _, p := range filter.push(data) {
			switch p.kind {
			case pieceSnapshot:
				continue
			case pieceOutput:
				if _, err := out.Write(p.bytes); err != nil {
					return nil, err
				}
			case pieceMessage:
				if !painted {
					painted = true
					if err := a.paint(ctx, out); err != nil {
						return nil, err
					}
				}
				if p.message.Type == "child_exited" {
					exit := 0
					if p.message.ExitCode != nil {
						exit = *p.message.ExitCode
					}
					return &exit, nil
				}
			}
		}
	}
}

func (a *attachment) paint(ctx context.Context, out io.Writer) error {
	args := map[string]any{"format": "vt"}
	for _, extra := range []string{
		"charsets", "cursor", "hyperlink", "keyboard", "kitty_keyboard", "modes",
		"palette", "protection", "pwd", "scrolling_region", "style", "tabstops",
	} {
		args["extra_"+extra] = true
	}
	var screen struct {
		Content string `json:"content"`
	}
	if err := a.client.BlockCall(ctx, a.session, a.block, "format", args, &screen); err != nil {
		return fmt.Errorf("paint the terminal: %w", err)
	}
	_, err := io.WriteString(out, clearScreen+screen.Content)
	return err
}

// typeInput types what in delivers until in closes.
func (a *attachment) typeInput(ctx context.Context, in io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			a.mu.Lock()
			stale := time.Since(a.takenAt) > retakeInterval
			a.mu.Unlock()
			if stale {
				if err := a.take(ctx); err != nil {
					return err
				}
			}
			text := strings.ToValidUTF8(string(buf[:n]), "�")
			if err := a.client.BlockCall(ctx, a.session, a.block, "write", map[string]string{"data": text}, nil); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
