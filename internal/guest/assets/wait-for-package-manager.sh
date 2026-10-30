#!/usr/bin/env bash
# Wait until no boot-time job holds the system package manager.
#
# A freshly installed Ubuntu runs apt-daily and apt-daily-upgrade
# (unattended-upgrades) right after its first boot, and snapd seeds its snaps.
# While they run, apt-get fails at once on the dpkg, package-list or archive
# lock, and snap refuses to install. Waiting once before the first package
# operation covers every later call, including the overlay's.
#
# werft copies it into the guest with its other helpers; a recipe script
# sources it before its first package operation. Needs sudo: the lock holders
# are root processes that fuser can only see as root. A no-op without apt-get.

PACKAGE_MANAGER_WAIT_TIMEOUT="${PACKAGE_MANAGER_WAIT_TIMEOUT:-1800}"

_package_manager_busy() {
    local unit state
    for unit in apt-daily.service apt-daily-upgrade.service; do
        state="$(systemctl show -p ActiveState --value "$unit" 2>/dev/null)"
        case "$state" in
            activating|active|deactivating|reloading) return 0 ;;
        esac
    done
    sudo fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock \
        /var/lib/apt/lists/lock /var/cache/apt/archives/lock >/dev/null 2>&1
}

wait_for_package_manager() {
    command -v apt-get >/dev/null 2>&1 || return 0

    local waited=0
    while _package_manager_busy; do
        if (( waited >= PACKAGE_MANAGER_WAIT_TIMEOUT )); then
            echo "The package manager is still busy after ${waited}s — check: systemctl status apt-daily-upgrade.service" >&2
            return 1
        fi
        # Printed on stderr so --quiet runs, which drop stdout, still show it.
        (( waited % 60 == 0 )) &&
            echo "Waiting for boot-time package jobs (unattended-upgrades) to finish... ${waited}s" >&2
        sleep 10
        waited=$(( waited + 10 ))
    done

    if command -v snap >/dev/null 2>&1; then
        sudo snap wait system seed.loaded
    fi
}
