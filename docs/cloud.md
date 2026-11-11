# Teploy Cloud

`teploy cloud` talks to Teploy Cloud (https://cloud.teploy.com), which sells managed servers. It needs an API key and the id of the organization the key belongs to, both from the Teploy Cloud dashboard; `teploy cloud login` checks them and stores them in `~/.teploy/cloud.yml` (mode 0600). `teploy cloud server create` buys a server, adds it to `~/.teploy/servers.yml` and runs `teploy setup` on it, so the next step is `server: <name>` in `teploy.yml` and `teploy deploy`.

```
teploy cloud login --org <org-id>                 # prompts for the key without echoing it
printf '%s' "$KEY" | teploy cloud login --key-stdin --org <org-id>
teploy cloud status                               # URL, organization, whether the key works
teploy cloud regions --provider vultr
teploy cloud sizes --provider vultr --region ewr
teploy cloud server create web1 --provider vultr --region ewr --size vc2-1c-1gb [--ssh-key ~/.ssh/id_ed25519.pub] [--no-setup] [--yes]
teploy cloud servers [--json]
teploy cloud server delete web1 [--yes]
teploy cloud logout
```

`server create` bills the organization monthly and asks before buying unless `--yes` is given. The SSH public key is `--ssh-key`, else `~/.ssh/id_ed25519.pub`, else `~/.ssh/id_rsa.pub`; it is registered with Teploy Cloud only if no key with the same public key is there. If setup fails after the server is active, the server stays bought and in servers.yml; re-run `teploy setup <ip> --name <name>`.

`server delete` destroys the server and everything on it. The servers.yml entry of the same name is removed only when its host is the deleted server's address.

`TEPLOY_CLOUD_URL` points the commands at another Teploy Cloud instance. Plain http is refused except to localhost and 127.0.0.1.
