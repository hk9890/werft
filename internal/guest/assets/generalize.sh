# shellcheck shell=sh
# Gives a clone its own identity: hostname $1, a new machine-id and new SSH
# host keys. The hostname comes first, so the new host keys carry it.
# systemd-machine-id-setup would derive the id from the SMBIOS UUID, so the id
# is drawn at random. An Arch guest may have no /var/lib/dbus.
set -e
hostnamectl set-hostname "$1"
sed -i "s/^127\.0\.1\.1[[:space:]].*/127.0.1.1\t$1/" /etc/hosts
systemctl try-restart avahi-daemon || true
systemd-id128 new >/etc/machine-id
[ ! -d /var/lib/dbus ] || [ -L /var/lib/dbus/machine-id ] || ln -sf /etc/machine-id /var/lib/dbus/machine-id
rm -f /etc/ssh/ssh_host_*
ssh-keygen -A >/dev/null
