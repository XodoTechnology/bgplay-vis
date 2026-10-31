#!/usr/bin/env bash
#
# bgplay-vis installer — Debian/Ubuntu.
#
#   sudo deploy/install-debian.sh --domain bgplay.example.com --email ops@x.com
#   sudo deploy/install-debian.sh                       # no TLS, UI on :80
#   sudo deploy/install-debian.sh --uninstall
#
# Flags:
#   --domain FQDN        serve via nginx with Let's Encrypt on :443
#   --email ADDR         certbot contact (required with --domain)
#   --asn N              local ASN (default 65000)
#   --rid IP             router-id (default: first global IPv4)
#   --bgp ADDR           BGP listen (default :179, "" disables)
#   --peer HOST          outbound peer, repeatable
#   --demo               enable synthetic feed
#   --tls letsencrypt|none   default: letsencrypt when --domain given
#   --uninstall          remove service, binary, nginx site
#
set -euo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN=/usr/local/bin/bgplay-vis
ENVFILE=/etc/bgplay/env
UNIT=/etc/systemd/system/bgplay-vis.service
NGX=/etc/nginx/sites-available/bgplay-vis

ASN=65000
RID=""
BGP_ADDR=":179"
WEB_ADDR="127.0.0.1:8080"
DOMAIN=""
EMAIL=""
TLS=""
DEMO=0
UNINSTALL=0
declare -a PEERS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2;;
    --email)  EMAIL="$2"; shift 2;;
    --asn)    ASN="$2"; shift 2;;
    --rid)    RID="$2"; shift 2;;
    --bgp)    BGP_ADDR="$2"; shift 2;;
    --peer)   PEERS+=("$2"); shift 2;;
    --tls)    TLS="$2"; shift 2;;
    --demo)   DEMO=1; shift;;
    --uninstall) UNINSTALL=1; shift;;
    -h|--help) sed -n '2,22p' "$0"; exit 0;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

[[ -z "$RID" ]] && RID="$(hostname -I 2>/dev/null | awk '{print $1}')"
[[ -z "$RID" ]] && RID="192.0.2.1"
[[ -z "$TLS" ]] && TLS=$([[ -n "$DOMAIN" ]] && echo letsencrypt || echo none)

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
command -v systemctl >/dev/null || die "systemd required"
command -v apt-get >/dev/null || die "this installer targets Debian/Ubuntu (apt-get)"

# ---------------- uninstall ----------------
if [[ $UNINSTALL -eq 1 ]]; then
  say "uninstalling bgplay-vis"
  systemctl disable --now bgplay-vis 2>/dev/null || true
  rm -f "$UNIT" "$BIN" "$ENVFILE"
  rm -f /etc/nginx/sites-enabled/bgplay-vis "$NGX"
  systemctl daemon-reload
  systemctl reload nginx 2>/dev/null || true
  say "done (user 'bgplay', certbot certs and logs left in place)"
  exit 0
fi

# ---------------- toolchain ----------------
need_go() {
  command -v go >/dev/null || return 0
  local v; v="$(go version | grep -oP 'go\K[0-9]+\.[0-9]+' | head -1)"
  awk -v v="$v" 'BEGIN{split(v,a,"."); exit !(a[1]>1 || a[1]==1 && a[2]>=24)}'
}

say "installing dependencies"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq libcap2-bin curl >/dev/null

if need_go; then
  say "installing Go toolchain"
  GOVER="$(curl -fsSL 'https://go.dev/dl/?mode=json' | grep -oP '"version":\s*"\Kgo[0-9.]+' | head -1)"
  [[ -n "$GOVER" ]] || die "could not resolve latest Go version"
  curl -fsSL "https://go.dev/dl/${GOVER}.linux-$(dpkg --print-architecture | sed 's/amd64/amd64/;s/arm64/arm64/').tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
fi
say "using $(go version)"

# ---------------- build + install ----------------
say "building bgplay-vis"
cd "$SRC_DIR"
go build -trimpath -o "$BIN" ./cmd/bgplay-vis
setcap 'cap_net_bind_service=+ep' "$BIN"
id -u bgplay >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin bgplay
mkdir -p /etc/bgplay

# ---------------- service ----------------
say "writing env + systemd unit"
if [[ "$TLS" == "letsencrypt" ]]; then
  WEB_ADDR="127.0.0.1:8080"     # behind nginx
else
  WEB_ADDR=":80"                # serve UI directly on :80
fi
ARGS="-asn $ASN -rid $RID -bgp '$BGP_ADDR' -web '$WEB_ADDR'"
for p in "${PEERS[@]:-}"; do [[ -n "$p" ]] && ARGS="$ARGS -peer '$p'"; done
[[ $DEMO -eq 1 ]] && ARGS="$ARGS -demo"

cat > "$ENVFILE" <<EOF
# bgplay-vis install parameters (the unit bakes these into ExecStart)
ARGS=$ARGS
EOF

cat > "$UNIT" <<EOF
[Unit]
Description=bgplay-vis — live BGP route collector + visualizer
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN $ARGS
User=bgplay
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=always
RestartSec=3
LimitNOFILE=65536
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
EOF

# ---------------- web / TLS ----------------
if [[ "$TLS" == "letsencrypt" ]]; then
  [[ -n "$EMAIL" ]] || die "--email required for letsencrypt"
  say "installing nginx + certbot"
  apt-get install -y -qq nginx certbot python3-certbot-nginx >/dev/null

  cat > "$NGX" <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;

    location / {
        proxy_pass http://$WEB_ADDR;
        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
EOF
  ln -sf "$NGX" /etc/nginx/sites-enabled/bgplay-vis
  rm -f /etc/nginx/sites-enabled/default
  nginx -t
  systemctl enable --now nginx
  say "requesting certificate for $DOMAIN"
  certbot --nginx --noninteractive --agree-tos -m "$EMAIL" -d "$DOMAIN" --redirect
  systemctl reload nginx
else
  say "plain HTTP on :80 (no TLS)"
fi

# ---------------- start ----------------
systemctl daemon-reload
systemctl enable --now bgplay-vis
sleep 1
systemctl is-active --quiet bgplay-vis || { journalctl -u bgplay-vis -n 20 --no-pager; die "service failed to start"; }

say "bgplay-vis is up"
echo
echo "  UI:      $([[ "$TLS" == letsencrypt ]] && echo "https://$DOMAIN" || echo "http://$(hostname -I | awk '{print $1}')")"
echo "  BGP:     $BGP_ADDR (AS$ASN, rid $RID) — point routers at this host"
echo "  status:  systemctl status bgplay-vis"
echo "  env:     $ENVFILE"
echo
echo "  router side (FRR):"
echo "    router bgp <your-as>"
echo "     neighbor $(hostname -I | awk '{print $1}') remote-as $ASN"
echo "     neighbor $(hostname -I | awk '{print $1}') ebgp-multihop"
echo "     address-family ipv4 unicast"
echo "      neighbor $(hostname -I | awk '{print $1}') activate"
