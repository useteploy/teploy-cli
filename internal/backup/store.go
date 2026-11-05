package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/useteploy/teploy/internal/ssh"
)

// The object store layer (Slice 4). Every backup artifact and sidecar goes
// through put and comes back through fetch, so the three guarantees below hold
// for every engine, sidecar and DR member alike:
//
//   - Encryption: when the policy names an owner recovery key, the object is
//     age-encrypted to that key AND the server's own identity, the ciphertext
//     is proven to decrypt back to the plaintext before anything leaves the
//     server, and only the ciphertext is uploaded (.age suffix). A failure to
//     encrypt fails the backup; it never falls back to plaintext.
//
//   - A final key means a verified object: the upload goes to a .incoming/ key
//     beside the final one, its size and checksum are checked there, and only
//     then is it copied server-side to its final name. A killed run therefore
//     can never leave a partial or unverified object where list, latest,
//     restore or prune would take it for a backup. Leftover .incoming/ objects
//     are invisible to those, and pruned by the next run.
//
//   - Commit order: a backup's sidecars and extra databases are stored BEFORE
//     its primary object, so a primary implies everything that belongs to it.

// ageSuffix marks an object encrypted with age.
const ageSuffix = ".age"

// incomingDir holds uploads until they are verified.
const incomingDir = ".incoming"

// readBackMax is the largest object whose bytes are read back and hashed after
// upload; above it the stored size and the checksum recorded at upload stand
// in (plus the aws CLI's own transfer checksums). Bandwidth, not safety.
const readBackMax = 256 << 20

// agelessFloorOverhead is what age adds to a tiny object (header with two
// X25519 stanzas plus the payload nonce and tag), so a floor measured on
// plaintext is not silently failed or passed by ciphertext.
const ageFloorOverhead = 200

// recipientRE is an X25519 age recipient: the only form teploy generates.
var recipientRE = regexp.MustCompile(`^age1[0-9a-z]{58}$`)

// ValidateRecipient checks an owner recovery public key.
func ValidateRecipient(r string) error {
	if !recipientRE.MatchString(r) {
		return fmt.Errorf("recipient must be an age public key (age1..., from `teploy backup keygen`)")
	}
	if _, err := age.ParseX25519Recipient(r); err != nil {
		return fmt.Errorf("recipient is not a valid age public key: %w", err)
	}
	return nil
}

// Encrypted reports whether objects written with this config are encrypted.
func (s3 S3Config) Encrypted() bool { return s3.OwnerRecipient != "" }

// objectExtra is what put reports about a stored object.
type storedObject struct {
	Key       string
	Size      int64
	SHA256    string
	Encrypted bool
}

// serverRecipient returns the server's own age public key, creating the
// identity if the server has none (shared with stored target credentials and
// the secret store, so a server never grows two).
func (c *Client) serverRecipient(ctx context.Context) (string, error) {
	if c.serverRecip != "" {
		return c.serverRecip, nil
	}
	if out, err := c.exec.Run(ctx, "command -v age >/dev/null 2>&1 && command -v age-keygen >/dev/null 2>&1 && echo ok"); err != nil || strings.TrimSpace(out) != "ok" {
		return "", fmt.Errorf("the age tool is not installed on this server, so nothing can be encrypted (run `teploy setup`, or apt-get install age)")
	}
	id, err := ensureAgeIdentity(ctx, c.exec)
	if err != nil {
		return "", err
	}
	out, err := c.exec.Run(ctx, "age-keygen -y "+ssh.ShellQuote(id))
	if err != nil {
		return "", fmt.Errorf("reading the server's age identity %s: %w", id, err)
	}
	r := strings.TrimSpace(out)
	if !recipientRE.MatchString(r) {
		return "", fmt.Errorf("the server's age identity %s did not yield a usable public key", id)
	}
	c.serverRecip = r
	return r, nil
}

// PrepareEncryption is prepareEncryption for a run's own preflight.
func (c *Client) PrepareEncryption(ctx context.Context, s3 S3Config) error {
	return c.prepareEncryption(ctx, s3)
}

// prepareEncryption fails BEFORE any dump runs when the policy asks for
// encryption this server cannot do.
func (c *Client) prepareEncryption(ctx context.Context, s3 S3Config) error {
	if !s3.Encrypted() {
		return nil
	}
	if err := ValidateRecipient(s3.OwnerRecipient); err != nil {
		return err
	}
	_, err := c.serverRecipient(ctx)
	return err
}

