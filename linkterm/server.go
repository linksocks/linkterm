package linkterm

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all connections
	},
}

// Server represents a terminal server
type Server struct {
	Port      int
	Host      string
	ShellPath string
	ShellArgs []string
	logger    zerolog.Logger
}

// NewServer creates a new terminal server with the specified port
func NewServer(port int, host string, shellPath string, shellArgs ...string) *Server {
	if shellPath == "" {
		shellPath = "/bin/bash"
	}

	if host == "" {
		host = "localhost"
	}

	return &Server{
		Port:      port,
		Host:      host,
		ShellPath: shellPath,
		ShellArgs: shellArgs,
		logger:    zerolog.Nop(), // Default no-op logger
	}
}

// SetLogger sets the logger for the server
func (s *Server) SetLogger(logger zerolog.Logger) {
	s.logger = logger
}

// Start starts the terminal server
func (s *Server) Start() error {
	http.HandleFunc("/terminal", s.handleTerminal)

	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)
	s.logger.Info().Str("addr", addr).Msg("Started WebSocket terminal server")
	return http.ListenAndServe(addr, nil)
}

// getClientIP extracts the real client IP from headers or remote address
func getClientIP(r *http.Request) string {
	// Check Cloudflare headers first
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}

	if ip := r.Header.Get("CF-Connecting-IPv6"); ip != "" {
		return ip
	}

	// Check X-Forwarded-For header
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		// X-Forwarded-For can contain multiple IPs, the first one is the client
		ips := strings.Split(forwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Fall back to remote address without port
	remoteAddr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}

	return remoteAddr
}

// handleTerminal handles the terminal WebSocket connection
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	// Get the client IP for logging
	clientIP := getClientIP(r)
	userAgent := r.UserAgent()
	if userAgent == "" {
		userAgent = "Unknown"
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error().Str("clientIP", clientIP).Err(err).Msg("Error upgrading to WebSocket")
		return
	}
	defer conn.Close()

	// Record connection start time
	startTime := time.Now()
	s.logger.Info().Str("clientIP", clientIP).Str("userAgent", userAgent).Msg("Client connected")

	// Create a new command
	cmd := exec.Command(s.ShellPath, s.ShellArgs...)
	cmd.Env = os.Environ()

	// Start the command with a pty
	ptmx, err := pty.Start(cmd)
	if err != nil {
		s.logger.Error().Str("clientIP", clientIP).Err(err).Msg("Error starting pty")
		return
	}

	// Create a clean shutdown function
	closeSession := func() {
		ptmx.Close()
		// Send terminal process termination signal
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
			// Wait for process to exit or force kill after a brief period
			done := make(chan struct{})
			go func() {
				cmd.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Process exited cleanly
			case <-time.After(time.Second):
				// Force kill if it doesn't respond
				cmd.Process.Kill()
			}
		}

		// Calculate session duration
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

		s.logger.Info().Str("clientIP", clientIP).Str("duration", durationStr).Msg("Session ended")
	}
	defer closeSession()

	// Channel to coordinate goroutine termination
	done := make(chan struct{})
	defer close(done)

	// Set up error handling that doesn't spam the logs
	isClosing := false

	// Handle terminal resize and input
	go func() {
		for {
			messageType, p, err := conn.ReadMessage()
			if err != nil {
				if !isClosing {
					if websocket.IsUnexpectedCloseError(err) {
						s.logger.Info().Str("clientIP", clientIP).Msg("Client disconnected unexpectedly")
					} else if !strings.Contains(err.Error(), "use of closed") {
						s.logger.Error().Str("clientIP", clientIP).Err(err).Msg("Error reading from client")
					}
					isClosing = true
				}
				return
			}

			if messageType == websocket.TextMessage {
				// Message format: "resize:cols:rows"
				if len(p) > 7 && string(p[0:7]) == "resize:" {
					parts := strings.Split(string(p[7:]), ":")
					if len(parts) == 2 {
						cols, err1 := strconv.Atoi(parts[0])
						rows, err2 := strconv.Atoi(parts[1])

						if err1 == nil && err2 == nil && cols > 0 && rows > 0 {
							if err := pty.Setsize(ptmx, &pty.Winsize{
								Cols: uint16(cols),
								Rows: uint16(rows),
							}); err != nil {
								s.logger.Error().Err(err).Msg("Error resizing pty")
							}
						}
					}
				} else {
					// Write input to the PTY
					_, _ = ptmx.Write(p)
				}
			}
		}
	}()

	// Copy output from the PTY to the WebSocket
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				if err != io.EOF && !isClosing && !strings.Contains(err.Error(), "input/output error") {
					s.logger.Error().Err(err).Msg("Error reading from PTY")
				}
				break
			}

			err = conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			if err != nil {
				if !isClosing && !strings.Contains(err.Error(), "use of closed") {
					s.logger.Error().Str("clientIP", clientIP).Err(err).Msg("Error writing to WebSocket client")
				}
				isClosing = true
				break
			}
		}
	}()

	// Wait for the process to end
	go func() {
		cmd.Wait()
		// Gracefully close the WebSocket connection when the terminal exits
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Terminal session ended")
		// Ignore errors during close, as the connection might already be gone
		conn.WriteMessage(websocket.CloseMessage, closeMsg)
		isClosing = true
	}()

	wg.Wait()
}
