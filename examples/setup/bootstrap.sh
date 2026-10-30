#!/usr/bin/env bash
# bootstrap.sh: the fresh-machine bootstrap of the setup repo, run in a
# build VM by the werft recipe next to it.
#
# Usage: bootstrap.sh <bootstrap-repo-url>
#
# It does what a person does on a new physical machine: clone
# bootstrap-scripts and run its install.sh with the key stick plugged in.
# werft passes the stick through, so import-key.sh finds it under /media
# (/run/media on Omarchy), and its pinentry step answers the passphrase
# prompt. wait-for-package-manager.sh is one of the helpers werft copies in
# next to this file.

set -euo pipefail

REPO_URL="${1:?usage: bootstrap.sh <bootstrap-repo-url>}"
GUEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOOTSTRAP_DIR="$HOME/bootstrap-scripts"

# shellcheck source=/dev/null
source "$GUEST_DIR/wait-for-package-manager.sh"

export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
# gsettings and gnome-extensions reach the logged-in GNOME session through it.
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"
exec </dev/null

step() { printf '\n==> %s\n' "$*"; }

# The recipe opens sudo for the length of this script; nothing below can
# answer a password prompt.
step "Checking that sudo is open"
sudo -n true || {
    echo "sudo asks for a password; the recipe opens it with the step sudo-grant first" >&2
    exit 1
}

# Before bootstrap-scripts' own apt-get calls too: right after boot the apt
# jobs hold the locks, and init-debian.sh would fail on them.
step "Waiting for the package manager"
wait_for_package_manager

# Omarchy ships git. Were it missing, pacman -S without -y installs it from
# the package lists omarchy update just refreshed: a refresh here without the
# upgrade would be a partial upgrade.
step "Installing git"
if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update -qq
    sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git
elif ! command -v git >/dev/null 2>&1; then
    sudo pacman -S --needed --noconfirm git
fi

step "Cloning $REPO_URL"
rm -rf "$BOOTSTRAP_DIR"
git clone -q "$REPO_URL" "$BOOTSTRAP_DIR"

step "Running bootstrap-scripts install.sh"
bash "$BOOTSTRAP_DIR/install.sh"