// encryptLocal encrypts local in place to local.age, proves the ciphertext
// decrypts back to the plaintext with the server's identity, and removes the
// plaintext. Files, not pipes: a pipeline reports only its last command's
// status, and a truncated stream would upload as a "backup".
func (c *Client) encryptLocal(ctx context.Context, s3 S3Config, local string) (string, error) {
	server, err := c.serverRecipient(ctx)
	if err != nil {
		return "", err
	}
	q := ssh.ShellQuote
	enc := local + ageSuffix
	id := ageIdentityShell
	cmd := fmt.Sprintf("age -r %s -r %s -o %s %s", q(s3.OwnerRecipient), q(server), q(enc), q(local))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(enc))
		return "", fmt.Errorf("encrypting the backup (nothing was uploaded): %w", err)
	}
	// cmp reads the decrypted stream to its end and compares it with the
	// plaintext: a decrypt failure shows up as a short stream, so losing age's
	// exit status in this pipe cannot hide anything.
	check := fmt.Sprintf(`age -d -i "%s" %s | cmp -s - %s`, id, q(enc), q(local))
	if _, err := c.exec.Run(ctx, check); err != nil {
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(enc))
		return "", fmt.Errorf("the encrypted backup did not decrypt back to its plaintext with the server's identity (nothing was uploaded): %w", err)
	}
	if _, err := c.exec.Run(ctx, "rm -f "+q(local)); err != nil {
		return "", fmt.Errorf("removing the plaintext after encrypting: %w", err)
	}
	return enc, nil
}

// ageIdentityShell expands to the server's identity path (same try-order as
// ensureAgeIdentity). It is a command substitution, for use inside "...".
const ageIdentityShell = `$(if [ -r /etc/teploy/age-key ]; then printf '%s' /etc/teploy/age-key; else printf '%s' /deployments/.age-key; fi)`

// localFacts measures a file: size and SHA-256, in one command.
func (c *Client) localFacts(ctx context.Context, local string) (int64, string, error) {
	q := ssh.ShellQuote(local)
	out, err := c.exec.Run(ctx, "sha256sum "+q+" && wc -c < "+q)
	if err != nil {
		return 0, "", fmt.Errorf("measuring %s: %w", path.Base(local), err)
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return 0, "", fmt.Errorf("measuring %s: unreadable output", path.Base(local))
	}
	size, err := strconv.ParseInt(f[len(f)-1], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("measuring %s: unreadable size %q", path.Base(local), f[len(f)-1])
	}
	return size, f[0], nil
}

// incomingKey is the verification-pending name of a final key.
func incomingKey(key string) string {
	dir, file := path.Split(key)
	return dir + incomingDir + "/" + file
}

// objectHead is the stored object's size and recorded checksum.
type objectHead struct {
	Size   int64
	SHA256 string
}

// head reads an object's size and recorded checksum.
func (c *Client) head(ctx context.Context, s3 S3Config, key string) (objectHead, error) {
	bucket, k, err := splitS3URI(key)
	if err != nil {
		return objectHead{}, err
	}
	res := ssh.RunDetailed(ctx, c.exec, s3.AWS("s3api head-object --bucket "+ssh.ShellQuote(bucket)+" --key "+ssh.ShellQuote(k)))
	if res.Failed() {
		return objectHead{}, fmt.Errorf("reading %s: %s", key, oneLine(res.ExitErrorText()))
	}
	var h struct {
		ContentLength int64             `json:"ContentLength"`
		Metadata      map[string]string `json:"Metadata"`
	}
	if err := json.Unmarshal(res.Stdout, &h); err != nil {
		return objectHead{}, fmt.Errorf("reading %s: unexpected reply: %w", key, err)
	}
	return objectHead{Size: h.ContentLength, SHA256: h.Metadata["sha256"]}, nil
}

// splitS3URI splits s3://bucket/key.
func splitS3URI(uri string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3:// key: %q", uri)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("not an s3:// object key: %q", uri)
	}
	return bucket, key, nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// put stores local at key (key.age when encrypting) with the guarantees in
