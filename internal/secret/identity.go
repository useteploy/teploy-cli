package secret

// identity.go — where the server's age identity lives (server-authority
// plan D2, security item). The identity used to sit at
// /deployments/.age-key, a tree the Caddy container mounts read-only, so
// a Caddy compromise could read every secret. It now lives at
// /etc/teploy/age-key (directory 0700, key 0600). Every reader and writer
// of the identity resolves the location through this file: new path
// first, legacy path second, so an install can migrate with no
// coordinated outage and a non-root deploy user that cannot write /etc
// keeps working on the legacy path.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	// IdentityDir holds the age identity and nothing else.
	IdentityDir = "/etc/teploy"
	// IdentityPath is the age identity's home (D2).
	IdentityPath = IdentityDir + "/age-key"
	// LegacyIdentityPath is the pre-D2 location, still read as a fallback
	// and still written when /etc/teploy cannot be created (non-root
	// deploy users on unmigrated setups).
	LegacyIdentityPath = deploymentsDir + "/.age-key"
)

// locateIdentity outcomes.
const (
	identityNew    = "new"    // readable at IdentityPath
	identityLegacy = "legacy" // a private key at LegacyIdentityPath
	identityMoved  = "moved"  // legacy file is the move marker: the identity is at IdentityPath but unreadable by this user
	identityAbsent = "absent" // no identity anywhere
)

// locateIdentityScript frames its answer as one word. The legacy file is
// only "legacy" while it still holds a private key; after migration it is
// a public-key marker (see MigrateIdentity), which must read as "moved"
// so a user who cannot traverse IdentityDir gets an explicit error
// instead of a silent attempt to generate a brand-new key.
var locateIdentityScript = `if [ -r ` + shell(IdentityPath) + ` ]; then printf 'new\n'; elif grep -q '^AGE-SECRET-KEY-' ` + shell(LegacyIdentityPath) + ` 2>/dev/null; then printf 'legacy\n'; elif [ -e ` + shell(LegacyIdentityPath) + ` ]; then printf 'moved\n'; else printf 'absent\n'; fi`

// markerNote is the body (besides the public key) MigrateIdentity leaves
// at the legacy path.
const markerNote = "# teploy: the age identity moved to " + IdentityPath + "; this file is not an identity"

func locateIdentity(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, locateIdentityScript)
	if err != nil {
		return "", fmt.Errorf("locating the age identity: %w", err)
	}
	switch strings.TrimSpace(out) {
	case identityNew, identityLegacy, identityMoved, identityAbsent:
		return strings.TrimSpace(out), nil
	}
	return "", fmt.Errorf("locating the age identity: unrecognized output framing")
}

