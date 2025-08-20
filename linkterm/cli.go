package linkterm

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	serverCmd.Flags().StringVarP(&linksocksToken, "token", "t", "", "LinkSocks token for intranet penetration")
	serverCmd.Flags().StringVarP(&linksocksURL, "linksocks-url", "U", "https://linksocks.zetx.tech", "LinkSocks server URL")

	// Add flags to client command
	clientCmd.Flags().StringVarP(&clientURL, "url", "u", "ws://localhost:8080", "URL to connect to (e.g. example.com or ws://example.com:8080/terminal)")
	clientCmd.Flags().CountVarP(&debugCount, "debug", "d", "Debug level (-d=debug, -dd=trace)")
	clientCmd.Flags().StringVarP(&linksocksToken, "token", "t", "", "LinkSocks token for intranet penetration")
	clientCmd.Flags().StringVarP(&linksocksURL, "linksocks-url", "U", "https://linksocks.zetx.tech", "LinkSocks server URL")
	clientCmd.Flags().StringVarP(&proxyURL, "proxy", "x", "", "Proxy URL (e.g. socks5://user:pass@host:port or http://user:pass@host:port)")

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
			// Default to bash if $SHELL is not set
			if _, err := exec.LookPath("bash"); err == nil {
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
		logger.Info().Str("url", linksocksURL).Msg("Starting LinkSocks connection")
		clientOpt := linksocks.DefaultClientOption().
			WithWSURL(linksocksURL).
			WithReverse(true).
			WithLogger(logger)

		wsClient := linksocks.NewLinkSocksClient(linksocksToken, clientOpt)
		defer wsClient.Close()

		err := wsClient.WaitReady(cmd.Context(), 0)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to connect to linksocks server")
			os.Exit(1)
		} else {
			connectorID, err := wsClient.AddConnector(linksocksToken)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to add connector token")
				os.Exit(1)
			} else {
				logger.Info().Str("connectorID", connectorID).Msg("Connected successfully to LinkSocks server")
			}
		}
	}

	logger.Info().Str("host", serverHost).Int("port", serverPort).Str("shell", shellPath).Msg("Starting terminal server")
	if err := server.Start(); err != nil {
		logger.Error().Err(err).Msg("Server error")
		os.Exit(1)
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
			WithSocksPort(wsocksLocalPort).
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

	if err := termClient.Connect(); err != nil {
		logger.Error().Err(err).Msg("Connection error")
		os.Exit(1)
	}
}