// this file's header. The plaintext is gone from disk afterwards only when
// encrypting; the caller's workspace owns the rest.
func (c *Client) put(ctx context.Context, s3 S3Config, local, key string) (storedObject, error) {
	var none storedObject
	src, final, encrypted := local, key, false
	if s3.Encrypted() {
		enc, err := c.encryptLocal(ctx, s3, local)
		if err != nil {
			return none, err
		}
		src, final, encrypted = enc, key+ageSuffix, true
	}
	size, sum, err := c.localFacts(ctx, src)
	if err != nil {
		return none, err
	}
	pending := incomingKey(final)
	q := ssh.ShellQuote
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s --metadata %s", q(src), q(pending), q("sha256="+sum)))); err != nil {
		return none, fmt.Errorf("uploading: %w", err)
	}
	removePending := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		c.exec.Run(rctx, s3.AWS("s3 rm "+q(pending)))
	}
	check := func(k string) error {
		h, err := c.head(ctx, s3, k)
		if err != nil {
			return err
		}
		if h.Size != size {
			return fmt.Errorf("stored %s is %d bytes, %d were sent", k, h.Size, size)
		}
		if h.SHA256 != "" && h.SHA256 != sum {
			return fmt.Errorf("stored %s records checksum %s, %s was sent", k, h.SHA256, sum)
		}
		return nil
	}
	if err := check(pending); err != nil {
		removePending()
		return none, fmt.Errorf("verifying the upload: %w", err)
	}
	if size <= readBackMax {
		// A pipe, but compared by value: anything short of the exact bytes
		// (a failed read included) produces a different hash.
		out, err := c.exec.Run(ctx, "{ "+s3.AWS("s3 cp "+q(pending)+" -")+"; } | sha256sum")
		if err != nil {
			removePending()
			return none, fmt.Errorf("reading the upload back: %w", err)
		}
		if f := strings.Fields(lastLine(out)); len(f) == 0 || f[0] != sum {
			removePending()
			return none, fmt.Errorf("the uploaded object's bytes do not match what was sent (checksum mismatch)")
		}
	}
	if _, err := c.exec.Run(ctx, s3.AWS("s3 cp "+q(pending)+" "+q(final))); err != nil {
		removePending()
		return none, fmt.Errorf("publishing the verified upload: %w", err)
	}
	if err := check(final); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		c.exec.Run(rctx, s3.AWS("s3 rm "+q(final)))
		removePending()
		return none, fmt.Errorf("verifying the published object: %w", err)
	}
	removePending()
	return storedObject{Key: final, Size: size, SHA256: sum, Encrypted: encrypted}, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// fetch downloads key to dest, checks it against the checksum recorded at
// upload (objects from older CLIs have none), and decrypts a .age object with
// the server's identity. dest never holds ciphertext.
func (c *Client) fetch(ctx context.Context, s3 S3Config, key, dest string) error {
	q := ssh.ShellQuote
	encrypted := strings.HasSuffix(key, ageSuffix)
	raw := dest
	if encrypted {
		raw = dest + ageSuffix
	}
	h, headErr := c.head(ctx, s3, key)
	if headErr == nil {
		// The download sits beside its plaintext until decrypted, on a disk a
		// failing restore may already have short of room.
		need := h.Size
		if encrypted {
			need *= 2
		}
		if free, known := c.freeBytes(ctx, "", path.Dir(dest)); known && free < need {
			return fmt.Errorf("not enough free space in %s for %s: %d bytes free, %d needed (nothing was changed)", path.Dir(dest), key, free, need)
		}
	}
	if _, err := c.exec.Run(ctx, s3.AWS("s3 cp "+q(key)+" "+q(raw))); err != nil {
		return fmt.Errorf("downloading %s: %w", key, err)
	}
	if headErr == nil && h.SHA256 != "" {
		if _, sum, err := c.localFacts(ctx, raw); err == nil && sum != h.SHA256 {
			c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(raw))
			return fmt.Errorf("%s is corrupt: its checksum does not match the one recorded when it was stored", key)
		}
	}
	if !encrypted {
		return nil
	}
	ids := `-i "` + ageIdentityShell + `"`
	if s3.ExtraIdentity != "" {
		ids += " -i " + q(s3.ExtraIdentity)
	}
	if _, err := c.exec.Run(ctx, "umask 077; age -d "+ids+" -o "+q(dest)+" "+q(raw)); err != nil {
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(raw)+" "+q(dest))
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return fmt.Errorf("decrypting %s with this server's identity failed (encrypted to other recipients, or corrupt; on another machine use `teploy backup fetch --identity`): %w", key, err)
	}
	c.exec.Run(ctx, "rm -f "+q(raw))
	return nil
}

// fetchSidecar is fetch for a sidecar that may be plaintext (older CLIs) or
// encrypted, and may not exist at all.
func (c *Client) fetchSidecar(ctx context.Context, s3 S3Config, key, dest string) bool {
	if c.fetch(ctx, s3, key, dest) == nil {
		return true
	}
	return c.fetch(ctx, s3, key+ageSuffix, dest) == nil
}

