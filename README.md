# bgplay·vis

Live BGP route collector + visualizer. Point your routers at it, it keeps
per-peer adj-rib-in tables, and you watch announces / withdrawals / path
changes fly around an animated AS graph in your browser.

```
        ┌─────────┐   BGP :179    ┌──────────────┐   ws :8080   ┌──────────┐
        │ router1 │ ────────────▶ │              │ ───────────▶ │ browser  │
        │ router2 │ ────────────▶ │  bgplay-vis  │              │ (canvas) │
        │ router3 │ ────────────▶ │  AS-graph UI │ ◀─────────── │ search   │
        └─────────┘               └──────────────┘   /api/*     └──────────┘
```

- Passive BGP speaker — receives everything, **never originates routes**.
- IPv4 + IPv6 unicast, 4-byte ASNs, add-path receive (multiple paths per
  prefix from route servers), MP-BGP, graceful-restart tolerant.
- Routes kept per-peer (adj-rib-in); session drops mark routes `stale`
  instead of dropping them, resync flushes what wasn't refreshed.
- Live canvas: local AS centered, peers on ring 1, transit/origin ASes on
  outer rings; announces stream in along the AS path and settle as glowing
  dots orbiting their origin; withdrawals race back out in red.
- Search by prefix (`1.1.1.0/24`), bare IP (`8.8.8.8` → LPM chain),
  AS number (`as13335` → every route through it), or peer name.
- Every event is broadcast on `/ws` — a future **bgpath** archiver can
  just tail the same stream for history.

## Run

```bash
go build -o bgplay-vis ./cmd/bgplay-vis
./bgplay-vis -asn 65000 -rid 10.0.0.5 -bgp :179 -web :8080
```

Port 179 is privileged → run as root, or `setcap 'cap_net_bind_service=+ep' bgplay-vis`,
or just run `-bgp :1179` and point routers at port 1179.

| flag | default | meaning |
|---|---|---|
| `-asn` | 65000 | local ASN presented in OPEN |
| `-rid` | 192.0.2.1 | BGP router-id |
| `-bgp` | `:179` | listen addr (`-bgp=` disables) |
| `-peer` | — | outbound session, repeatable (`-peer 192.0.2.1`) |
| `-hold` | 90 | holdtime seconds (0 disables keepalives) |
| `-web` | `:8080` | UI address |
| `-demo` | off | synthetic feed, no router needed |

Try it instantly: `./bgplay-vis -demo -bgp :1179` → http://localhost:8080

## Docker

```bash
docker build -t bgplay-vis .
docker run -d -p 179:179 -p 8080:8080 bgplay-vis -asn 65000
```

or `docker compose up -d`. Runs as a non-root user — the binary carries
`cap_net_bind_service` so `:179` still binds. Stateless, `read_only` is
safe.

## Install (Debian/Ubuntu, one command)

```bash
sudo deploy/install-debian.sh --domain bgplay.example.com --email ops@x.com
```

Builds from source (installs Go if needed), drops a systemd unit with
`AmbientCapabilities=CAP_NET_BIND_SERVICE`, and front-ends the UI with
nginx + certbot (`443` with a `80→443` redirect). Without `--domain` it
serves plain HTTP on `:80`. Other flags: `--asn`, `--rid`, `--bgp`,
`--peer`, `--demo`, `--tls none|letsencrypt`, `--uninstall`.

## Router config

The collector is a passive iBGP-style neighbor. It only receives — give it
a session that exports your table (full view, or a route-server feed).

**FRR** (router connects to collector):

```
router bgp 64512
 neighbor 10.0.0.5 remote-as 65000
 neighbor 10.0.0.5 description bgplay-vis
 address-family ipv4 unicast
  neighbor 10.0.0.5 activate
  neighbor 10.0.0.5 addpath-tx-all-paths   ! optional: all paths, not just best
 exit-address-family
```

**BIRD**:

```
protocol bgp bgplay {
  local as 64512;
  neighbor 10.0.0.5 port 179 as 65000;
  ipv4 { export all; };
  ipv6 { export all; };
  add paths tx;          # optional
}
```

**pathvector**: add a peer block — it renders the same BIRD config:

```yaml
peers:
  bgplay-vis:
    asn: 65000
    neighbors: ["10.0.0.5"]
    type: upstream        # exports the full table toward it
    add-path: true        # if your pathvector version supports it
```

Or let the collector dial out instead: `./bgplay-vis -peer r1.example.net -peer r2.example.net`
(configure `neighbor … passive` on the routers).

## API

| endpoint | returns |
|---|---|
| `GET /ws` | websocket: `snapshot` then batched `events` + 1s `stats` |
| `GET /api/stats` | counters + peer table |
| `GET /api/search?q=` | prefix / IP / `asNNNN` / peer-name → matching routes |

Event shape on the wire:

```json
{"seq":4102,"ts":1793403661,"type":"announce","peer":"10.0.0.9","pa":64512,
 "pfx":"203.0.113.0/24","path":[64512,174,64496],"nh":"10.0.0.1","fam":"ipv4-unicast"}
```

`type` ∈ `announce | withdraw | change | peer_up | peer_down | eor | flush`.

## Layout

```
cmd/bgplay-vis      flags + wiring
internal/bgp        wire format, OPEN caps, UPDATE parser, session FSM, speaker
internal/rib        adj-rib-in, diff engine, prefix trie, search
internal/hub        event batching → fanout
internal/web        embedded UI, REST, hand-rolled websocket
internal/demo       synthetic feed (-demo)
```

No external dependencies. `go test ./...` covers the UPDATE parser
(MP-BGP, AS4 merge, add-path, EOR).
