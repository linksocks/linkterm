# LinkTerm

A powerful WebSocket-based terminal sharing tool that allows you to securely expose and share your terminal (TTY) over any network, even through firewalls and NATs.

## Quick Start

Share your terminal through any network/firewall:

```bash
# On the server (machine sharing the terminal):
linkterm server -t YOUR_TOKEN

# On the client (machine accessing the terminal):
linkterm client -t YOUR_TOKEN
```

This method works everywhere - no port forwarding or firewall configuration needed!

The connection is proxied via our public server: https://linksocks.zetx.tech using [Linksocks](https://github.com/linksocks/linksocks). You can also host your Linksocks server on Cloudflare Workers: [linksocks/linksocks.js](https://github.com/linksocks/linksocks.js)

You should use a complex token, as anyone holding the token can connect to your terminal.

```bash
openssl rand -hex 16
```

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
