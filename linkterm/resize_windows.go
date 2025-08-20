//go:build windows

package linkterm

import (
	"os"
	"time"

	"golang.org/x/term"
)

func setupResizeHandler() chan os.Signal {
	sigwinchCh := make(chan os.Signal, 1)

	// On Windows, poll for terminal size changes
	go func() {
		var lastWidth, lastHeight int
		lastWidth, lastHeight, _ = term.GetSize(int(os.Stdin.Fd()))
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			width, height, err := term.GetSize(int(os.Stdin.Fd()))
			if err != nil {
				continue
			}
			if width != lastWidth || height != lastHeight {
				lastWidth, lastHeight = width, height
				sigwinchCh <- os.Interrupt // Use Interrupt as a dummy signal
			}
		}
	}()

	return sigwinchCh
}
