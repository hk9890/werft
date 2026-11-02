# shellcheck shell=sh
# Seals the copy of a build VM into a stock image, as root. $1 is the image's
# own user, $2 the werft key prepare installed for it, as "<type> <base64>",
# or empty. It removes the identity of the build VM, so each lease gets its
# own: the machine-id, the hostname and the SSH host keys. An empty
# machine-id makes systemd take a new one at the next boot. A static
# hostname would override the hostname credential of a lease.
# A guest without host keys accepts a vsock connection and closes it at once,
# so sshd makes them before it starts: in the vsock instances of
# systemd-ssh-generator, and through a oneshot for the TCP units.
# It prints the guest family and the systemd version first, before it
# changes anything.
set -e
home=$(getent passwd "$1" | cut -d: -f6)
[ -n "$home" ] || { echo "the guest has no user $1" >&2; exit 1; }
if [ -d /usr/share/omarchy ]; then echo omarchy; else echo ubuntu; fi
systemctl --version | sed -n '1s/^systemd \([0-9]*\).*/\1/p'

: >/etc/machine-id
rm -f /etc/hostname /etc/ssh/ssh_host_*

keys=$home/.ssh/authorized_keys
if [ -n "$2" ] && [ -f "$keys" ]; then
	grep -vF "$2" "$keys" >"$keys.werft" || true
	cat "$keys.werft" >"$keys"
	rm -f "$keys.werft"
fi

units=/etc/systemd/system
mkdir -p "$units/sshd-vsock@.service.d"
printf '[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n' >"$units/sshd-vsock@.service.d/werft-host-keys.conf"
printf '[Unit]\nDescription=SSH host keys of a werft stock image\n[Service]\nType=oneshot\nExecStart=/usr/bin/ssh-keygen -A\n' >"$units/werft-ssh-host-keys.service"
for unit in ssh sshd; do
	mkdir -p "$units/$unit.service.d"
	printf '[Unit]\nWants=werft-ssh-host-keys.service\nAfter=werft-ssh-host-keys.service\n' >"$units/$unit.service.d/werft-host-keys.conf"
done

printf '%s ALL=(ALL) NOPASSWD: ALL\n' "$1" >/etc/sudoers.d/werft
chmod 0440 /etc/sudoers.d/werft
visudo -cqf /etc/sudoers.d/werft >/dev/null
sync
