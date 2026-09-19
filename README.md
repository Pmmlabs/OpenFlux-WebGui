# OpenFlux

**English** | [Русский](README.ru.md)

Network stack research tool: a TCP tunnel carried over pluggable covert
transports instead of a direct connection to a VPN/proxy server.

# Disclaimer

The author of OpenFlux **does not encourage** the use of this project to bypass
restrictions or violate the rules of any platform, and **is not responsible**
for the final scenarios of how users apply this tool in real life or on the
Internet. Any specific technical features of the application are nothing more
than an **architectural coincidence**, created **without any intent**.

The project is **entirely non-commercial**, contains **no paid features, hidden
subscriptions, or commercial benefit**.

The author **is not responsible** for forks, modifications, or derivative
versions of OpenFlux created by third parties. Any changes added to a fork are
the responsibility of its author.

The author **is not responsible** for:

- Any use of OpenFlux by third parties
- Consequences caused by the use of forks and modifications
- Damage resulting from derivative versions
- Violations committed using forks

The original code is provided **as is**, **without any warranties**.

## Description

Fork of [p1neappleXpress/OpenFlux](https://github.com/p1neappleXpress/OpenFlux),
paired with an [Android client fork](https://github.com/Kingofthedivanich/OpenFluxAndroid).
On top of upstream, this fork adds: opt-in Noise encryption (with a PSK-only
AES-256-GCM mode that interoperates with old `--encryption-key-file`
clients), a multi-client web admin panel (`--role=exit-panel`) with an
optional Telegram bot, one-scan QR client provisioning, auto-generated
Yandex documents, multi-stream (several documents, one tunnel), an SSRF
denylist on the exit node, and a long list of correctness/robustness fixes
across the transport and exit-node paths — see the commit history for
specifics.

## How it works

- Client and exit node talk to each other through a covert transport
  (Yandex.Docs, Yandex Volga, MAX/OneMe, Cups.online, Mail.ru Docs) instead of
  a direct connection.
- Traffic is coalesced, zstd-compressed, and optionally encrypted: Noise
  NKpsk0 (X25519 + AES-256-GCM, rotating session keys) with key files, or
  AES-256-GCM PSK-only (no handshake) with just a shared `--psk-file`.
- The exit node forwards real traffic with one of two backends: `--mode=l3`
  (raw SNAT/DNAT, Linux + root, fastest) or `--mode=l4` (gVisor userspace
  proxy, any OS, no root).
- `--role=exit-panel` runs many clients from one process behind a small web
  UI (add/remove, status, QR codes, optional Telegram bot), instead of one
  exit-node process per client.

## Install guide

```bash
go build -o openflux ./cmd/openflux
```

Exit node (pick one):

```bash
# l3 — Linux, root, fastest
sudo ./openflux --role=exit --mode=l3 --transport=yandex --url="YOUR_DOC_URL"

# l4 — any OS, no root
./openflux --role=exit --mode=l4 --transport=yandex --url="YOUR_DOC_URL"
```

Client:

```bash
./openflux --role=client --transport=yandex --url="YOUR_DOC_URL"
```

macOS uses a utun interface by default; everywhere else it's a SOCKS5 proxy
on `127.0.0.1:1080` (`--socks5=<addr>` to change).

Multi-client exit node with a web panel, instead of one process per client:

```bash
./openflux --role=exit-panel \
    --panel-addr=127.0.0.1:8088 --panel-user=admin --panel-pass=CHANGE_ME
```

Manage clients (add/remove, QR codes, Telegram bot, auto-generated Yandex
documents) from the web UI. Keep `--panel-addr` on loopback and reach it over
an SSH tunnel (`ssh -L 8088:127.0.0.1:8088 user@host`). On the VPS itself,
[`deploy/menu.sh`](deploy/menu.sh) (`openflux-ctl`) is a 3x-ui-style console
for updates, clients, and the bot — no hand-editing the systemd unit.

Encryption is opt-in: without key flags the tunnel runs plaintext
(`--allow-plaintext` only silences the warning). To encrypt, either share a
`--psk-file` between both peers (AES-256-GCM PSK-only mode, no key files —
also what old `--encryption-key-file` clients speak), or have the exit
print its public key on first run and give it to clients as `--peer-key`
(Noise NKpsk0). The panel's `--panel-key-file` is optional too: without it
panel clients run plaintext or PSK-only.

Prebuilt clients: [Android](https://github.com/Kingofthedivanich/OpenFluxAndroid/releases)
(this fork's paired app). [iOS](https://github.com/Kingofthedivanich/OpenFluxiOS)
is this fork's paired client too (source only for now, build it yourself);
[upstream's iOS TestFlight](https://testflight.apple.com/join/BwnAcdus) also
works but isn't paired with this fork's encryption — runs plaintext.
macOS/Linux/Windows: build from source above.

Full flag reference: `./openflux --help`.

## License

GNU General Public License v3.0 or later. See LICENSE for the full text.

Third-party licenses are listed in [NOTICE](NOTICE).
