#!/usr/bin/env bash
set -euo pipefail

binary="${1:?usage: install.sh path/to/peerly-server-linux-arm64}"

sudo useradd --system --home /var/lib/peerly --shell /usr/sbin/nologin peerly 2>/dev/null || true
sudo install -d -o peerly -g peerly -m 750 /var/lib/peerly
sudo install -d -m 750 /etc/peerly
if [ ! -f /etc/peerly/env ]; then
	echo "PEERLY_ADMIN_KEY=$(openssl rand -hex 24)" | sudo tee /etc/peerly/env >/dev/null
	sudo chmod 600 /etc/peerly/env
fi
sudo install -m 755 "$binary" /usr/local/bin/peerly-server
sudo install -m 644 "$(dirname "$0")/peerly.service" /etc/systemd/system/peerly.service
sudo systemctl daemon-reload
sudo systemctl enable --now peerly
sudo systemctl restart peerly
echo "admin key for creating groups:"
sudo cat /etc/peerly/env
cat <<'NOTE'

peerly listens on 127.0.0.1:8787 only. Put Caddy in front of it (see deploy/Caddyfile) and open ports 80 and 443.
On Oracle Cloud two firewalls block them by default:
  1. the VCN security list: add ingress rules for TCP 80 and 443 in the web console
  2. the instance itself:
       sudo iptables -I INPUT 6 -m state --state NEW -p tcp --dport 80 -j ACCEPT
       sudo iptables -I INPUT 6 -m state --state NEW -p tcp --dport 443 -j ACCEPT
       sudo netfilter-persistent save
NOTE
