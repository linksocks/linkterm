package linkterm

import "io"

// Terminal is a platform-independent pseudo-terminal attached to a command.
type Terminal interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() error
}
