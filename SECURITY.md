# Security Policy

## Reporting a vulnerability

Report a vulnerability **privately**, through GitHub's
[private vulnerability reporting](https://github.com/hk9890/werft/security/advisories/new)
("Report a vulnerability" on the **Security** tab of the repository). Do not open a public issue
for a suspected vulnerability.

Include:

- A description of the issue and its impact.
- Steps to reproduce (commands, recipe, environment).
- The affected version (`werft version`) or commit.

Expect an acknowledgement within a few days. When a fix is ready, we publish a patched release
and credit the reporter, unless they ask to stay anonymous.

## Supported versions

Security fixes go into the latest release only. Older releases get no backports.

## Scope

werft is a local CLI. It runs as your user and drives `qemu:///system` through group `libvirt`,
without sudo. Of particular relevance:

- **Lease keys and `ssh_config`** under `~/.local/state/werft/leases/<id>/`: any process of your
  user can read them and reach the lease.
- **Secrets**: the guest password and recipe secrets come from a keyring item, `pass` or a
  terminal prompt.
  werft passes them to the guest on stdin, never as a command-line argument, with two
  exceptions: it types the LUKS disk password at the guest console through libvirt, and the
  `pinentry` step keeps the passphrase in a file in the guest until the step is undone.
- **Recipes** run commands in the guest and copy host files into it. Treat a recipe as trusted
  input, as you would a shell script.
- **Stock images** are copies of your build VMs. Anything a build VM holds, a lease of it holds.
