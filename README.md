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

How it works:

1. The server connects to the relay anonymously. The relay assigns a
   temporary identity (server token) which the server saves and uses to
   reconnect with, so the same relay is reused across reconnects.
2. The server then registers a connector token. Without `-t`, a random one
   is generated and printed - use that on the client:

   ```
   INF Server is online
   INF Clients can connect with: linkterm client -t 3a8b4dfe5e605e01
   ```

   ```
   # On the client (machine accessing the terminal):
   linkterm client -t 3a8b4dfe5e605e01
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

The connection is proxied via our public relay: https://l.zetx.tech. You can also host your own relay on Cloudflare Workers: [linksocks/linksocks.js](https://github.com/linksocks/linksocks.js)

## Direct Connection Mode

For local network or when you have direct access:

Server:

```bash
# Host server at 8273
./linkterm server --port 8273 --host localhost
```

Client:

```bash
# Connect to local server
./linkterm client --url ws://localhost:8273
```

## TUI Client (tmux-like)

The client uses a full-screen terminal UI by default (similar to tmux) when
stdout is a terminal: a content area renders the remote terminal, a bottom
status bar shows the host, latency and relay state, and a log panel can be
toggled open. Use `--no-tui` to force the plain console mode (this also
happens automatically when stdout is not a terminal, e.g. in a pipe).

```bash
# default: TUI on a terminal
./linkterm client --url ws://localhost:8273

# plain console mode
./linkterm client --no-tui --url ws://localhost:8273
```

Layout (top to bottom):

```
+---------------------------------------------+
|  content area: the remote terminal          |
|  or the fullscreen log panel (F2 toggles)   |
+---------------------------------------------+
|  ● host | Latency 12ms |  F2 Logs F3 Quit   |
+---------------------------------------------+
```

The status bar shows (left to right) a connection dot, the remote host or
connection state, the link latency, and the clickable hotkey hints. The dot
is amber while connecting, green when connected, and red when the connection
failed or is disconnected, in which case the status text shows
`Connection failed` / `Disconnected` plus the reason and the latency is
hidden.

The TUI opens immediately and the link connection runs inside it: while the
relay handshake and the terminal dial are in progress the status bar shows
`Connecting to <host>...`, every client log line lands in the F2 panel in
real time (colour-coded by level), and a failed connection keeps the TUI
alive so you can read the logs before quitting with F3 (which exits with a
non-zero code and prints a concise error on stderr).

Keys:

| Key | Action |
| --- | --- |
| `F2` | toggle the fullscreen log panel (all client logs land here, with zerolog timestamps and colour-coded levels) |
| `F3` / `Ctrl+Q` | quit the TUI |
| mouse wheel | rewind the content scrollback; scrolls the log panel when it is open |
| left mouse drag (log panel) | select log text; long lines wrap to the panel width |
| `Ctrl+C` (log panel) | copy the selected text through the terminal clipboard (OSC 52) |
| `PgUp` / `PgDn` (log panel) | scroll the log panel by one page |
| `Esc` / `g` (while rewound) | jump back to the live screen |

Clicking the `F2 Logs` / `F3 Quit` text in the status bar works the same as
pressing the keys. All other keys are forwarded to the remote terminal as
usual; logs are routed into the log panel instead of stdout. Log entries use
the same zerolog console layout as plain mode (`timestamp level message`),
with timestamps dimmed and level markers colour-coded.

If the terminal WebSocket drops, the TUI keeps the last screen, shows a red
`Disconnected` status and automatically reconnects with exponential backoff,
restoring the session when the link recovers. The relay state is appended to
the host (`disconnected` / `reconnecting`) when the terminal stays up but the
link relay link drops.

Note: a connector token is validated by the relay (at least 8 characters and
not a trivial pattern like `abc123`). If you pass an explicit weak `-t` to
the server, it exits immediately with an error instead of silently swapping
in a random token, so the client can never end up with a mismatched identity.

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
