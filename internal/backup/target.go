package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// One backup target per server (D3): bucket, region, endpoint and an
// optional key prefix, stored ON the server and read at run time so no
// crontab line or laptop environment has to carry them.
//
// The file lives outside /deployments (which Caddy mounts read-only into
// untrusted containers): /etc/teploy/backup-target.json, mode 0600. A
// non-root deploy user that cannot write /etc/teploy falls back to
// ~/.config/teploy/backup-target.json and the command says so.
//
// Credentials, when given, are age-encrypted with the server's own age
// identity and stored INSIDE the JSON as the "credentials" field. The
// plaintext never touches the client's disk, argv, or the crontab; at use
// time S3Config.AWS decrypts and exports them inside the command's shell.
// The identity is resolved by try-order (/etc/teploy/age-key, then
// /deployments/.age-key) so it follows the same move the secret store's
// identity is making; a fresh identity is only created when NEITHER path
// holds one, and then at /deployments/.age-key — the pre-migration
// canonical location — so a server never grows two identities.

const (
	// TargetFileEtc is the preferred backup-target location.
	TargetFileEtc = "/etc/teploy/backup-target.json"
	// TargetFileHome is the non-root fallback location.
	TargetFileHome = ".config/teploy/backup-target.json"

	ageIdentityEtc  = "/etc/teploy/age-key"
	ageIdentityFarm = "/deployments/.age-key"
)

// Target is the non-secret half of the stored backup target plus a flag
// saying whether encrypted credentials travel with it.
type Target struct {
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Endpoint string `json:"endpoint,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	// HasCredentials reports whether the stored file carries an encrypted
	// credentials blob. The blob itself is never decoded into this struct.
	HasCredentials bool `json:"-"`
	// Path is the server path the target was read from (diagnostics only).
	Path string `json:"-"`
}

// targetFile is the on-disk shape. Credentials holds age armor (empty when
// the target relies on the server's ambient AWS credential chain).
type targetFile struct {
	V           string `json:"v"`
	Bucket      string `json:"bucket"`
	Region      string `json:"region"`
	Endpoint    string `json:"endpoint"`
	Prefix      string `json:"prefix"`
	Credentials string `json:"credentials,omitempty"`
}

// credentialValueRE is the charset a stored S3 access key or secret may
// use. The plaintext is evaluated by the shell at use time
// (S3Config.AWS), so anything that could end a word or quote a string is
// refused here rather than escaped.
var credentialValueRE = regexp.MustCompile(`^[A-Za-z0-9/+_.=:@-]{1,255}$`)

// ValidateCredential refuses values the run-time eval path cannot treat as
// a single safe shell word.
func ValidateCredential(kind, value string) error {
	if !credentialValueRE.MatchString(value) {
		return fmt.Errorf("%s must be 1-255 characters of letters, digits and / + _ . = : @ - (no spaces, quotes or shell metacharacters)", kind)
	}
	return nil
}

var endpointRE = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~/=-]*)?$`)

// ValidateEndpoint accepts an http(s) URL only: the value is interpolated
// into aws invocations server-side.
func ValidateEndpoint(endpoint string) error {
	if !endpointRE.MatchString(endpoint) || strings.ContainsAny(endpoint, " \t\r\n'\"\\") {
		return fmt.Errorf("endpoint must be an http:// or https:// URL without spaces or quotes")
	}
	return nil
}

var prefixSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidatePrefix accepts slash-joined safe segments ("/" is not a leading
// or trailing possibility).
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) > 200 || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") || strings.Contains(prefix, "//") {
		return fmt.Errorf("prefix must be slash-joined segments without leading/trailing slashes (max 200 chars)")
	}
	for _, seg := range strings.Split(prefix, "/") {
		if !prefixSegmentRE.MatchString(seg) {
			return fmt.Errorf("prefix segment %q must be alphanumeric with dots, dashes or underscores", seg)
		}
	}
	return nil
}

