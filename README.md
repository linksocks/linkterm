# LinkTerm

A powerful WebSocket-based terminal sharing tool that allows you to securely expose and share your terminal (TTY) over any network, even through firewalls and NATs. The server runs on Linux, macOS, Windows (via ConPTY), FreeBSD and other Unix-like systems.

## Quick Start

Share your terminal through any network/firewall:

```bash
# On the server (machine sharing the terminal):
linkterm server

# On the client (machine accessing the terminal):
linkterm client -t <CONNECTOR_TOKEN>
```

The server connects anonymously by default and prints a connector token, which is used by the client:

```
INF Connected successfully to LinkSocks server connectorID=3a8b4dfe5e605e01
```

This method works everywhere - no port forwarding or firewall configuration needed!

For additional security you can use a custom token on both sides:

```bash
# Server:
linkterm server -t YOUR_TOKEN

# Client:
linkterm client -t YOUR_TOKEN
```

You should use a complex token in this case, as anyone holding the token can connect to your terminal:

```bash
openssl rand -hex 16
```

The connection is proxied via our public server: https://l.zetx.tech using [Linksocks](https://github.com/linksocks/linksocks). You can also host your Linksocks server on Cloudflare Workers: [linksocks/linksocks.js](https://github.com/linksocks/linksocks.js)

## Direct Connection Mode

For local network or when you have direct access:

Server:

```bash
# Host server at 8080
./linkterm server --port 8080 --host localhost
```

Client:

```bash
# Connect to local server
./linkterm client --url ws://localhost:8080
```

## TUI Client (tmux-like)

`--tui` switches the client to a full-screen terminal UI, similar to tmux:
a content area renders the remote terminal, a bottom status bar shows the
connected host and current latency, and a log panel can be toggled open.

```bash
./linkterm client --tui --url ws://localhost:8080
```

Layout (top to bottom):

```
+---------------------------------------------+
|  content area: the remote terminal          |
|  or the fullscreen log panel (F2 toggles)   |
+---------------------------------------------+
|  ● host  |  Latency 12ms  |  F2 Logs F3 Quit |
+---------------------------------------------+
```

Keys:

| Key | Action |
| --- | --- |
| `F2` | toggle the fullscreen log panel (all linkterm logs land here) |
| `F3` / `Ctrl+Q` | quit the TUI |
| mouse wheel | rewind the content scrollback; scrolls the log panel when it is open |
| `Esc` / `g` (while rewound) | jump back to the live screen |

All other keys are forwarded to the remote terminal as usual. In the TUI,
logs are routed into the log panel instead of stdout.

## Installation

LinkTerm can be installed by:

```bash
go install github.com/linksocks/linkterm/cmd/linkterm@latest
```

You can also download pre-built binaries for your architecture from the [releases page](https://github.com/linksocks/linkterm/releases).

LinkTerm is also available via Docker:

```bash
docker run --rm -it jackzzs/linkterm --help
```

## License

MIT 