// ---- layout ----

// AdoptedApp is the key namespace of adopted containers (policy `adopted:`):
// containers with no teploy labels, addressed by their exact docker name. The
// underscore cannot occur in an app name, so it cannot collide with one. S3:
// s3://<base>/_adopted/accessories/<container>/...
const AdoptedApp = "_adopted"

// FleetApp is the pseudo-app whose event log holds `backup run --all` summaries.
const FleetApp = "_fleet"

// ContainerName is the docker name of an accessory: <app>-<name>, or the bare
// container name in the adopted namespace.
func ContainerName(app, name string) string {
	if app == AdoptedApp {
		return name
	}
	return app + "-" + name
}

// dbSegmentSafe are the bytes a database name keeps in an S3 key.
func dbSegmentSafe(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.'
}

// dbSegment encodes a database name as one S3 key segment. Schemas can hold any
// byte: "/" must not add a level, spaces would break `aws s3 ls` parsing, and
// ".." must not look like a path.
func dbSegment(db string) string {
	var b strings.Builder
	for i := 0; i < len(db); i++ {
		c := db[i]
		if dbSegmentSafe(c) && !(c == '.' && (i == 0 || strings.HasPrefix(db[i:], ".."))) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// decodeDBSegment reverses dbSegment.
func decodeDBSegment(seg string) string {
	if d, err := url.PathUnescape(seg); err == nil {
		return d
	}
	return seg
}

// addressableDB reports whether a database name can be handed to the engine
// tools as one literal shell-quoted argument. pg_dump and psql read a name with
// "=" as a connection string, and every tool reads a leading "-" as an option;
// control characters cannot survive line-based listings. Everything else,
// including quotes, spaces and $(...), is safe once shell-quoted. A name that
// fails this is reported as not backed up, never skipped silently.
func addressableDB(db string) bool {
	if db == "" || len(db) > 64 || db[0] == '-' || strings.Contains(db, "=") {
		return false
	}
	for _, r := range db {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// seriesPrefix is where one accessory's backups live: the primary series, or
// one database's series below it.
func seriesPrefix(s3 S3Config, app, name, db string) string {
	p := fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Base(), app, name)
	if db != "" {
		p += "databases/" + dbSegment(db) + "/"
	}
	return p
}

// ---- listing ----

// storedKey is one object in a recursive listing.
type storedKey struct {
	Key  string // relative to the bucket root
	Size int64
}

// listTree lists every object under s3://<base>/<rel> recursively. A prefix with
// no objects is an empty result: `aws s3 ls` exits 1 with no output for it,
// which every other failure mode does not.
func (c *Client) listTree(ctx context.Context, s3 S3Config, rel string) ([]storedKey, error) {
	root := "s3://" + s3.Base() + "/" + rel
	res := ssh.RunDetailedLimit(ctx, c.exec, s3.AWS("s3 ls "+ssh.ShellQuote(root)+" --recursive"), 64<<20)
	if res.Err != nil {
		return nil, fmt.Errorf("listing %s: %w", root, res.Err)
	}
	if res.Truncated {
		return nil, fmt.Errorf("listing %s: too many objects to read safely", root)
	}
	if res.ExitCode != 0 {
		if res.ExitCode == 1 && strings.TrimSpace(string(res.Stdout)) == "" && strings.TrimSpace(string(res.Stderr)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s: %s", root, oneLine(res.ExitErrorText()))
	}
	var out []storedKey
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, storedKey{Key: f[3], Size: size})
	}
	return out, nil
}

// locations lists where this config's backups may be: the current prefix, then
// any previous prefix of the same bucket.
func (s3 S3Config) locations() []S3Config {
	out := []S3Config{s3}
	for _, p := range s3.PreviousPrefixes {
		loc := s3
		loc.Prefix = p.Prefix
		loc.PreviousPrefixes = nil
		loc.isPrevious = true
		loc.previousOwned = p.Owned
		out = append(out, loc)
	}
	return out
}

// relToBase strips the configured prefix from a bucket-relative key. A key
// outside the prefix is not this config's.
func (s3 S3Config) relToBase(key string) (string, bool) {
	if s3.Prefix == "" {
		return key, true
	}
	return strings.CutPrefix(key, s3.Prefix+"/")
}

// findObject finds backup id in a series across the current and previous
// locations, trying each extension and its encrypted form. It returns the
// location it was found in (sidecars live beside it).
func (c *Client) findObject(ctx context.Context, s3 S3Config, rel, id string, exts ...string) (key, ext string, loc S3Config, err error) {
	var listErr error
	for _, l := range s3.locations() {
		prefix := "s3://" + l.Base() + "/" + rel
		out, lsErr := c.exec.Run(ctx, l.AWS("s3 ls "+ssh.ShellQuote(prefix+id+".")))
		if lsErr != nil {
			listErr = lsErr
			continue
		}
		names := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				names[f[len(f)-1]] = true
			}
		}
		for _, e := range exts {
			if names[id+e] {
				return prefix + id + e, e, l, nil
			}
			if names[id+e+ageSuffix] {
				return prefix + id + e + ageSuffix, e, l, nil
			}
		}
	}
	detail := ""
	if listErr != nil {
		detail = fmt.Sprintf(" (listing: %v)", listErr)
	}
	return "", "", s3, fmt.Errorf("no backup %s found under s3://%s/%s (looked for %s)%s", id, s3.Base(), rel, strings.Join(exts, ", "), detail)
}