// ReadTarget loads the server's stored backup target. A server with no
// target is (Target{}, nil) — the normal "not configured yet" case, which
// callers turn into the command-appropriate error.
func ReadTarget(ctx context.Context, exec ssh.Executor) (Target, error) {
	res := ssh.RunDetailed(ctx, exec, readTargetScript)
	if res.Err != nil {
		return Target{}, fmt.Errorf("reading the backup target: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return Target{}, fmt.Errorf("reading the backup target: %s", res.ExitErrorText())
	}
	out := strings.TrimSpace(string(res.Stdout))
	if out == "absent" {
		return Target{}, nil
	}
	var t Target
	line, rest, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(line, "path ") {
		return Target{}, fmt.Errorf("reading the backup target: unrecognized output framing")
	}
	t.Path = strings.TrimSpace(strings.TrimPrefix(line, "path "))
	var f targetFile
	if err := json.Unmarshal([]byte(rest), &f); err != nil {
		return Target{}, fmt.Errorf("parsing %s: %w", t.Path, err)
	}
	if f.V != "1" {
		return Target{}, fmt.Errorf("%s: unsupported backup target version %q", t.Path, f.V)
	}
	t.Bucket, t.Region, t.Endpoint, t.Prefix = f.Bucket, f.Region, f.Endpoint, f.Prefix
	t.HasCredentials = f.Credentials != ""
	return t, nil
}

// readTargetScript prints "absent" or "path <p>\n<file contents>". One
// framed read (exit-status-checked, absence distinguished from failure).
const readTargetScript = `for p in /etc/teploy/backup-target.json "$HOME"/.config/teploy/backup-target.json; do
if [ -r "$p" ]; then printf 'path %s\n' "$p"; cat "$p"; exit 0; fi
done
echo absent`

// WriteTarget stores the target (and, when creds is non-empty, the
// age-encrypted credentials blob) on the server and returns the path
// written plus whether the non-root fallback location was used.
func WriteTarget(ctx context.Context, exec ssh.Executor, t Target, credsArmor string) (path string, fallback bool, err error) {
	f := targetFile{V: "1", Bucket: t.Bucket, Region: t.Region, Endpoint: t.Endpoint, Prefix: t.Prefix, Credentials: credsArmor}
	payload, err := json.Marshal(f)
	if err != nil {
		return "", false, err
	}
	res := ssh.RunInputDetailed(ctx, exec, writeTargetScript, strings.NewReader(string(payload)))
	if res.Err != nil {
		return "", false, fmt.Errorf("writing the backup target: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return "", false, fmt.Errorf("writing the backup target: %s", res.ExitErrorText())
	}
	out := strings.Fields(strings.TrimSpace(string(res.Stdout)))
	// "written <path>" on stdout; "fallback" on stderr selects the report.
	fallback = strings.Contains(string(res.Stderr), "fallback")
	if len(out) != 2 || out[0] != "written" {
		return "", fallback, fmt.Errorf("writing the backup target: unrecognized output framing")
	}
	return out[1], fallback, nil
}

// writeTargetScript reads the JSON payload from stdin and publishes it
// atomically at 0600. Root (or a user with a writable /etc/teploy) writes
// the preferred location; anyone else falls back to ~/.config/teploy and
// the script says "fallback" on stderr so the command can tell the user.
const writeTargetScript = `d=
if [ "$(id -u)" = 0 ]; then
  mkdir -p /etc/teploy && chmod 700 /etc/teploy || exit 1
  d=/etc/teploy/backup-target.json
elif [ -w /etc/teploy ] 2>/dev/null; then
  d=/etc/teploy/backup-target.json
else
  dir=${XDG_CONFIG_HOME:-$HOME/.config}/teploy
  mkdir -p "$dir" && chmod 700 "$dir" || exit 1
  d=$dir/backup-target.json
  echo fallback >&2
fi
umask 077
t=$(mktemp "$(dirname "$d")/.backup-target.XXXXXXXX") || exit 1
cat > "$t" || { rm -f -- "$t"; exit 1; }
chmod 600 "$t" || { rm -f -- "$t"; exit 1; }
mv -f -- "$t" "$d" || { rm -f -- "$t"; exit 1; }
printf 'written %s\n' "$d"`

// UnsetTarget removes the stored target from every location it may occupy.
// It reports the paths removed (empty when nothing was configured).
func UnsetTarget(ctx context.Context, exec ssh.Executor) ([]string, error) {
	res := ssh.RunDetailed(ctx, exec, unsetTargetScript)
	if res.Err != nil {
		return nil, fmt.Errorf("removing the backup target: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("removing the backup target: %s", res.ExitErrorText())
	}
	var removed []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "removed ") {
			removed = append(removed, strings.TrimSpace(strings.TrimPrefix(line, "removed ")))
		}
	}
	return removed, nil
}

const unsetTargetScript = `for p in /etc/teploy/backup-target.json "$HOME"/.config/teploy/backup-target.json; do
if [ -e "$p" ]; then rm -f -- "$p" || exit 1; echo "removed $p"; fi
done`

// EncryptCredentials encrypts the two-line credentials payload (the
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY export lines S3Config.AWS
// evaluates) to the server's age identity and returns the armor. The
// plaintext travels only over the SSH channel as stdin.
func EncryptCredentials(ctx context.Context, exec ssh.Executor, accessKey, secretKey string) (string, error) {
	if err := ValidateCredential("access key", accessKey); err != nil {
		return "", err
	}
	if err := ValidateCredential("secret key", secretKey); err != nil {
		return "", err
	}
	idPath, err := ensureAgeIdentity(ctx, exec)
	if err != nil {
		return "", err
	}
	recipient, err := exec.Run(ctx, "age-keygen -y "+ssh.ShellQuote(idPath))
	if err != nil {
		return "", fmt.Errorf("reading the age identity's recipient: %w", err)
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" || strings.Contains(recipient, "\n") {
		return "", fmt.Errorf("age-keygen returned an unusable recipient")
	}
	// export is required: the eval runs in the invocation's own shell and
	// the assignments must reach the exec'd aws child (bare assignments
	// would stay unexported shell variables).
	plaintext := "export AWS_ACCESS_KEY_ID=" + accessKey + "\nexport AWS_SECRET_ACCESS_KEY=" + secretKey + "\n"
	res := ssh.RunInputDetailed(ctx, exec, "age -r "+ssh.ShellQuote(recipient)+" -a", strings.NewReader(plaintext))
	if res.Err != nil {
		return "", fmt.Errorf("encrypting the credentials: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("encrypting the credentials: %s", res.ExitErrorText())
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// ensureAgeIdentity resolves the server's age identity with the W2 move
// in mind (/etc/teploy/age-key first, /deployments/.age-key as the
// pre-migration location) and creates one at the legacy path only when
// neither exists — so a server never ends up with two identities.
func ensureAgeIdentity(ctx context.Context, exec ssh.Executor) (string, error) {
	for _, p := range []string{ageIdentityEtc, ageIdentityFarm} {
		out, err := exec.Run(ctx, fmt.Sprintf("if [ -r %s ]; then echo yes; else echo no; fi", ssh.ShellQuote(p)))
		if err != nil {
			return "", fmt.Errorf("checking %s: %w", p, err)
		}
		if strings.TrimSpace(out) == "yes" {
			return p, nil
		}
	}
	if _, err := exec.Run(ctx, fmt.Sprintf("umask 077; age-keygen -o %s 2>/dev/null && chmod 600 %s", ssh.ShellQuote(ageIdentityFarm), ssh.ShellQuote(ageIdentityFarm))); err != nil {
		return "", fmt.Errorf("generating an age identity at %s: %w", ageIdentityFarm, err)
	}
	return ageIdentityFarm, nil
}

// StoredCredsPrefix is the shell prelude S3Config.AWS uses when the config
// carries a stored target: it decrypts the credentials blob inside the
// command's own shell and exports it, failing closed when decryption
// yields nothing (so a broken target never silently falls back to the
// ambient AWS chain). The identity try-order matches ensureAgeIdentity.
func StoredCredsPrefix(jsonPath string) string {
	id := `$(if [ -r /etc/teploy/age-key ]; then printf '%s' /etc/teploy/age-key; else printf '%s' /deployments/.age-key; fi)`
	return fmt.Sprintf(
		`( c=$(python3 -c 'import json,sys; sys.stdout.write(json.load(open(sys.argv[1])).get("credentials") or "")' %s | age -d -i "%s") && [ -n "$c" ] && eval "$c" && exec `,
		ssh.ShellQuote(jsonPath), id,
	)
}