// HasLegacyIdentity reports whether a private key still lives at the
// legacy path — setup's cheap "is the migration needed at all" probe, so
// a fresh or already-migrated server adds no work to `teploy setup`.
func HasLegacyIdentity(ctx context.Context, exec ssh.Executor) (bool, error) {
	out, err := exec.Run(ctx, `if [ -r `+shell(LegacyIdentityPath)+` ] && grep -q '^AGE-SECRET-KEY-' `+shell(LegacyIdentityPath)+` 2>/dev/null; then printf 'yes\n'; else printf 'no\n'; fi`)
	if err != nil {
		return false, fmt.Errorf("checking the age identity location: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	return false, fmt.Errorf("checking the age identity location: unrecognized output framing")
}

// ResolveIdentity returns the readable identity path on the target, ""
// when none exists. It is the one helper every reader outside this
// package (DR bundles) uses, so the location never forks.
func ResolveIdentity(ctx context.Context, exec ssh.Executor) (string, error) {
	loc, err := locateIdentity(ctx, exec)
	if err != nil {
		return "", err
	}
	switch loc {
	case identityNew:
		return IdentityPath, nil
	case identityLegacy:
		return LegacyIdentityPath, nil
	}
	return "", nil
}

// identityUnavailableError explains the one state a reader cannot recover
// from alone: the identity exists at IdentityPath but this user cannot
// read it.
func identityUnavailableError() error {
	return fmt.Errorf("the age identity has moved to %s, which this user cannot read — have an administrator run `teploy setup` on the server (or make %s readable by the deploy user)", IdentityPath, IdentityDir)
}

// generateIdentityScript creates a fresh identity when none exists,
// preferring /etc/teploy and falling back to the legacy path when /etc is
// not writable (non-root first runs). Prints the path it chose.
var generateIdentityScript = `umask 077; p=''; if mkdir -p ` + shell(IdentityDir) + ` 2>/dev/null && chmod 700 ` + shell(IdentityDir) + ` 2>/dev/null && age-keygen -o ` + shell(IdentityPath) + ` 2>/dev/null && chmod 600 ` + shell(IdentityPath) + `; then p='` + IdentityPath + `'; elif mkdir -p ` + shell(deploymentsDir) + ` 2>/dev/null && age-keygen -o ` + shell(LegacyIdentityPath) + ` 2>/dev/null && chmod 600 ` + shell(LegacyIdentityPath) + `; then p='` + LegacyIdentityPath + `'; fi; [ -n "$p" ] && printf '%s\n' "$p"`

// ensureIdentity returns the path of an existing identity, generating one
// when none exists anywhere.
func ensureIdentity(ctx context.Context, exec ssh.Executor) (string, error) {
	loc, err := locateIdentity(ctx, exec)
	if err != nil {
		return "", err
	}
	switch loc {
	case identityNew:
		return IdentityPath, nil
	case identityLegacy:
		return LegacyIdentityPath, nil
	case identityMoved:
		return "", identityUnavailableError()
	}
	out, err := exec.Run(ctx, generateIdentityScript)
	if err != nil {
		return "", fmt.Errorf("generating age key: %w", err)
	}
	switch p := strings.TrimSpace(out); p {
	case IdentityPath, LegacyIdentityPath:
		return p, nil
	}
	return "", fmt.Errorf("generating age key: no identity was created (is age-keygen installed?)")
}

// anyIdentity reports whether an identity file exists at either location,
// readable or not — the "never replace an existing key" check.
var anyIdentityScript = `[ -e ` + shell(IdentityPath) + ` ] || [ -e ` + shell(LegacyIdentityPath) + ` ]`

func anyIdentity(ctx context.Context, exec ssh.Executor) (bool, error) {
	out, err := exec.Run(ctx, anyIdentityScript+` && printf 'yes\n' || printf 'no\n'`)
	if err != nil {
		return false, fmt.Errorf("checking for an existing age identity: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	return false, fmt.Errorf("checking for an existing age identity: unrecognized output framing")
}

// InstallBundledKey installs a bundled age key (DR restore) as the
// target's own identity ONLY when no identity exists at either location,
// preferring /etc/teploy. Returns the path written, "" when nothing was
// installed. The install uses ln (create-only) against the final path so
// a concurrent publisher can never overwrite an existing key.
func InstallBundledKey(ctx context.Context, exec ssh.Executor, src string) (string, error) {
	exists, err := anyIdentity(ctx, exec)
	if err != nil {
		return "", err
	}
	if exists {
		return "", nil
	}
	cmd := fmt.Sprintf(installBundledKeyScript, shell(src))
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("installing the bundled age key: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// installBundledKeyScript installs a key file as the target's own
// identity, preferring /etc/teploy and falling back to /deployments
// when that directory cannot be made writable. ln (create-only) against
// the final path makes an existing key a FAILURE, never a replacement —
// the retry below only covers the directory choice, and only before
// anything was created.
var installBundledKeyScript = `umask 077; d='` + IdentityDir + `'; if ! mkdir -p "$d" 2>/dev/null || ! chmod 700 "$d" 2>/dev/null; then d='` + deploymentsDir + `'; fi; mkdir -p "$d" || exit 1; t=$(mktemp "$d/.age-key.XXXXXXXX") || exit 1; trap 'rm -f -- "$t"' EXIT HUP INT TERM; cp -- %s "$t" && chmod 600 "$t" && ln "$t" "$d/age-key" || exit 1; printf '%%s\n' "$d/age-key"`

// MigrateIdentity moves a legacy identity to /etc/teploy/age-key (D2).
// sudo prefixes privileged commands for non-root setup runs. The move is
// verified before anything is removed: the copy must parse as an age
// identity, derive the same public key, and decrypt one existing secret
// when any exist. The legacy file is then REPLACED BY A PUBLIC-KEY
// MARKER rather than deleted: an older teploy still on some machine
// reads the marker's "# public key:" line and keeps encrypting to the
// same identity — deleting the file outright would make it silently
// generate a new key and fork the store. Returns a human-readable action
// line.
func MigrateIdentity(ctx context.Context, exec ssh.Executor, w io.Writer, sudo string) (string, error) {
	loc, err := locateIdentity(ctx, exec)
	if err != nil {
		return "", err
	}
	switch loc {
	case identityNew, identityMoved, identityAbsent:
		if loc == identityNew {
			// Present at both? Verify they name the same key and drop the
			// legacy copy down to the marker.
			out, err := exec.Run(ctx, bothKeysScript)
			if err != nil {
				return "", fmt.Errorf("comparing age identities: %w", err)
			}
			switch strings.TrimSpace(out) {
			case "legacy-marker", "legacy-absent":
				return "age identity already at " + IdentityPath, nil
			case "same-key":
				if err := writeMarker(ctx, exec, sudo); err != nil {
					return "", err
				}
				return "duplicate legacy age key replaced with a public-key marker", nil
			case "different-keys":
				fmt.Fprintf(w, "  Warning: %s and %s hold DIFFERENT keys — nothing was changed; resolve which one is real (the store's secrets decrypt with it) and re-run setup\n", IdentityPath, LegacyIdentityPath)
				return "left alone: two different age keys exist", nil
			}
			return "", fmt.Errorf("comparing age identities: unrecognized output framing")
		}
		if loc == identityAbsent {
			return "no age identity on this server yet (created on first use)", nil
		}
		return "age identity already at " + IdentityPath, nil
	}

	// Legacy key present: try the move.
	res, err := exec.Run(ctx, fmt.Sprintf(migrateScript, shell(sudo)))
	if err != nil {
		return "", fmt.Errorf("moving the age identity to %s: %w", IdentityPath, err)
	}
	switch strings.TrimSpace(res) {
	case "migrated":
		if err := writeMarker(ctx, exec, sudo); err != nil {
			return "", err
		}
		return "moved the age identity from " + LegacyIdentityPath + " to " + IdentityPath + " (decrypt verified; a public-key marker remains at the old path)", nil
	case "not-writable":
		fmt.Fprintf(w, "  Warning: %s is not writable by this user — the age identity stays at %s, which the Caddy container mounts read-only; re-run setup with privileges that can create %s\n", IdentityDir, LegacyIdentityPath, IdentityDir)
		return "kept the legacy age identity (cannot write " + IdentityDir + ")", nil
	case "not-an-identity", "key-mismatch", "decrypt-failed":
		fmt.Fprintf(w, "  Warning: the legacy age key did not verify (%s) — nothing was changed\n", strings.TrimSpace(res))
		return "kept the legacy age identity (verification failed: " + strings.TrimSpace(res) + ")", nil
	}
	return "", fmt.Errorf("moving the age identity: unrecognized output framing")
}

// bothKeysScript compares the two locations when both hold content.
// Outputs legacy-marker | legacy-absent | same-key | different-keys.
var bothKeysScript = `if [ ! -r ` + shell(LegacyIdentityPath) + ` ]; then printf 'legacy-absent\n'; elif ! grep -q '^AGE-SECRET-KEY-' ` + shell(LegacyIdentityPath) + ` 2>/dev/null; then printf 'legacy-marker\n'; else a=$(grep 'public key:' ` + shell(LegacyIdentityPath) + ` | awk '{print $NF}' | head -n1); b=$(grep 'public key:' ` + shell(IdentityPath) + ` | awk '{print $NF}' | head -n1); [ -n "$a" ] && [ "$a" = "$b" ] && printf 'same-key\n' || printf 'different-keys\n'; fi`

// migrateScript does the verified move of the legacy key into
// /etc/teploy/age-key. %s is the (possibly empty) sudo prefix. Every
// failure path leaves both files exactly as they were; only a fully
// verified copy reaches the final mv. With sudo, the directory is
// created and handed to the deploying user (setup runs as the deploy
// user); without privileges the script reports not-writable and the
// legacy key keeps serving reads.
var migrateScript = `set -e; umask 077; %smkdir -p ` + shell(IdentityDir) + ` 2>/dev/null || true; if [ -d ` + shell(IdentityDir) + ` ] && [ ! -w ` + shell(IdentityDir) + ` ]; then %schown $(id -u):$(id -g) ` + shell(IdentityDir) + ` 2>/dev/null || true; fi; if [ ! -d ` + shell(IdentityDir) + ` ] || [ ! -w ` + shell(IdentityDir) + ` ]; then printf 'not-writable\n'; exit 0; fi; chmod 700 ` + shell(IdentityDir) + `; t=$(mktemp ` + shell(IdentityDir+"/.age-key.XXXXXXXX") + `); trap 'rm -f -- "$t"' EXIT HUP INT TERM; cp -- ` + shell(LegacyIdentityPath) + ` "$t" && chmod 600 "$t"; age-keygen -y "$t" >/dev/null 2>&1 || { printf 'not-an-identity\n'; exit 0; }; a=$(grep 'public key:' ` + shell(LegacyIdentityPath) + ` | awk '{print $NF}' | head -n1); b=$(age-keygen -y "$t" 2>/dev/null | head -n1); [ -n "$a" ] && [ "$a" = "$b" ] || { printf 'key-mismatch\n'; exit 0; }; c=$(find ` + shell(deploymentsDir) + ` -maxdepth 3 -type f -name '*.age' 2>/dev/null | head -n1); [ -z "$c" ] || age -d -i "$t" -- "$c" >/dev/null 2>&1 || { printf 'decrypt-failed\n'; exit 0; }; mv -fT -- "$t" ` + shell(IdentityPath) + `; trap - EXIT HUP INT TERM; printf 'migrated\n'`

// writeMarkerScript replaces the legacy key file with the public-key
// marker. %s is the (possibly empty) sudo prefix. The public key is read
// from the live identity; the marker is world readable because it holds
// public information only.
var writeMarkerScript = `%schown $(id -u):$(id -g) ` + shell(LegacyIdentityPath) + ` 2>/dev/null || true; p=$(grep 'public key:' ` + shell(IdentityPath) + ` 2>/dev/null | awk '{print $NF}' | head -n1); t=$(mktemp ` + shell(deploymentsDir+"/.age-key.XXXXXXXX") + `) || exit 1; trap 'rm -f -- "$t"' EXIT HUP INT TERM; { printf '%%s\n' '` + markerNote + `'; [ -n "$p" ] && printf '# public key: %%s\n' "$p"; } > "$t" && chmod 644 "$t" && mv -fT -- "$t" ` + shell(LegacyIdentityPath)

// writeMarker replaces the legacy key file with the public-key marker.
// The public key is read from the live identity; the marker is world
// readable because it holds public information only.
func writeMarker(ctx context.Context, exec ssh.Executor, sudo string) error {
	if _, err := exec.Run(ctx, fmt.Sprintf(writeMarkerScript, sudo)); err != nil {
		return fmt.Errorf("leaving the public-key marker at %s: %w", LegacyIdentityPath, err)
	}
	return nil
}

func shell(s string) string { return ssh.ShellQuote(s) }
