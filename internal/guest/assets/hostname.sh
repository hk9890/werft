# shellcheck shell=sh
# Names the guest $1. The guest then answers on $1.local: avahi-daemon reads
# the host name only at its start, so it is restarted.
set -e
hostnamectl set-hostname "$1"
sed -i "s/^127\.0\.1\.1[[:space:]].*/127.0.1.1\t$1/" /etc/hosts
systemctl try-restart avahi-daemon || true