// artifactFloor is the smallest object of this name that can be a real backup.
// Anything below it is an empty-output failure (U11: fourteen 20-byte files)
// and never counts as a good copy.
func artifactFloor(name string) int64 {
	base := strings.TrimSuffix(name, ageSuffix)
	floor := int64(0)
	switch {
	case strings.HasSuffix(base, ".pgdump"):
		floor = minPostgresDump
	case strings.HasSuffix(base, ".sql.gz"):
		floor = minMySQLDump
	case strings.HasSuffix(base, ".archive.gz"):
		floor = minMongoDump
	case strings.HasSuffix(base, ".rdb.gz"):
		floor = minRDBSnapshot
	case strings.HasSuffix(base, ".tar.gz"):
		floor = minTarArchive
	}
	if base != name {
		floor += ageFloorOverhead
	}
	return floor
}

// ---- janitor ----

// tmpLeftoverAge is how old an unattended workspace or .incoming object must be
// before it is treated as a killed run's leftover: longer than the deadline of
// any one target, shorter than a disk can afford to wait.
const tmpLeftoverMinutes = 420

// workspaceGlobs are what the run cleans up: private run workspaces a killed
// backup can leave (dumps, possibly a credential file), and a recovery key a
// killed `dr restore --identity` left on this server. Only ever this: a killed
// restore's own files can be the only copy of something.
const workspaceGlobs = "/var/tmp/teploy-backup.* /tmp/teploy-backup.* /var/tmp/teploy-dr-identity.*"

// leftoverGlobs are what doctor and status report: the above, plus a kept or
// killed restore's and verify's scratch files.
const leftoverGlobs = workspaceGlobs + " /var/tmp/teploy-restore.* /tmp/teploy-restore.* /tmp/teploy-volume-restore.* /var/tmp/teploy-verify.*"

// CleanStaleWorkspaces removes run workspaces older than the leftover age and
// returns what it removed. Only called while holding the run lock, so a live
// `backup run --all` cannot be hit; the age protects a concurrent manual one.
func (c *Client) CleanStaleWorkspaces(ctx context.Context) []string {
	out, err := c.exec.Run(ctx, fmt.Sprintf(`for d in %s; do [ -d "$d" ] || continue; if [ -n "$(find "$d" -maxdepth 0 -mmin +%d 2>/dev/null)" ]; then rm -rf -- "$d" && printf '%%s\n' "$d"; fi; done; true`, workspaceGlobs, tmpLeftoverMinutes))
	if err != nil {
		return nil
	}
	return splitLines(out)
}

// LeftoverWorkspacesProbe lists workspaces past the leftover age, for doctor.
func LeftoverWorkspacesProbe() string {
	return fmt.Sprintf(`for d in %s; do [ -d "$d" ] || continue; if [ -n "$(find "$d" -maxdepth 0 -mmin +%d 2>/dev/null)" ]; then printf '%%s\n' "$d"; fi; done; docker ps -a --filter name=teploy-backup-verify- --format 'container {{.Names}}' 2>/dev/null; true`, leftoverGlobs, tmpLeftoverMinutes)
}

