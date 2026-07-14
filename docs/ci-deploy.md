# CI/CD: deploy from Forgejo or GitHub Actions

Teploy doesn't ship a CI runner — it integrates with the one you already run.
This is the recipe to wire push-to-deploy into Forgejo Actions or GitHub
Actions. Same shape as the [secrets-scanning](secrets-scanning.md) recipe: a
drop-in workflow, not a product.

There are two models. Pick one:

- **CI-driven deploy** (this doc) — CI builds + tests + `teploy deploy`s. Full
  control, test-gate before deploy. Best for most teams.
- **Webhook auto-deploy** — the box redeploys itself on push, no CI secrets. See
  [§Webhook alternative](#webhook-alternative) below. Simplest, no test-gate.

## How it works

CI builds your image, pushes it to a registry the server can pull from, then
runs `teploy deploy` over SSH — the same zero-downtime deploy you run locally.
Your `teploy.yml` (committed to the repo) supplies the app config; `--host` /
`--user` / `--key` stand in for `servers.yml`, which isn't in CI.

**One-time server prep:** if your registry is private, the *server* needs to be
able to pull from it — run `teploy registry login <server>` once beforehand.

## Forgejo Actions

`.forgejo/workflows/deploy.yml`:

```yaml
name: deploy
on:
  push:
    branches: [main]

env:
  IMAGE: ${{ secrets.REGISTRY }}/myapp   # your image repo

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      # (optional but recommended) run your tests first — a failure here
      # stops the deploy.
      - name: Test
        run: make test   # or: go test ./...  /  npm test  /  ...

      # 1. Build + push the image so the server can pull it.
      - name: Build and push
        run: |
          TAG="${IMAGE}:${GITHUB_SHA::12}"
          echo "${{ secrets.REGISTRY_PASSWORD }}" | docker login "${{ secrets.REGISTRY }}" -u "${{ secrets.REGISTRY_USER }}" --password-stdin
          docker build -t "$TAG" .
          docker push "$TAG"
          echo "TAG=$TAG" >> "$GITHUB_ENV"

      # 2. Install the teploy CLI.
      - name: Install teploy
        run: |
          curl -fsSL https://github.com/useteploy/teploy-cli/releases/latest/download/teploy_linux_amd64.tar.gz | tar xz
          sudo mv teploy /usr/local/bin/

      # 3. Deploy over SSH (zero-downtime). teploy.yml supplies app config.
      - name: Deploy
        run: |
          install -m600 /dev/stdin deploy_key <<< "${{ secrets.TEPLOY_SSH_KEY }}"
          teploy deploy \
            --host "${{ secrets.DEPLOY_HOST }}" \
            --user "${{ secrets.DEPLOY_USER }}" \
            --key deploy_key \
            --image "$TAG"
```

## GitHub Actions

Identical, at `.github/workflows/deploy.yml` — only the trigger context vars
differ in name; `${{ secrets.* }}`, `$GITHUB_SHA`, and `$GITHUB_ENV` are the
same on both. The workflow above works verbatim on GitHub Actions.

## Required secrets

Set these in your repo's **Settings → Secrets**:

| Secret | What it is |
|---|---|
| `REGISTRY` | registry host, e.g. `ghcr.io/you` or your Forgejo registry |
| `REGISTRY_USER` / `REGISTRY_PASSWORD` | push credentials for the build step |
| `DEPLOY_HOST` | the server's IP/hostname |
| `DEPLOY_USER` | SSH user (often `root` or a deploy user) |
| `TEPLOY_SSH_KEY` | the **private** SSH key authorized on the server |

The SSH key must be authorized on the server (its public half in the deploy
user's `~/.ssh/authorized_keys`). Use a dedicated deploy key, not a personal one.

## Multi-server

`--host` targets one server. For a fleet, drop `--host/--user/--key`, commit a
`servers.yml` step (or bake it from a secret), and let `teploy deploy` fan out
across the `servers:` list with your rollout policy — same as a local deploy.

## Webhook alternative

If you'd rather not put deploy secrets in CI, use the built-in webhook: the box
listens and redeploys itself on push.

```
teploy autodeploy setup <server>     # prints a webhook URL + secret
```

Add that URL as a **push webhook** in your Forgejo/GitHub repo settings. Every
push then triggers a redeploy on the server — no CI job, no SSH key in CI. The
trade-off: no test-gate (the box just rebuilds), and the build happens on the
server rather than in CI. `teploy autodeploy status` / `remove` manage it.
