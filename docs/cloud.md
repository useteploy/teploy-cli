# Teploy Cloud

`teploy cloud` talks to Teploy Cloud (https://cloud.teploy.com), which sells managed servers. It needs an API key and the id of the organization the key belongs to; `teploy cloud login` checks them and stores them in `~/.teploy/cloud.yml` (mode 0600). `teploy cloud server create` buys a server, adds it to `~/.teploy/servers.yml` and runs `teploy setup` on it, so the next step is `server: <name>` in `teploy.yml` and `teploy deploy`.

```
teploy cloud login --org <org-id>                 # prompts for the key without echoing it
printf '%s' "$KEY" | teploy cloud login --key-stdin --org <org-id>
teploy cloud status                               # URL, organization, whether the key works
teploy cloud regions --provider vultr
teploy cloud sizes --provider vultr --region ewr
teploy cloud server create web1 --provider vultr --region ewr --size vc2-1c-1gb [--no-setup] [--yes]
teploy cloud server attach <name-or-id> [--name N] [--no-setup]   # finish a server that is already bought
teploy cloud servers [--json]
teploy cloud server delete web1 [--yes]
teploy cloud logout
```

## The API key and the organization id

Both come from the Teploy Cloud dashboard:

- **Organization id**: Settings > General, "Organization ID" (there is a copy button).
- **API key**: Settings > API keys > create a key, with the organization you want to use selected. A key works only in the organization it was created in; using it with another organization id is refused ("Key is bound to another organization"). The key is shown once.

A new key has no permissions until you tick them. `teploy cloud` needs:

| Permission | Used by |
|---|---|
| `read:servers` | `login` (it checks the key by listing servers), `status`, `servers`, `regions`, `sizes`, `server attach` |
| `write:servers` | `server create` (it also registers your SSH public key) and `server delete` |

`deploy:*` grants both (and the other infrastructure permissions); `admin:*` grants everything. A key without `read:servers` is refused at login with "API key lacks required permission: read:servers". Creating and deleting servers, and registering an SSH key, also need your role in the organization to be owner or admin: a member's key is refused for any change, whatever its permissions.

## Buying a server

`server create` bills the organization monthly and asks before buying unless `--yes` is given. Nothing is registered or bought before you confirm.

The server gets the public half of the SSH key the CLI then connects with, so the two always match: `--key` if given, else the private key next to `--ssh-key <key>.pub`, else `~/.ssh/id_ed25519`, else `~/.ssh/id_rsa`. The public key is derived from that private key (a `.pub` file that does not match it is refused) and registered with Teploy Cloud only if the organization does not have it yet.

The purchase is one request, sent once (with an `Idempotency-Key` header; the CLI does not rely on Teploy Cloud honouring it) and given up to two minutes. If the answer is lost (a timeout, a dropped connection, a 5xx), the CLI does not send it again: it lists the organization's servers and, when exactly one new server with the requested name has appeared, continues with that one. When none appears it reports that the server was not bought and tells you to check `teploy cloud servers` before trying again. A refusal from Teploy Cloud (a 4xx, such as a card problem or the new-organization server limit) is shown with Teploy Cloud's message, and nothing is bought.

The server is used only once Teploy Cloud reports it active with a real address (Teploy Cloud shows `0.0.0.0` until the provider assigns one; `teploy cloud servers` shows `-` meanwhile). The wait is bounded at ten minutes.

If the command stops after the purchase (the wait ran out, the name is taken in servers.yml, Ctrl-C), the server stays bought, and the error prints the command that finishes the job, for example `teploy cloud server attach 3f2c9a7e-...`. Do not run `server create` again: that buys a second server. If setup fails after the server was added to servers.yml, re-run `teploy setup <ip> --name <name>` as the error says.

## Deleting a server

`server delete` destroys the server and everything on it. The servers.yml entry of the same name is removed only when its host is the deleted server's address.

## Machine output

Every `--json` document carries `machine_interface` at its root, like the rest of the CLI (capability `cloud-v1`; schemas in `contracts/schema/cloud-*.schema.json`). `cloud status --json` exits 0 and reports a key that does not work in `key_works` and `error`. `server create --json` needs `--yes` and `--no-setup`, `server attach --json` needs `--no-setup`, and `server delete --json` needs `--yes`; progress goes to stderr. `create` and `attach` print their result document whenever a server exists, also when they then fail, with `ok: false`, `error` and the `resume` argv.

`TEPLOY_CLOUD_URL` points the commands at another Teploy Cloud instance. Plain http is refused except to localhost and 127.0.0.1.
