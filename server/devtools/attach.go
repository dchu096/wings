package devtools

import (
	"context"
	"io"
	"strconv"
	"sync"

	"emperror.dev/errors"
	"github.com/docker/docker/api/types/container"
)

// Session is an active PTY exec session inside the sidecar.
type Session struct {
	ExecID string
	stdin  io.WriteCloser
	stdout io.Reader
	conn   io.Closer
	mgr    *Manager

	mu     sync.Mutex
	closed bool
}

// AttachPTY starts a docker exec TTY in the sidecar and returns a Session.
func (m *Manager) AttachPTY(ctx context.Context) (*Session, error) {
	if err := m.EnsureRunning(ctx); err != nil {
		return nil, err
	}

	shell := m.Shell()
	cfg := container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          shell,
		WorkingDir:   m.Workdir(),
	}

	created, err := m.client.ContainerExecCreate(ctx, m.ContainerName(), cfg)
	if err != nil {
		return nil, errors.Wrap(err, "devtools: exec create failed")
	}

	hijacked, err := m.client.ContainerExecAttach(ctx, created.ID, container.ExecStartOptions{
		Tty: true,
	})
	if err != nil {
		return nil, errors.Wrap(err, "devtools: exec attach failed")
	}

	m.NoteAttach()

	return &Session{
		ExecID: created.ID,
		stdin:  hijacked.Conn,
		stdout: hijacked.Reader,
		conn:   hijacked.Conn,
		mgr:    m,
	}, nil
}

// Stdin returns the writer for terminal input.
func (s *Session) Stdin() io.Writer {
	return s.stdin
}

// Stdout returns the reader for terminal output.
func (s *Session) Stdout() io.Reader {
	return s.stdout
}

// Resize updates the PTY size.
func (s *Session) Resize(ctx context.Context, cols, rows uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("devtools: session closed")
	}
	return s.mgr.client.ContainerExecResize(ctx, s.ExecID, container.ResizeOptions{
		Width:  cols,
		Height: rows,
	})
}

// ResizeFromStrings parses decimal col/row strings.
func (s *Session) ResizeFromStrings(ctx context.Context, cols, rows string) error {
	c, err := strconv.ParseUint(cols, 10, 32)
	if err != nil {
		return errors.New("devtools: invalid cols")
	}
	r, err := strconv.ParseUint(rows, 10, 32)
	if err != nil {
		return errors.New("devtools: invalid rows")
	}
	return s.Resize(ctx, uint(c), uint(r))
}

// Close tears down the exec stream.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.conn != nil {
		err = s.conn.Close()
	}
	s.mgr.NoteDetach()
	return err
}

// CopyStdout pumps stdout to w until EOF or context cancel.
func (s *Session) CopyStdout(ctx context.Context, w io.Writer) error {
	errCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(w, s.stdout)
		errCh <- err
	}()
	select {
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}
