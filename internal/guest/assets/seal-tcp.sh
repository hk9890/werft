# shellcheck shell=sh
# Runs before seal.sh on a systemd 255 guest, as root. $3 is the public key
# of the seal run. It removes that key, and fails when the key stays. A
# regular D-Bus machine-id file would win over the machine-id credential of a
# lease, so it becomes a link to /etc/machine-id. It prints nothing on stdout:
# seal reads the guest facts of seal.sh there.
set -e
v=$(systemctl --version | sed -n '1s/^systemd \([0-9]*\).*/\1/p')
[ "$v" = 255 ] || { echo "the guest has systemd $v, not 255" >&2; exit 1; }
home=$(getent passwd "$1" | cut -d: -f6)
group=$(id -gn "$1")
[ "$home" = "/home/$1" ] && [ "$group" = "$1" ] || {
	echo "a lease at systemd 255 writes its key to /home/$1 with group $1; the guest has home $home and group $group" >&2
	exit 1
}
sed -i '/^127\.0\.1\.1[[:space:]]/d' /etc/hosts
keys=/root/.ssh/authorized_keys
rc=0; grep -vxF -- "$3" "$keys" >"$keys.werft" || rc=$?
[ "$rc" -le 1 ] || exit "$rc"
cat "$keys.werft" >"$keys"
rm -f "$keys.werft"
rc=0; grep -qxF -- "$3" "$keys" || rc=$?
[ "$rc" -eq 1 ] || { echo "the seal key is still in $keys" >&2; exit 1; }
if [ -f /var/lib/dbus/machine-id ] && [ ! -L /var/lib/dbus/machine-id ]; then
	ln -sf /etc/machine-id /var/lib/dbus/machine-id
fi
