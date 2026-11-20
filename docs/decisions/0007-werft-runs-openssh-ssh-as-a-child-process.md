# 0007: werft runs OpenSSH `ssh` as a child process

The agent gets the same command that werft used, and can run it again. vsock works through
`systemd-ssh-proxy`.

Rejected: `golang.org/x/crypto/ssh` with an AF_VSOCK dialer, because the agent cannot run it, although
it saves a process per call.
