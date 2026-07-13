# Secrets scanning: keep credentials out of git

Teploy deliberately does **not** ship a CI runner — it integrates with the one
you already run (Forgejo Actions, GitHub Actions). Secrets scanning is the same
philosophy: this is a recipe you drop into your CI, not a Teploy service.

The risk it closes: an API key, database URL, or private key committed to git
stays in history forever, even after you delete it in a later commit. A scanner
that runs on every push catches it at the door.

The tool is [Gitleaks](https://github.com/gitleaks/gitleaks) — a single Go
binary, MIT-licensed, no runtime deps. Same shape as Teploy itself.

## Forgejo Actions

Drop this at `.forgejo/workflows/secrets-scan.yml`:

```yaml
name: secrets-scan
on: [push, pull_request]

jobs:
  gitleaks:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # full history — scan every commit, not just the tip
      - name: Install gitleaks
        run: |
          VERSION=8.21.2
          curl -sSL "https://github.com/gitleaks/gitleaks/releases/download/v${VERSION}/gitleaks_${VERSION}_linux_x64.tar.gz" \
            | tar -xz -C /usr/local/bin gitleaks
      - name: Scan
        run: gitleaks detect --source . --redact --verbose --exit-code 1
```

`--exit-code 1` fails the job on any finding; `--redact` keeps the secret itself
out of the CI logs (a scanner that prints the leak defeats the point).

## GitHub Actions

The official action, at `.github/workflows/secrets-scan.yml`:

```yaml
name: secrets-scan
on: [push, pull_request]

jobs:
  gitleaks:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: gitleaks/gitleaks-action@v2
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

## Catch it before it's committed

CI is the backstop; the cheapest fix is never committing the secret. A local
pre-commit hook scans only staged changes (fast):

```bash
# .git/hooks/pre-commit  (chmod +x)
#!/bin/sh
gitleaks protect --staged --redact --exit-code 1
```

Or via the [pre-commit](https://pre-commit.com) framework in
`.pre-commit-config.yaml`:

```yaml
repos:
  - repo: https://github.com/gitleaks/gitleaks
    rev: v8.21.2
    hooks:
      - id: gitleaks
```

## Tuning

- **False positives / allowlist:** commit a `.gitleaks.toml` with an
  `[allowlist]` (regexes, paths, or commit SHAs) — e.g. to exclude test
  fixtures that contain fake keys.
- **A pre-existing leak in history:** rotate the credential first (assume it's
  compromised), then scrub history with `git filter-repo` if needed. Scanning
  doesn't un-leak an exposed secret — rotation does.

## Where Teploy's own secrets belong

Scanning keeps secrets out of git; it doesn't store them. For the app's runtime
secrets, use `teploy secret set` (encrypted, injected at deploy) or the
`env_files:` block with SOPS/age-encrypted files — never plaintext in the repo.
