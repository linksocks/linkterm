package linkterm

import (
	"context"
	"fmt"
	"io"
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

	// Create synchronized console writer (silences noisy library info lines)
	output := zerolog.ConsoleWriter{
		Out:        zerolog.SyncWriter(os.Stdout),
		TimeFormat: time.RFC3339,
	}

	// Return configured logger
	return zerolog.New(quietWriter{output}).With().Timestamp().Logger()
}

// quietWriter drops noisy info-level library messages from the logger output.
// It wraps the final io.Writer, so it filters on ConsoleWriter-formatted text.
// Use -d/-dd to see all messages unfiltered (Debug/Trace bypass the filter).
type quietWriter struct {
	inner io.Writer
}

func (w quietWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.Contains(s, "is connecting to") ||
		strings.Contains(s, "Using proxy from environment") ||
		strings.Contains(s, "Welcome to LinkSocks.js") ||
		strings.Contains(s, "Server ready, latency") {
		return len(p), nil // drop
	}
	return w.inner.Write(p)
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
	serverCmd.Flags().StringVarP(&linksocksToken, "token", "t", "anonymous", "Connector token clients use to reach this terminal (random if omitted)")
	serverCmd.Flags().StringVarP(&linksocksURL, "linksocks-url", "U", "https://l.zetx.tech", "LinkSocks relay server URL")

	// Add flags to client command
	clientCmd.Flags().StringVarP(&clientURL, "url", "u", "ws://localhost:8080", "URL to connect to (e.g. example.com or ws://example.com:8080/terminal)")
	clientCmd.Flags().CountVarP(&debugCount, "debug", "d", "Debug level (-d=debug, -dd=trace)")
	clientCmd.Flags().StringVarP(&linksocksToken, "token", "t", "", "LinkSocks connector token from the server")
	clientCmd.Flags().StringVarP(&linksocksURL, "linksocks-url", "U", "https://l.zetx.tech", "LinkSocks relay server URL")
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

	// Start the LinkSocks tunnel. The server always dials as an anonymous
	// provider and mounts a connector token (random unless -t is given);
	// "anonymous" here is just the sentinel meaning "no custom token".
	tunnelToken := linksocksToken
	if tunnelToken == "" {
		tunnelToken = "anonymous"
	}
	logger.Info().Str("url", linksocksURL).Str("token", tunnelToken).Msg("Starting LinkSocks connection")

	errCh := make(chan error, 2)
	go func() {
		if err := runTunnel(cmd.Context(), tunnelToken, logger); err != nil {
			errCh <- err
		}
	}()
	go func() {
		if err := server.Start(); err != nil {
			errCh <- err
		}
	}()
	if err := <-errCh; err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runTunnel maintains the reverse LinkSocks tunnel across reconnects.
//
// The -t / --token value is the connector token that clients use to reach
// this relay. It is NOT used as the provider dial token. We always dial as
// an anonymous provider so the relay assigns a fresh identity, and reconnect
// with that returned token to keep the same relay. The connector token is
// mounted via AddConnector once per relay session; replayConnectorTokens
// handles re-mounting automatically after reconnect.
//
// Returns nil on clean shutdown (ctx cancelled), or a fatal error when the
// user-specified connector token is rejected by the relay.
func runTunnel(ctx context.Context, token string, logger zerolog.Logger) error {
	currentToken := "anonymous"
	var connectorToken string

	for {
		if ctx.Err() != nil {
			return nil
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
				logger.Warn().Err(err).Str("token", currentToken).
					Msg("LinkSocks reconnect with server token failed, falling back to anonymous")
				currentToken = "anonymous"
				continue
			}
			logger.Warn().Err(err).Msg("LinkSocks connection failed, retrying in 5s")
			if !sleepCtx(ctx, 5*time.Second) {
				return nil
			}
			continue
		}

		// Remember the token assigned by the server so the next reconnect
		// targets the same relay while it is still alive.
		if serverToken := wsClient.GetServerToken(); serverToken != "" {
			currentToken = serverToken
			logger.Info().Str("serverToken", serverToken).Msg("LinkSocks server assigned a relay token")
		}

		// Mount the connector token (once per relay session).
		// replayConnectorTokens handles re-mounting after reconnect.
		if connectorToken == "" {
			mount := ""
			if token != "anonymous" {
				mount = token
			}
			cid, err := registerConnector(wsClient, mount, logger)
			if err != nil {
				// If the user explicitly requested a connector token and it was
				// rejected, the tunnel cannot work with the intended identity.
				// Fail fast so the user knows to use a stronger token.
				if mount != "" {
					logger.Error().Err(err).Str("token", mount).
						Msg("Connector token rejected by the relay; use a stronger token (at least 8 characters, not too simple)")
					wsClient.Close()
					return fmt.Errorf("connector token %q rejected: %w", mount, err)
				}
				// mount == "" (random): should never fail, but retry if it does.
				logger.Error().Err(err).Msg("Failed to add connector token")
				wsClient.Close()
				if !sleepCtx(ctx, 5*time.Second) {
					return nil
				}
				continue
			}
			connectorToken = cid
		}

		logger.Info().Str("connectorID", connectorToken).Msg("Connected successfully to LinkSocks server")

		// Wait for the connection to drop, then reconnect
		disconnected := wsClient.DisconnectedChan()
		select {
		case <-disconnected:
			logger.Warn().Msg("LinkSocks connection lost, reconnecting")
		case <-ctx.Done():
			wsClient.Close()
			return nil
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

	// Build termClient early for URL processing.
	termClient := NewClient(clientURL)

	// If TUI mode, create the TUI early so the log ring is ready before any
	// wsClient is created — this way all linksocks logs land in the panel.
	var tUI *tui
	if tuiMode {
		tUI = newTUI(termClient.URL, nil, nil) // dialer/rttFn filled later
		level := logger.GetLevel()
		logger = zerolog.New(quietWriter{zerolog.ConsoleWriter{
			Out: tUI.ring, NoColor: true, TimeFormat: time.RFC3339,
		}}).Level(level).With().Timestamp().Logger()
	}

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
			logger.Error().Err(err).Msg("Failed to connect to link relay")
			os.Exit(1)
		}
		logger.Info().Msg("Connected successfully to LinkSocks server")
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

	termClient.SetLogger(logger)
	if customDialer != nil {
		termClient.SetCustomDialer(customDialer)
	}

	if tuiMode {
		// tmux-like TUI: content area renders the remote terminal, the
		// bottom status bar shows host / latency, F2 toggles the log panel.
		tUI.dialer = customDialer
		tUI.rttFn = rttFn
		tUI.ring.Logf("LinkTerm TUI — connecting to %s", termClient.URL)
		if err := tUI.Run(); err != nil {
			// TUI already Fini'd; print to stderr
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := termClient.Connect(); err != nil {
		logger.Error().Err(err).Msg("Connection error")
		os.Exit(1)
	}
}
