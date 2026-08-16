//go:build windows

package linkterm

import (
	"context"
	"os/exec"
	"sync"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

type windowsTerminal struct {
	cpty *conpty.ConPty
	once sync.Once
}

func startTerminal(cmd *exec.Cmd) (Terminal, error) {
	opts := []conpty.ConPtyOption{
		conpty.ConPtyEnv(cmd.Env),
		conpty.ConPtyDimensions(80, 24),
	}
	if cmd.Dir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(cmd.Dir))
	}

	cpty, err := conpty.Start(windows.ComposeCommandLine(cmd.Args), opts...)
	if err != nil {
		return nil, err
	}
	return &windowsTerminal{cpty: cpty}, nil
}

func (t *windowsTerminal) Read(p []byte) (int, error) {
	return t.cpty.Read(p)
}

func (t *windowsTerminal) Write(p []byte) (int, error) {
	return t.cpty.Write(p)
}

func (t *windowsTerminal) Close() error {
	var err error
	t.once.Do(func() {
		err = t.cpty.Close()
	})
	return err
}

func (t *windowsTerminal) Resize(cols, rows uint16) error {
	return t.cpty.Resize(int(cols), int(rows))
}

func (t *windowsTerminal) Wait() error {
	_, err := t.cpty.Wait(context.Background())
	return err
}