// AbortStaleUploads aborts multipart uploads a killed run left under the base
// (they are invisible in listings and bill storage). Best effort; returns how
// many were aborted.
func (c *Client) AbortStaleUploads(ctx context.Context, s3 S3Config, now time.Time) int {
	q := ssh.ShellQuote
	args := "s3api list-multipart-uploads --bucket " + q(s3.Bucket) + " --output json"
	if s3.Prefix != "" {
		args += " --prefix " + q(s3.Prefix+"/")
	}
	res := ssh.RunDetailed(ctx, c.exec, s3.AWS(args))
	if res.Failed() {
		return 0
	}
	var list struct {
		Uploads []struct {
			Key       string `json:"Key"`
			UploadID  string `json:"UploadId"`
			Initiated string `json:"Initiated"`
		} `json:"Uploads"`
	}
	if json.Unmarshal(res.Stdout, &list) != nil {
		return 0
	}
	sort.Slice(list.Uploads, func(i, j int) bool { return list.Uploads[i].Key < list.Uploads[j].Key })
	aborted := 0
	for _, u := range list.Uploads {
		t, err := time.Parse(time.RFC3339, u.Initiated)
		if err != nil || now.Sub(t) < tmpLeftoverMinutes*time.Minute {
			continue
		}
		if _, err := c.exec.Run(ctx, s3.AWS("s3api abort-multipart-upload --bucket "+q(s3.Bucket)+" --key "+q(u.Key)+" --upload-id "+q(u.UploadID))); err == nil {
			aborted++
		}
	}
	return aborted
}

// ---- namespace ownership ----

// ownersDir holds one marker per server that has written into an app namespace:
// <base>/<app>/.owners/<fingerprint>. Prune refuses a namespace with another
// server's marker, so a shared bucket or a reused prefix can never make one
// server delete another's backups.
const ownersDir = ".owners"

var ownerMarkerRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// fingerprint identifies this server: a hash of its machine id, else its age
// recipient. Empty when neither can be read.
func (c *Client) fingerprint(ctx context.Context) string {
	if c.serverFP != "" {
		return c.serverFP
	}
	out, err := c.exec.Run(ctx, `{ cat /etc/machine-id 2>/dev/null || hostname; } | sha256sum`)
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	if len(f) == 0 || len(f[0]) < 16 {
		return ""
	}
	if !ownerMarkerRE.MatchString(f[0][:16]) {
		return ""
	}
	c.serverFP = f[0][:16]
	return c.serverFP
}

func namespaceOwnersPrefix(s3 S3Config, app string) string {
	return fmt.Sprintf("s3://%s/%s/%s/", s3.Base(), app, ownersDir)
}

// namespaceOwners lists the servers that have marked this namespace.
func (c *Client) namespaceOwners(ctx context.Context, s3 S3Config, app string) []string {
	out, err := c.exec.Run(ctx, s3.AWS("s3 ls "+ssh.ShellQuote(namespaceOwnersPrefix(s3, app))))
	if err != nil {
		return nil
	}
	var owners []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && ownerMarkerRE.MatchString(f[len(f)-1]) {
			owners = append(owners, f[len(f)-1])
		}
	}
	return owners
}

// markNamespaceOwner records this server as a writer of the namespace. Best
// effort; called after a successful backup.
func (c *Client) markNamespaceOwner(ctx context.Context, s3 S3Config, app string) {
	fp := c.fingerprint(ctx)
	if fp == "" {
		return
	}
	for _, o := range c.namespaceOwners(ctx, s3, app) {
		if o == fp {
			return
		}
	}
	key := namespaceOwnersPrefix(s3, app) + fp
	c.exec.Run(ctx, "printf '%s\\n' "+ssh.ShellQuote(time.Now().UTC().Format(time.RFC3339))+" | "+s3.AWS("s3 cp - "+ssh.ShellQuote(key)))
}

// ErrSharedNamespace marks a prune refused because another server writes there.
var ErrSharedNamespace = errors.New("shared backup namespace")

// checkNamespaceOwner refuses when another server has marked this namespace.
func (c *Client) checkNamespaceOwner(ctx context.Context, s3 S3Config, app string) error {
	owners := c.namespaceOwners(ctx, s3, app)
	if len(owners) == 0 {
		return nil
	}
	fp := c.fingerprint(ctx)
	var others []string
	for _, o := range owners {
		if o != fp {
			others = append(others, o)
		}
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("%w: another server (%s) also stores %s backups under s3://%s, so pruning here could delete its copies; nothing was pruned. Give each server its own prefix (`teploy server backup-target set --prefix <server>`)", ErrSharedNamespace, strings.Join(others, ", "), app, s3.Base())
}
