# TODO

## harden: bake trusted-network `ignoreip` into the fail2ban config

`internal/harden/harden.go` writes `[sshd]\nenabled = true\nmode = aggressive` and enables
fail2ban, but it does **not** set `ignoreip`. Combined with aggressive mode + a low
`maxretry`, this causes the server to ban trusted IPs (e.g. an operator's VPN / tailnet
CGNAT range such as `100.64.0.0/10`) after only a couple of failed auth attempts —
locking the operator out of their own SSH for the full `bantime`.

Fix: when writing the jail config, include an `ignoreip` line covering at minimum
`127.0.0.1/8 ::1` plus the deployer's trusted network. Parameterize the trusted CIDR
(a `--trusted-cidr` flag, or read from the teploy config file) so the operator's mesh
traffic is never banned by their own hardening step.

Observed in the wild: a Tailscale tailnet IP got banned for 24h after two failed pubkey
attempts, manifesting as "Connection refused" on port 22 (banaction = ufw sends RST).
