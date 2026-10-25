#!/usr/bin/env bash
# One-time setup on a fresh Ubuntu 24.04 VPS. Run from the deploy/ directory:
#   sudo ./setup.sh
# Installs Docker if missing, opens the firewall, creates the data dirs,
# writes .env (asking for the address and login), and starts everything.
set -euo pipefail
cd "$(dirname "$0")"

if [ "$(id -u)" -ne 0 ]; then
	echo "run with sudo" >&2
	exit 1
fi

if ! command -v docker >/dev/null; then
	echo "Installing Docker from Docker's apt repository..."
	apt-get update
	apt-get install -y ca-certificates curl
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	. /etc/os-release
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
		>/etc/apt/sources.list.d/docker.list
	apt-get update
	apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi

if [ ! -f .env ]; then
	cp .env.example .env
	guess=$(hostname -I | awk '{print $1}')
	read -rp "Public IP or domain of this server [$guess]: " site
	site=${site:-$guess}
	read -rp "Login user name [admin]: " user
	user=${user:-admin}
	read -rsp "Login password: " pw
	echo
	if [ -z "$pw" ]; then
		echo "a password is required" >&2
		exit 1
	fi
	hash=$(docker run --rm caddy:2.10 caddy hash-password --plaintext "$pw")
	sed -i "s|^SITE_ADDRESS=.*|SITE_ADDRESS=$site|; s|^AUTH_USER=.*|AUTH_USER=$user|; s|^AUTH_HASH=.*|AUTH_HASH='$hash'|" .env
	chmod 600 .env
	echo "Wrote .env (edit it for storage limits and encoder settings)."
fi

# The container runs as UID 1000 (the image's "ubuntu" user).
mkdir -p data/work data/output data/torrents data/db data/fonts data/keys data/caddy data/caddy-config
chown -R 1000:1000 data/work data/output data/torrents data/db data/fonts data/keys

if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
	port=$(grep -E '^TORRENT_PORT=' .env | cut -d= -f2)
	ufw allow 80/tcp
	ufw allow 443/tcp
	ufw allow 443/udp
	ufw allow "${port:-42069}"
fi

docker compose up -d --build
echo
echo "torClient is starting. Open https://$(grep -E '^SITE_ADDRESS=' .env | cut -d= -f2)/"
echo "With an IP address, the browser warns about the certificate until you trust Caddy's CA (see README)."
