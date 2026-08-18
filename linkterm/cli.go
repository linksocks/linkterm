package linkterm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/linksocks/linksocks/linksocks"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

var (
	// Common flags
	debugCount int

	// Server flags
	serverPort int
	serverHost string
	shellPath  string

	// Client flags
	clientURL string

	// TUI flag
	tuiMode bool

	// LinkSocks flags
	linksocksToken string
	linksocksURL   string

	// Proxy flag
	proxyURL string
)

// initLogging sets up zerolog with appropriate level
func initLogging(debug int) zerolog.Logger {
	// Set global log level based on debug count
	switch debug {
	case 0:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case 1:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	}

	// Create synchronized console writer
	output := zerolog.ConsoleWriter{
		Out:        zerolog.SyncWriter(os.Stdout),
		TimeFormat: time.RFC3339,
	}

	// Return configured logger
	return zerolog.New(output).With().Timestamp().Logger()
}

// RunCLI runs the command line interface for the terminal server and client
func RunCLI() {
	rootCmd := &cobra.Command{
		Use:   "linkterm",
		Short: "WebSocket Terminal client/server",
		Long:  "A terminal over WebSocket with proxy and tunneling capabilities",
	}

	// Server command
	serverCmd := &cobra.Command{
		Use:   "server",
		Short: "Run in server mode",
		Run:   runServer,
	}

	// Client command
	clientCmd := &cobra.Command{
		Use:   "client",
		Short: "Run in client mode",
		Run:   runClient,
	}

	// Add flags to server command
	serverCmd.Flags().IntVarP(&serverPort, "port", "P", 8080, "Port to listen on")
	serverCmd.Flags().StringVarP(&serverHost, "host", "H", "localhost", "Host address to bind to")
	serverCmd.Flags().StringVarP(&shellPath, "shell", "s", "", "Shell to use")
	serverCmd.Flags().CountVarP(&debugCount, "debug", "d", "Debug level (-d=debug, -dd=trace)")
	serverCmd.Flags().StringVarP(&linksocksToken, "token", "t", "anonymous", "LinkSocks token for intranet penetration")
	serverCmd.Flags().StringVarP(&linksocksURL, "linksocks-url", "U", "https://l.zetx.tech", "LinkSocks server URL")

	// Add flags to client command
	clientCmd.Flags().StringVarP(&clientURL, "url", "u", "ws://localhost:8080", "URL to connect to (e.g. example.com or ws://example.com:8080/terminal)")
	clientCmd.Flags().CountVarP(&debugCount, "debug", "d", "Debug level (-d=debug, -dd=trace)")
	clientCmd.Flags().StringVarP(&linksocksToken, "token", "t", "anonymous", "LinkSocks token for intranet penetration")
	clientCmd.Flags().StringVarP(&linksocksURL, "linksocks-url", "U", "https://l.zetx.tech", "LinkSocks server URL")
	clientCmd.Flags().StringVarP(&proxyURL, "proxy", "x", "", "Proxy URL (e.g. socks5://user:pass@host:port or http://user:pass@host:port)")
	clientCmd.Flags().BoolVar(&tuiMode, "tui", false, "Use the tmux-like TUI client (content area + status bar + toggleable log panel)")

	// Add commands to root command
	rootCmd.AddCommand(serverCmd, clientCmd)

	// Execute the root command
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runServer(cmd *cobra.Command, args []string) {
	// Initialize logger with the specified debug level
	logger := initLogging(debugCount)

	if shellPath == "" {
		// Try to detect the default shell
		shellPath = os.Getenv("SHELL")
		if shellPath == "" {
			if runtime.GOOS == "windows" {
				// Fall back to cmd.exe on Windows
				shellPath = "cmd.exe"
			} else if _, err := exec.LookPath("bash"); err == nil {
				shellPath = "bash"
			} else if _, err := exec.LookPath("sh"); err == nil {
				shellPath = "sh"
			} else {
				logger.Error().Msg("Could not detect a shell to use")
				os.Exit(1)
			}
		}
	}

	server := NewServer(serverPort, serverHost, shellPath)
	server.SetLogger(logger)

	// Start LinkSocks client if token is provided
	if linksocksToken != "" {
		logger.Info().Str("url", linksocksURL).Str("token", linksocksToken).Msg("Starting LinkSocks connection")
		go runTunnel(cmd.Context(), linksocksToken, logger)
	}

	logger.Info().Int("port", serverPort).Str("shell", shellPath).Msg("Starting terminal server")
	if err := server.Start(); err != nil {
		logger.Error().Err(err).Msg("Server error")
		os.Exit(1)
	}
}

// runTunnel maintains the reverse LinkSocks tunnel across reconnects.
// It starts with the configured token (usually "anonymous"), remembers the
// token assigned by the server, and reconnects with it first so the same
// relay is reused within the server's grace period. If that fails the relay
// was garbage-collected, so it falls back to a fresh anonymous connection.
func runTunnel(ctx context.Context, token string, logger zerolog.Logger) {
	currentToken := token
	var connectorToken string

	for {
		if ctx.Err() != nil {
			return
		}

		clientOpt := linksocks.DefaultClientOption().
			WithWSURL(linksocksURL).
			WithReverse(true).
			WithSocksHost("127.0.0.1").
			WithSocksPort(0).
			WithSocksWaitServer(true).
			WithLogger(logger)

		wsClient := linksocks.NewLinkSocksClient(currentToken, clientOpt)

		err := wsClient.WaitReady(ctx, 0)
		if err != nil {
			wsClient.Close()
			if currentToken != "anonymous" {
				// Either the user-supplied token was rejected, or the token
				// previously assigned by the relay no longer works (relay was
				// garbage-collected or tombstoned). Fall back to a fresh
				// anonymous provider so the tunnel stays up with a new relay.
				logger.Warn().Err(err).Str("token", currentToken).
					Msg("LinkSocks connect with token failed, falling back to anonymous")
				currentToken = "anonymous"
				continue
			}
			logger.Warn().Err(err).Msg("LinkSocks connection failed, retrying in 5s")
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}

		// Remember the token assigned by the server so the next reconnect
		// targets the same relay while it is still alive.
		if serverToken := wsClient.GetServerToken(); serverToken != "" {
			currentToken = serverToken
			logger.Info().Str("serverToken", serverToken).Msg("LinkSocks server assigned a relay token")
		}

		// Reuse the same connector token across reconnects: it stays valid
		// while the relay is unchanged, and re-registering it on the same
		// relay is idempotent.
		connectorID, err := registerConnector(wsClient, connectorToken, logger)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to add connector token")
			wsClient.Close()
			if strings.Contains(strings.ToLower(err.Error()), "already exists") {
				// The connector token is bound to another relay, so use a fresh one
				connectorToken = ""
			}
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		connectorToken = connectorID

		logger.Info().Str("connectorID", connectorID).Msg("Connected successfully to LinkSocks server")

		// Wait for the connection to drop, then reconnect
		disconnected := wsClient.DisconnectedChan()
		select {
		case <-disconnected:
			logger.Warn().Msg("LinkSocks connection lost, reconnecting")
		case <-ctx.Done():
			wsClient.Close()
			return
		}
		wsClient.Close()
	}
}

// registerConnector registers a connector token and returns it.
// An empty connector token makes the server generate a random one.
func registerConnector(wsClient *linksocks.LinkSocksClient, connectorToken string, logger zerolog.Logger) (string, error) {
	connectorID, err := wsClient.AddConnector(connectorToken)
	if err != nil {
		return "", err
	}
	return connectorID, nil
}

// sleepCtx sleeps for d unless the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func runClient(cmd *cobra.Command, args []string) {
	// Initialize logger with the specified debug level
	logger := initLogging(debugCount)

	// Check if both proxy and linksocks are set
	if proxyURL != "" && linksocksToken != "" {
		logger.Error().Msg("Cannot use both proxy (-x) and LinkSocks token (-t) at the same time")
		os.Exit(1)
	}

	var customDialer *websocket.Dialer
	var wsocksLocalPort int
	// rttFn reports the link RTT when connected via linksocks; nil otherwise.
	var rttFn func() time.Duration

	// Start LinkSocks client if token is provided
	if linksocksToken != "" {
		logger.Info().Str("token", linksocksToken).Str("url", linksocksURL).Msg("Starting LinkSocks client")

		// Find a random available port on localhost
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			logger.Error().Err(err).Msg("Failed to find available port")
			os.Exit(1)
		}

		// Get the port assigned by the system
		wsocksLocalPort = listener.Addr().(*net.TCPAddr).Port
		listener.Close()

		clientOpt := linksocks.DefaultClientOption().
			WithWSURL(linksocksURL).
			WithSocksHost("127.0.0.1").
			WithSocksPort(wsocksLocalPort).
			WithSocksWaitServer(true).
			WithReconnect(true).
			WithLogger(logger)

		wsClient := linksocks.NewLinkSocksClient(linksocksToken, clientOpt)
		defer wsClient.Close()

		err = wsClient.WaitReady(cmd.Context(), 0)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to connect to linksocks server")
			os.Exit(1)
		} else {
			logger.Info().Msg("Connected successfully to LinkSocks server")
		}
		rttFn = wsClient.GetRTT

		// Configure WebSocket dialer to use LinkSocks SOCKS5 proxy
		customDialer = &websocket.Dialer{
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", wsocksLocalPort))
			},
			HandshakeTimeout: 10 * time.Second,
		}
	} else if proxyURL != "" {
		// Configure WebSocket dialer to use the provided proxy
		proxyURLParsed, err := url.Parse(proxyURL)
		if err != nil {
			logger.Error().Err(err).Str("proxy", proxyURL).Msg("Invalid proxy URL")
			os.Exit(1)
		}

		logger.Info().Str("proxy", proxyURL).Msg("Using proxy")

		customDialer = &websocket.Dialer{
			Proxy:            http.ProxyURL(proxyURLParsed),
			HandshakeTimeout: 10 * time.Second,
		}
	}

	termClient := NewClient(clientURL)
	termClient.SetLogger(logger)
	if customDialer != nil {
		termClient.SetCustomDialer(customDialer)
	}

	if tuiMode {
		// tmux-like TUI: content area renders the remote terminal, the
		// bottom status bar shows host / latency, F2 toggles the log panel.
		t := newTUI(termClient.URL, customDialer, rttFn)
		// route all logs into the TUI log panel instead of stdout (stdout is
		// owned by the TUI here).
		level := logger.GetLevel()
		logger = zerolog.New(zerolog.ConsoleWriter{
			Out: t.ring, NoColor: true, TimeFormat: time.RFC3339,
		}).Level(level).With().Timestamp().Logger()
		t.ring.Logf("LinkTerm TUI — connecting to %s", termClient.URL)
		if err := t.Run(); err != nil {
			logger.Error().Err(err).Msg("TUI error")
			os.Exit(1)
		}
		return
	}

	if err := termClient.Connect(); err != nil {
		logger.Error().Err(err).Msg("Connection error")
		os.Exit(1)
	}
}
