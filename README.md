# WSTerm

A powerful WebSocket-based terminal sharing tool that allows you to securely expose and share your terminal (TTY) over any network, even through firewalls and NATs.

## Features

- Server mode: Expose your shell securely to clients through WebSockets
- Client mode: Connect to remote terminals with interactive access
- Intranet penetration via WSSocks: Share terminals through firewalls/NAT using secure tunneling

## Installation

WSTerm can be installed by:

```bash
go install github.com/zetxtech/wsterm/cmd/wsterm@latest
```

You can also download pre-built binaries for your architecture from the [releases page](https://github.com/zetxtech/wsterm/releases).

WSTerm is also available via Docker:

```bash
docker run --rm -it jackzzs/wsterm --help
```

## Usage

The application supports both direct connections and tunneled connections through WSSocks:

### Quick Start with WSSocks (Recommended)

Share your terminal through any network/firewall:

```bash
# On the server (machine sharing the terminal):
wsterm server -t YOUR_TOKEN

# On the client (machine accessing the terminal):
wsterm client -t YOUR_TOKEN
```

This method works everywhere - no port forwarding or firewall configuration needed!

You should use a complex token, as anyone holding the token can connect to your terminal.

```bash
openssl rand -hex 16
```

### Direct Connection Mode

For local network or when you have direct access:

#### Server Mode

```bash
# Basic usage
./wsterm server --port 8080 --host localhost

# Accept connections from any interface
./wsterm server --port 8080 --host 0.0.0.0

# Specify a custom shell
./wsterm server --shell /bin/zsh
```

#### Client Mode

```bash
# Connect to local server
./wsterm client --url ws://localhost:8080

# Connect to remote server
./wsterm client --url ws://example.com:8080

# Use with proxy
./wsterm client --proxy socks5://proxy.example.com:1080
```

## License

MIT 