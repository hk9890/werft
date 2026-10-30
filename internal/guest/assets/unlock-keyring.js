#!/usr/bin/env gjs
///////////////////////////////////////////////////////////////////////////////
// unlock-keyring.js — unlocks the default keyring inside a test VM, or
// creates the login keyring when there is none
//
// Usage: gjs unlock-keyring.js    (the user's password on stdin; prints the
//                                  object path of the keyring it unlocked)
//
// Run over SSH by the werft step unlock-keyring after an automatic login. A
// password login unlocks the keyring through PAM; an automatic one leaves it
// locked, and gh keeps its token in it. This is the step the password at the
// greeter does on a physical machine.
//
// The keyring is the one the 'default' alias names, which is where gh's
// token goes: Omarchy's installer creates 'Default_keyring' and points the
// alias at it. Without such an alias it is the login keyring.
//
// A base nobody has logged in to with a password has no login keyring at
// all: PAM creates it with the user's password on the first password login,
// and an automatic login creates none. The helper then creates it as PAM
// does, so a later password login unlocks it with the same password. The
// daemon takes both the object path and the file name from the label, so the
// label is 'login' at creation (collection/login, login.keyring) and becomes
// 'Login' after, which is what a keyring made by PAM shows.
//
// gnome-keyring-daemon --unlock cannot do it: on Ubuntu 26.04 it starts a
// second daemon next to the session's. The daemon's own unlock and create
// calls are used instead. gjs, because it ships with GNOME and a stock
// install has no python3-gi. Over SSH it needs the session's XDG_RUNTIME_DIR
// and DBUS_SESSION_BUS_ADDRESS. A wrong password throws
// org.gnome.keyring.Error.Denied, gjs exits non-zero, and the keyring stays
// locked.
///////////////////////////////////////////////////////////////////////////////

const {Gio, GioUnix, GLib} = imports.gi;

const stdin = new Gio.DataInputStream({
    base_stream: new GioUnix.InputStream({fd: 0, close_fd: false}),
});
const [line] = stdin.read_line_utf8(null);
// gjs packs 'ay' from a plain Array of byte numbers, not from a Uint8Array.
const password = Array.from(new TextEncoder().encode(line ?? ''));

const SERVICE = '/org/freedesktop/secrets';
const LOGIN = '/org/freedesktop/secrets/collection/login';
const INTERNAL = 'org.gnome.keyring.InternalUnsupportedGuiltRiddenInterface';
const bus = Gio.DBus.session;
const call = (path, iface, method, args) => bus.call_sync(
    'org.freedesktop.secrets', path, iface, method, args, null,
    Gio.DBusCallFlags.NONE, -1, null);

// "plain": the password travels unencrypted, over the user's own session bus.
const [, session] = call(SERVICE, 'org.freedesktop.Secret.Service',
    'OpenSession', new GLib.Variant('(sv)', ['plain', new GLib.Variant('s', '')])).deep_unpack();

// A Secret is (oayays): session, parameters, value, and a content type that
// is a string, not bytes.
const secret = [session, [], password, 'text/plain'];

const [alias] = call(SERVICE, 'org.freedesktop.Secret.Service', 'ReadAlias',
    new GLib.Variant('(s)', ['default'])).deep_unpack();
const [collections] = call(SERVICE, 'org.freedesktop.DBus.Properties', 'Get',
    new GLib.Variant('(ss)', ['org.freedesktop.Secret.Service', 'Collections'])).deep_unpack();
const known = collections.deep_unpack();
// ReadAlias answers '/' for an alias that names nothing.
const target = [alias, LOGIN].find(path => known.includes(path));
if (target) {
    call(SERVICE, INTERNAL, 'UnlockWithMasterPassword',
        new GLib.Variant('(o(oayays))', [target, secret]));
    print(target);
} else {
    const [created] = call(SERVICE, INTERNAL, 'CreateWithMasterPassword',
        new GLib.Variant('(a{sv}(oayays))', [
            {'org.freedesktop.Secret.Collection.Label': new GLib.Variant('s', 'login')},
            secret,
        ])).deep_unpack();
    if (created !== LOGIN)
        throw new Error(`created ${created}, not ${LOGIN}`);
    call(created, 'org.freedesktop.DBus.Properties', 'Set', new GLib.Variant('(ssv)', [
        'org.freedesktop.Secret.Collection', 'Label', new GLib.Variant('s', 'Login')]));
    call(SERVICE, 'org.freedesktop.Secret.Service', 'SetAlias',
        new GLib.Variant('(so)', ['default', created]));
    print(created);
}
