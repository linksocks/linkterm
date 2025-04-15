//go:build !windows

package wsterm

import (
	"os"
	"os/signal"
	"syscall"
)

func setupResizeHandler() chan os.Signal {
	sigwinchCh := make(chan os.Signal, 1)
	signal.Notify(sigwinchCh, syscall.SIGWINCH)
	return sigwinchCh
}
