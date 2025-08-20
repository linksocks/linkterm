package linkterm

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"golang.org/x/term"
)

// Client represents a terminal client
type Client struct {
	URL    string
	dialer *websocket.Dialer
	logger zerolog.Logger
}

// NewClient creates a new terminal client
func NewClient(url string) *Client {
	if url == "" {
		url = "ws://localhost/terminal"
	}

	// Handle URL scheme conversion
	if strings.HasPrefix(url, "http://") {
		url = "ws://" + strings.TrimPrefix(url, "http://")
	} else if strings.HasPrefix(url, "https://") {
		url = "wss://" + strings.TrimPrefix(url, "https://")
	} else if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		url = "ws://" + url
	}

	// Parse URL to check path
	parts := strings.SplitN(url, "/", 4)
	if len(parts) == 3 { // scheme://domain
		url = url + "/terminal"
	}

	return &Client{
		URL:    url,
		dialer: websocket.DefaultDialer,
		logger: zerolog.Nop(), // Default no-op logger
	}
}

// SetCustomDialer sets a custom websocket dialer for the client
func (c *Client) SetCustomDialer(dialer *websocket.Dialer) {
	c.dialer = dialer
}

// SetLogger sets the logger for the client
func (c *Client) SetLogger(logger zerolog.Logger) {
	c.logger = logger
}

// Connect connects to the terminal server and starts the terminal session
func (c *Client) Connect() error {
	c.logger.Info().Str("url", c.URL).Msg("Connecting to terminal server")

	// Use custom dialer if set, or the default one
	dialer := c.dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}

	dialer.HandshakeTimeout = 5 * time.Second

	// Set User-Agent header: LinkTerm/{version} {SystemInfo}
	header := make(map[string][]string)
	header["User-Agent"] = []string{fmt.Sprintf("LinkTerm/%s %s", Version, Platform)}

	conn, resp, err := dialer.Dial(c.URL, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("failed to connect to terminal server: HTTP %d - %s", resp.StatusCode, err)
		}
		return fmt.Errorf("failed to connect to terminal server: %w", err)
	}

	// Record connection start time
	startTime := time.Now()
	c.logger.Info().Str("url", c.URL).Msg("Connected to terminal server")

	// Track if disconnected message has been displayed
	var disconnectOnce sync.Once
	var hasDisconnected bool

	// Create a function to handle disconnection with duration
	disconnect := func(reason string) {
		disconnectOnce.Do(func() {
			hasDisconnected = true
			duration := time.Since(startTime)
			hours := int(duration.Hours())
			minutes := int(duration.Minutes()) % 60
			seconds := int(duration.Seconds()) % 60

			// Format duration string
			var durationStr string
			if hours > 0 {
				durationStr = fmt.Sprintf("%d hours, %d minutes, %d seconds", hours, minutes, seconds)
			} else if minutes > 0 {
				durationStr = fmt.Sprintf("%d minutes, %d seconds", minutes, seconds)
			} else {
				durationStr = fmt.Sprintf("%d seconds", seconds)
			}

			// Reset line before printing disconnect message
			fmt.Printf("\r\033[KDisconnected from terminal server after %s (%s)\n", durationStr, reason)
		})
	}

	defer func() {
		// Try to close gracefully
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client disconnected")
		conn.WriteMessage(websocket.CloseMessage, closeMsg)
		conn.Close()

		// Only show disconnect message if we haven't already shown one
		if !hasDisconnected {
			disconnect("client closed")
		}
	}()

	// Handle graceful shutdown on interrupt
	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-interruptChan
		fmt.Println("\nReceived interrupt, disconnecting...")
		// Try to close gracefully
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client disconnected")
		conn.WriteMessage(websocket.CloseMessage, closeMsg)
		conn.Close()
		disconnect("interrupted by user")
		os.Exit(0)
	}()

	// Put the local terminal into raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to put terminal into raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Get terminal size and send it
	width, height, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Printf("Warning: could not get terminal size: %v", err)
	} else {
		resizeMsg := fmt.Sprintf("resize:%d:%d", width, height)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(resizeMsg)); err != nil {
			fmt.Printf("Warning: could not send terminal size: %v", err)
		}
	}

	// Handle terminal resize
	sigwinchCh := setupResizeHandler()

	go func() {
		for range sigwinchCh {
			width, height, err := term.GetSize(int(os.Stdin.Fd()))
			if err != nil {
				continue
			}

			resizeMsg := fmt.Sprintf("resize:%d:%d", width, height)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(resizeMsg)); err != nil {
				if !strings.Contains(err.Error(), "use of closed") {
					fmt.Printf("Warning: could not send terminal size: %v", err)
				}
				return
			}
		}
	}()

	// Set up channels for coordinating exit
	done := make(chan struct{})

	// Send terminal input to WebSocket
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(done)
				return
			}

			err = conn.WriteMessage(websocket.TextMessage, buf[:n])
			if err != nil {
				// Only log if not a normal closure
				if !strings.Contains(err.Error(), "use of closed") &&
					!websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					fmt.Printf("Error writing to WebSocket: %v", err)
				}
				close(done)
				return
			}
		}
	}()

	// Receive terminal output from WebSocket
	go func() {
		defer close(done)
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				// Check if it's a normal closure or abnormal
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
					strings.Contains(err.Error(), "use of closed") {
					// Normal close, show normal disconnect message
					disconnect("client closed")
					return
				}

				// Reset terminal and clear the current line to avoid formatting issues
				fmt.Print("\r\033[K\n")
				fmt.Printf("Connection closed: %v", err)
				disconnect("connection error")
				return
			}

			if messageType == websocket.CloseMessage {
				disconnect("server sent close message")
				return
			}

			_, err = os.Stdout.Write(message)
			if err != nil {
				fmt.Printf("Error writing to stdout: %v", err)
				disconnect("output error")
				return
			}
		}
	}()

	// Wait for done signal
	<-done
	return nil
}
