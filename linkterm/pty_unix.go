//go:build !windows

package linkterm

import (
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

type unixTerminal struct {
	ptmx *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func startTerminal(cmd *exec.Cmd) (Terminal, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &unixTerminal{ptmx: ptmx, cmd: cmd}, nil
}

func (t *unixTerminal) Read(p []byte) (int, error) {
	return t.ptmx.Read(p)
}

func (t *unixTerminal) Write(p []byte) (int, error) {
	return t.ptmx.Write(p)
}

func (t *unixTerminal) Close() error {
	var err error
	t.once.Do(func() {
		err = t.ptmx.Close()
	})
	return err
}

func (t *unixTerminal) Resize(cols, rows uint16) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

func (t *unixTerminal) Wait() error {
	return t.cmd.Wait()
}
