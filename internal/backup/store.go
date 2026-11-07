package backup

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
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
//   - A final key means a verified object: the upload goes under one
//     <base>/.incoming/ tree, its size and checksum are checked there, and only
//     then is it copied server-side to its final name. A killed run therefore
//     can never leave a partial or unverified object where list, latest,
//     restore or prune would take it for a backup. One tree because a store that
//     keeps versions (B2 only hides on delete) bills every byte ever written
//     there until a lifecycle rule expires it, and a rule can only name a prefix:
//     `.incoming/` with a 1-day expiry is what doctor looks for.
//
//   - A store that refuses the server-side copy (B2 cannot change a checksum
//     algorithm on one, R2 has no GetObjectTagging) still gets the object: the
//     verified local file is uploaded straight to the final name and verified
//     there. Atomic like any upload; a kill between that upload completing and
//     its check is the one window a final key is unverified.
//
//   - Commit order: a backup's sidecars and extra databases are stored BEFORE
//     its primary object, so a primary implies everything that belongs to it.

// ageSuffix marks an object encrypted with age.
const ageSuffix = ".age"

// incomingDir holds uploads until they are verified.
const incomingDir = ".incoming"

// readBackMax is the largest object whose bytes are read back and hashed after
// upload. Above it the object is uploaded with a provider-verified checksum
// (--checksum-algorithm SHA256: the store checks every part on receipt) and that
// is read back instead: reading a large backup back costs bandwidth, and our own
// metadata proves only what we sent.
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
		return "", fmt.Errorf("the age tool is not installed on this server, so nothing can be encrypted (run `teploy server backup-preflight --host <server> --install-tools` or `teploy setup`, or apt-get install age)")
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
	cmd := fmt.Sprintf("age -r %s -r %s -o %s %s", q(s3.OwnerRecipient), q(server), q(enc), q(local))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(enc))
		return "", fmt.Errorf("encrypting the backup (nothing was uploaded): %w", err)
	}
	// cmp reads the decrypted stream to its end and compares it with the
	// plaintext: a decrypt failure shows up as a short stream, so losing age's
	// exit status in this pipe cannot hide anything.
	check := fmt.Sprintf(`age -d %s %s | cmp -s - %s`, ageIdentityArgs, q(enc), q(local))
	if _, err := c.exec.Run(ctx, check); err != nil {
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(enc))
		return "", fmt.Errorf("the encrypted backup did not decrypt back to its plaintext with the server's identity (nothing was uploaded): %w", err)
	}
	if _, err := c.exec.Run(ctx, "rm -f "+q(local)); err != nil {
		return "", fmt.Errorf("removing the plaintext after encrypting: %w", err)
	}
	return enc, nil
}

// ageIdentityArgs expands to "-i <path>" for the server's identity, same
// try-order as ensureAgeIdentity, and to nothing on a host that has none (or only
// the public-key marker left behind when the key moved). age aborts on a missing
// -i file, so a clean host recovering with --identity must not pass one.
const ageIdentityArgs = `$(for p in /etc/teploy/age-key /deployments/.age-key; do if [ -r "$p" ] && grep -q '^AGE-SECRET-KEY-' "$p" 2>/dev/null; then printf -- '-i %s' "$p"; break; fi; done)`

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

// incomingKey is the verification-pending name of a final key: the same path
// under the base's one .incoming/ tree.
func incomingKey(s3 S3Config, key string) string {
	base := "s3://" + s3.Base() + "/"
	if rel, ok := strings.CutPrefix(key, base); ok {
		return base + incomingDir + "/" + rel
	}
	// Not under the base (no caller builds such a key): under the bucket's own
	// tree rather than anywhere a listing would take it for a backup.
	bucket, rel, err := splitS3URI(key)
	if err != nil {
		return key + "." + incomingDir
	}
	return "s3://" + bucket + "/" + incomingDir + "/" + rel
}

// objectHead is the stored object's size and recorded checksum.
type objectHead struct {
	Size   int64
	SHA256 string
	// ProviderSHA256 is the store's own checksum (base64), when it kept one and
	// was asked: "xxx-N" for a multipart object, whose parts it checked on receipt.
	ProviderSHA256 string
	// LastModified is the store's clock at the object's last write.
	LastModified time.Time
}

// head reads an object's size and recorded checksum.
func (c *Client) head(ctx context.Context, s3 S3Config, key string) (objectHead, error) {
	return c.headArgs(ctx, s3, key, "")
}

func (c *Client) headArgs(ctx context.Context, s3 S3Config, key, extra string) (objectHead, error) {
	bucket, k, err := splitS3URI(key)
	if err != nil {
		return objectHead{}, err
	}
	res := ssh.RunDetailed(ctx, c.exec, s3.AWS("s3api head-object --bucket "+ssh.ShellQuote(bucket)+" --key "+ssh.ShellQuote(k)+extra))
	if res.Failed() {
		return objectHead{}, fmt.Errorf("reading %s: %s", key, oneLine(res.ExitErrorText()))
	}
	var h struct {
		ContentLength  int64             `json:"ContentLength"`
		Metadata       map[string]string `json:"Metadata"`
		ChecksumSHA256 string            `json:"ChecksumSHA256"`
		LastModified   string            `json:"LastModified"`
	}
	if err := json.Unmarshal(res.Stdout, &h); err != nil {
		return objectHead{}, fmt.Errorf("reading %s: unexpected reply: %w", key, err)
	}
	out := objectHead{Size: h.ContentLength, SHA256: h.Metadata["sha256"], ProviderSHA256: h.ChecksumSHA256}
	// aws CLI v2 prints RFC 3339, v1 an HTTP date.
	for _, layout := range []string{time.RFC3339, time.RFC1123, "Mon, 02 Jan 2006 15:04:05 GMT"} {
		if t, err := time.Parse(layout, h.LastModified); err == nil {
			out.LastModified = t.UTC()
			break
		}
	}
	return out, nil
}

func (c *Client) readBackLimit() int64 {
	if c.readBack > 0 {
		return c.readBack
	}
	return readBackMax
}

// sha256HexToBase64 converts a sha256sum digest to the form a provider reports.
func sha256HexToBase64(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// checksumUnsupported recognises a CLI or provider that cannot take
// --checksum-algorithm: an old aws CLI, a store without additional checksums.
func checksumUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"unknown options", "notimplemented", "not implemented", "invalidargument", "invalidrequest", "unsupported", "x-amz-checksum", "x-amz-sdk-checksum"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// copyRefused recognises a store that answered a server-side copy with a refusal:
// the checksum path above, or any error reply to the copy. A connection that
// failed says none of this, and a re-upload would fail the same way.
func copyRefused(err error) bool {
	return checksumUnsupported(err) || strings.Contains(strings.ToLower(err.Error()), "an error occurred (")
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
	pending := incomingKey(s3, final)
	q := ssh.ShellQuote
	large := size > c.readBackLimit()
	// upload reports whether the store checks a checksum on receipt.
	upload := func(dst string) (bool, error) {
		cmd := fmt.Sprintf("s3 cp %s %s --metadata %s", q(src), q(dst), q("sha256="+sum))
		if !large {
			_, err := c.exec.Run(ctx, s3.AWS(cmd))
			return false, err
		}
		_, err := c.exec.Run(ctx, s3.AWS(cmd+" --checksum-algorithm SHA256"))
		if err == nil || !checksumUnsupported(err) {
			return err == nil, err
		}
		fmt.Fprintf(c.out, "Warning: this store or aws CLI has no upload checksums, so %s is verified by size alone: %v\n", path.Base(final), oneLine(err.Error()))
		_, err = c.exec.Run(ctx, s3.AWS(cmd))
		return false, err
	}
	check := func(k string) (objectHead, error) {
		h, err := c.head(ctx, s3, k)
		if err != nil {
			return h, err
		}
		if h.Size != size {
			return h, fmt.Errorf("stored %s is %d bytes, %d were sent", k, h.Size, size)
		}
		if h.SHA256 != "" && h.SHA256 != sum {
			return h, fmt.Errorf("stored %s records checksum %s, %s was sent", k, h.SHA256, sum)
		}
		return h, nil
	}
	// verify proves an upload: size and recorded checksum, then the bytes read back
	// or the store's own checksum.
	verify := func(k string, providerChecked bool) (objectHead, error) {
		h, err := check(k)
		if err != nil {
			return h, err
		}
		if providerChecked {
			// The store checked every part on receipt; a single-part object's digest
			// can be compared whole, and a store that kept none has proved nothing.
			ph, err := c.headArgs(ctx, s3, k, " --checksum-mode ENABLED")
			switch {
			case err != nil && checksumUnsupported(err):
				fmt.Fprintf(c.out, "Warning: this store or aws CLI cannot report checksums, so %s is verified by size alone: %v\n", path.Base(final), oneLine(err.Error()))
			case err != nil:
				return h, err
			case ph.ProviderSHA256 == "":
				fmt.Fprintf(c.out, "Warning: the store keeps no checksum for %s; it is verified by size alone\n", path.Base(final))
			case !strings.Contains(ph.ProviderSHA256, "-") && ph.ProviderSHA256 != sha256HexToBase64(sum):
				return h, fmt.Errorf("the store's checksum of the upload does not match what was sent (checksum mismatch)")
			}
		} else if !large {
			// A pipe, but compared by value: anything short of the exact bytes
			// (a failed read included) produces a different hash.
			out, err := c.exec.Run(ctx, "{ "+s3.AWS("s3 cp "+q(k)+" -")+"; } | sha256sum")
			if err != nil {
				return h, fmt.Errorf("reading the upload back: %w", err)
			}
			if f := strings.Fields(lastLine(out)); len(f) == 0 || f[0] != sum {
				return h, fmt.Errorf("the uploaded object's bytes do not match what was sent (checksum mismatch)")
			}
		}
		return h, nil
	}
	remove := func(k string) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		c.exec.Run(rctx, s3.AWS("s3 rm "+q(k)))
	}
	providerChecked, err := upload(pending)
	if err != nil {
		return none, fmt.Errorf("uploading: %w", err)
	}
	pendingHead, err := verify(pending, providerChecked)
	if err != nil {
		remove(pending)
		return none, fmt.Errorf("verifying the upload: %w", err)
	}
	if err := c.publish(ctx, s3, pending, final, pendingHead.SHA256, check, func() error {
		fmt.Fprintf(c.out, "A server-side copy of %s did not work on this store, so it is uploaded straight to its final name\n", path.Base(final))
		checked, err := upload(final)
		if err != nil {
			remove(final)
			return fmt.Errorf("uploading: %w", err)
		}
		if _, err := verify(final, checked); err != nil {
			remove(final)
			return fmt.Errorf("verifying the published object: %w", err)
		}
		return nil
	}, remove); err != nil {
		remove(pending)
		return none, err
	}
	remove(pending)
	return storedObject{Key: final, Size: size, SHA256: sum, Encrypted: encrypted}, nil
}

// publish copies a verified upload to its final name. The copy names
// --copy-props metadata-directive because a copy of 8 MB or more is multipart and
// its default also reads the source's tags, which R2 does not implement; metadata
// is what fetch checks the object against. aws CLI v1 has no such flag, and its
// multipart copy drops metadata: a final object that lost the checksum the pending
// one had is replaced too. reupload is the way out whenever the store refuses.
func (c *Client) publish(ctx context.Context, s3 S3Config, pending, final, recorded string, check func(string) (objectHead, error), reupload func() error, remove func(string)) error {
	q := ssh.ShellQuote
	copyCmd := "s3 cp " + q(pending) + " " + q(final)
	_, err := c.exec.Run(ctx, s3.AWS(copyCmd+" --copy-props metadata-directive"))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown options") {
		_, err = c.exec.Run(ctx, s3.AWS(copyCmd))
	}
	if err != nil {
		if ctx.Err() != nil || !copyRefused(err) {
			return fmt.Errorf("publishing the verified upload: %w", err)
		}
		fmt.Fprintf(c.out, "Warning: %s\n", oneLine(err.Error()))
		return reupload()
	}
	h, err := check(final)
	if err != nil {
		remove(final)
		return fmt.Errorf("verifying the published object: %w", err)
	}
	if recorded != "" && h.SHA256 == "" {
		return reupload()
	}
	return nil
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
	ids := ageIdentityArgs
	if s3.ExtraIdentity != "" {
		ids += " -i " + q(s3.ExtraIdentity)
	}
	if _, err := c.exec.Run(ctx, "umask 077; age -d "+ids+" -o "+q(dest)+" "+q(raw)); err != nil {
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+q(raw)+" "+q(dest))
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return fmt.Errorf("decrypting %s failed: it is corrupt, or encrypted to other recipients than this server's identity (pass --identity <recovery key file>, or `teploy backup fetch --identity` on another machine): %w", key, err)
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
// "=" or a URI scheme as a connection string, and every tool reads a leading "-"
// as an option;
// control characters cannot survive line-based listings. Everything else,
// including quotes, spaces and $(...), is safe once shell-quoted. A name that
// fails this is reported as not backed up, never skipped silently.
func addressableDB(db string) bool {
	if db == "" || len(db) > 64 || db[0] == '-' || strings.Contains(db, "=") || strings.Contains(db, "://") {
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
	// Modified is the store's date of the last write, in this machine's zone as
	// the aws CLI prints it: good for "this week", not for arithmetic in minutes.
	Modified time.Time
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
		reason := oneLine(res.ExitErrorText())
		if reason == "" {
			reason = fmt.Sprintf("aws exited with status %d and gave no reason", res.ExitCode)
		}
		return nil, fmt.Errorf("listing %s: %s", root, reason)
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
		k := storedKey{Key: f[3], Size: size}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", f[0]+" "+f[1], time.Local); err == nil {
			k.Modified = t
		}
		out = append(out, k)
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

// MaxDeadline is the longest deadline any one backup, restore or verify may be
// given: the whole fleet run's. Anything past it is refused, not silently capped.
const MaxDeadline = 23 * time.Hour

// ValidateDeadline checks a deadline an operator named (0 is the default).
func ValidateDeadline(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("deadline must be positive")
	}
	if d > MaxDeadline {
		return fmt.Errorf("deadline %s is over the %s a run may last", d, MaxDeadline)
	}
	return nil
}

// tmpLeftoverMinutes is how old a temp directory with no expiry marker must be
// before it is treated as a killed run's leftover. A backup workspace carries its
// own (workspaceExpiry), so a run with a long --deadline is not cleaned from under
// itself by a concurrent run's janitor, whatever that run's deadline is.
const tmpLeftoverMinutes = 420

// incomingStaleAfter is how old a pending upload must be before it is a leftover:
// past the longest deadline any run may have, and what a store's 1-day lifecycle
// rule on .incoming/ would do anyway.
const incomingStaleAfter = MaxDeadline + time.Hour

// workspaceExpiry names the file in a backup workspace saying, in epoch seconds,
// when the run that made it must be over.
const workspaceExpiry = ".expires"

// leftoverTest is shell: true when $d is a leftover.
func leftoverTest() string {
	return fmt.Sprintf(`if [ -f "$d/%s" ]; then [ "$(date +%%s)" -gt "$(cat "$d/%s" 2>/dev/null || echo 0)" ]; else [ -n "$(find "$d" -maxdepth 0 -mmin +%d 2>/dev/null)" ]; fi`, workspaceExpiry, workspaceExpiry, tmpLeftoverMinutes)
}

// tmpBases are the directories backup workspaces can be under: disk, /tmp from
// older runs, and a configured TEPLOY_BACKUP_TMPDIR or policy tmp_dir, which is
// where a killed run's plaintext dump would otherwise sit forever unnoticed.
func tmpBases(extra string) []string {
	bases := []string{"/var/tmp", "/tmp"}
	for _, d := range []string{backupTmpDir(), extra} {
		d = strings.TrimRight(strings.TrimSpace(d), "/")
		if d != "" && !slices.Contains(bases, d) {
			bases = append(bases, d)
		}
	}
	return bases
}

func tmpGlobs(extra string, names ...string) string {
	var out []string
	for _, base := range tmpBases(extra) {
		for _, n := range names {
			out = append(out, ssh.ShellQuote(base)+"/"+n)
		}
	}
	return strings.Join(out, " ")
}

// CleanStaleWorkspaces removes run workspaces (dumps, possibly a credential file)
// and any recovery key a killed `restore --identity` left, once they are older
// than the leftover age or past their own expiry, and returns what it removed. Only
// called while holding the run lock, so a live `backup run --all` cannot be hit; the
// expiry protects a concurrent manual one. Never a killed restore's own files: those
// can be the only copy of something.
func (c *Client) CleanStaleWorkspaces(ctx context.Context) []string {
	out, err := c.exec.Run(ctx, cleanWorkspacesScript(tmpGlobs("", "teploy-backup.*", "teploy-dr-identity.*")))
	if err != nil {
		return nil
	}
	return splitLines(out)
}

func cleanWorkspacesScript(globs string) string {
	return fmt.Sprintf(`for d in %s; do [ -d "$d" ] || continue; if %s; then rm -rf -- "$d" && printf '%%s\n' "$d"; fi; done; true`, globs, leftoverTest())
}

// LeftoverWorkspacesProbe lists what doctor and status report: the above plus a
// kept or killed restore's and verify's scratch files and containers.
func LeftoverWorkspacesProbe(tmpDir string) string {
	globs := tmpGlobs(tmpDir, "teploy-backup.*", "teploy-dr-identity.*", "teploy-restore.*", "teploy-volume-restore.*", "teploy-verify.*")
	return fmt.Sprintf(`for d in %s; do [ -d "$d" ] || continue; if %s; then printf '%%s\n' "$d"; fi; done; docker ps -a --filter name=teploy-backup-verify- --format 'container {{.Names}}' 2>/dev/null; true`, globs, leftoverTest())
}

// AbortStaleUploads aborts multipart uploads a killed run left under the given
// namespaces, in the .incoming/ tree and at final names (invisible in listings, and
// they bill storage). Scoped to namespaces only this server writes to: with no
// prefix a bucket-wide list would catch other servers' uploads. Best effort; returns
// how many were aborted.
func (c *Client) AbortStaleUploads(ctx context.Context, s3 S3Config, namespaces []string, now time.Time) int {
	q := ssh.ShellQuote
	aborted := 0
	for _, ns := range namespaces {
		owners, err := c.namespaceOwners(ctx, s3, ns)
		fp := c.fingerprint(ctx)
		if err != nil || fp == "" || slices.ContainsFunc(owners, func(o string) bool { return o != fp }) {
			continue
		}
		base := ""
		if s3.Prefix != "" {
			base = s3.Prefix + "/"
		}
		for _, prefix := range []string{base + ns + "/", base + incomingDir + "/" + ns + "/"} {
			res := ssh.RunDetailed(ctx, c.exec, s3.AWS("s3api list-multipart-uploads --bucket "+q(s3.Bucket)+" --prefix "+q(prefix)+" --output json"))
			if res.Failed() {
				continue
			}
			var list struct {
				Uploads []struct {
					Key       string `json:"Key"`
					UploadID  string `json:"UploadId"`
					Initiated string `json:"Initiated"`
				} `json:"Uploads"`
			}
			if json.Unmarshal(res.Stdout, &list) != nil {
				continue
			}
			for _, u := range list.Uploads {
				t, err := time.Parse(time.RFC3339, u.Initiated)
				if err != nil || now.Sub(t) < incomingStaleAfter {
					continue
				}
				if _, err := c.exec.Run(ctx, s3.AWS("s3api abort-multipart-upload --bucket "+q(s3.Bucket)+" --key "+q(u.Key)+" --upload-id "+q(u.UploadID))); err == nil {
					aborted++
				}
			}
		}
	}
	return aborted
}

// ---- namespace ownership ----

// ownersDir holds one marker per server that has written into an app namespace:
// <base>/<app>/.owners/<fingerprint>, a JSON body naming the server's age public
// key. Prune refuses a namespace with another server's marker, so a shared bucket
// or a reused prefix can never make one server delete another's backups. Anything
// short of proof of sole ownership refuses: a listing that fails is not "no owners".
//
// The fingerprint is a hash of /etc/machine-id and the hostname (an empty
// machine-id, or a clone's copy of one, is not enough by itself), and the body's
// recipient tells apart two hosts that share both. It is in the body and not the
// name so that an identity created later does not make a server its own stranger.
//
// Two limits, documented rather than papered over with an id file a clone would copy
// too. A renamed host gets a new fingerprint, so it sees its old marker as another
// server's and refuses to prune: safe, and `owners --clear` ends it. A clone that
// keeps the machine-id and hostname and has no age identity is the same server to
// this check: clones need their own prefix (Dash always sets one) or a fresh
// machine-id.
//
// Residual: an upgraded server's first prune cannot tell its own unmarked legacy
// objects from those of a server that has not upgraded yet (AUDIT_OPEN).
const ownersDir = ".owners"

var ownerMarkerRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ownerMarker is a marker's body.
type ownerMarker struct {
	Recipient string `json:"recipient,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
}

// fingerprint identifies this server. Empty when it cannot be worked out.
func (c *Client) fingerprint(ctx context.Context) string {
	if c.serverFP != "" {
		return c.serverFP
	}
	out, err := c.exec.Run(ctx, `{ cat /etc/machine-id 2>/dev/null; hostname 2>/dev/null; } | sha256sum`)
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	if len(f) == 0 || len(f[0]) < 16 || !ownerMarkerRE.MatchString(f[0][:16]) {
		return ""
	}
	// sha256 of empty input: neither source answered, and every such host agrees.
	if strings.HasPrefix(f[0], "e3b0c44298fc1c14") {
		return ""
	}
	c.serverFP = f[0][:16]
	return c.serverFP
}

// localRecipient is this server's age public key, "" when it has no identity.
func (c *Client) localRecipient(ctx context.Context) string {
	out, err := c.exec.Run(ctx, `for p in /etc/teploy/age-key /deployments/.age-key; do if [ -r "$p" ] && grep -q '^AGE-SECRET-KEY-' "$p" 2>/dev/null; then age-keygen -y "$p" 2>/dev/null; break; fi; done; true`)
	if err != nil {
		return ""
	}
	if r := strings.TrimSpace(out); recipientRE.MatchString(r) {
		return r
	}
	return ""
}

func namespaceOwnersPrefix(s3 S3Config, app string) string {
	return fmt.Sprintf("s3://%s/%s/%s/", s3.Base(), app, ownersDir)
}

// namespaceOwners lists the servers that have marked this namespace. An empty
// namespace is no owners; a listing that fails is an error, never "no owners".
func (c *Client) namespaceOwners(ctx context.Context, s3 S3Config, app string) ([]string, error) {
	keys, err := c.listTree(ctx, s3, app+"/"+ownersDir+"/")
	if err != nil {
		return nil, err
	}
	var owners []string
	for _, k := range keys {
		name := path.Base(k.Key)
		if ownerMarkerRE.MatchString(name) {
			owners = append(owners, name)
		}
	}
	return owners, nil
}

func (c *Client) readOwnerMarker(ctx context.Context, s3 S3Config, key string) ownerMarker {
	var m ownerMarker
	if res := ssh.RunDetailed(ctx, c.exec, s3.AWS("s3 cp "+ssh.ShellQuote(key)+" -")); !res.Failed() {
		_ = json.Unmarshal(res.Stdout, &m)
	}
	return m
}

func (c *Client) writeOwnerMarker(ctx context.Context, s3 S3Config, key string, m ownerMarker) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = c.exec.Run(ctx, "printf '%s\\n' "+ssh.ShellQuote(string(body))+" | "+s3.AWS("s3 cp - "+ssh.ShellQuote(key)))
	return err
}

// markNamespaceOwner records this server as a writer of the namespace. Best
// effort, after a successful backup; a prune claims the namespace for itself.
func (c *Client) markNamespaceOwner(ctx context.Context, s3 S3Config, app string) {
	fp := c.fingerprint(ctx)
	if fp == "" {
		return
	}
	owners, err := c.namespaceOwners(ctx, s3, app)
	if err != nil || slices.Contains(owners, fp) {
		return
	}
	_ = c.writeOwnerMarker(ctx, s3, namespaceOwnersPrefix(s3, app)+fp, ownerMarker{Recipient: c.localRecipient(ctx), FirstSeen: time.Now().UTC().Format(time.RFC3339)})
}

// ErrSharedNamespace marks a prune refused because this server cannot show it is
// the only one writing there.
var ErrSharedNamespace = errors.New("shared backup namespace")

// claimNamespace proves this server may prune the namespace and marks it (a prune
// never runs on an unmarked namespace). The marker is written once, plus once more
// to record an identity created later: on a store that keeps versions every rewrite
// is another stored copy.
func (c *Client) claimNamespace(ctx context.Context, s3 S3Config, app string) error {
	base := "s3://" + s3.Base()
	owners, err := c.namespaceOwners(ctx, s3, app)
	if err != nil {
		return fmt.Errorf("%w: could not list the owners of %s/%s, so nothing was pruned: %v", ErrSharedNamespace, base, app, err)
	}
	fp := c.fingerprint(ctx)
	if fp == "" {
		return fmt.Errorf("%w: this server cannot identify itself (no machine-id or hostname), so nothing was pruned", ErrSharedNamespace)
	}
	var others []string
	for _, o := range owners {
		if o != fp {
			others = append(others, o)
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("%w: another server (%s) also stores %s backups under %s, so pruning here could delete its copies; nothing was pruned. Give each server its own prefix (`teploy server backup-target set --prefix <server>`). If that marker is this server before a rebuild: `teploy server backup-target owners --clear %s`", ErrSharedNamespace, strings.Join(others, ", "), app, base, strings.Join(others, ","))
	}
	key := namespaceOwnersPrefix(s3, app) + fp
	self := c.localRecipient(ctx)
	m := ownerMarker{Recipient: self, FirstSeen: time.Now().UTC().Format(time.RFC3339)}
	if slices.Contains(owners, fp) {
		old := c.readOwnerMarker(ctx, s3, key)
		if old.Recipient != "" && self != "" && old.Recipient != self {
			return fmt.Errorf("%w: the marker for this machine under %s/%s names another age identity (%s), so this is a clone or a rebuilt server; nothing was pruned. If it is this server's own old marker: `teploy server backup-target owners --clear %s`", ErrSharedNamespace, base, app, old.Recipient, fp)
		}
		if old.Recipient != "" || self == "" {
			return nil
		}
		if old.FirstSeen != "" {
			m.FirstSeen = old.FirstSeen
		}
	}
	if err := c.writeOwnerMarker(ctx, s3, key, m); err != nil {
		return fmt.Errorf("%w: could not mark %s/%s as this server's before pruning, so nothing was pruned: %v", ErrSharedNamespace, base, app, err)
	}
	return nil
}

// storeClock reads the store's clock: a tiny object written under .incoming/ (which
// a lifecycle rule expires, so it costs nothing on a store that keeps versions),
// dated by the store, and removed. It is the one reference a wrong server clock
// cannot fool.
func (c *Client) storeClock(ctx context.Context, s3 S3Config) (time.Time, error) {
	key := "s3://" + s3.Base() + "/" + incomingDir + "/.clock-" + c.fingerprint(ctx)
	q := ssh.ShellQuote
	if _, err := c.exec.Run(ctx, "printf '%s\\n' clock | "+s3.AWS("s3 cp - "+q(key))); err != nil {
		return time.Time{}, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		c.exec.Run(rctx, s3.AWS("s3 rm "+q(key)))
	}()
	h, err := c.head(ctx, s3, key)
	if err != nil {
		return time.Time{}, err
	}
	if h.LastModified.IsZero() {
		return time.Time{}, errors.New("the store's reply carries no date")
	}
	return h.LastModified, nil
}

// NamespaceMarker is one server's marker in one namespace.
type NamespaceMarker struct {
	Namespace   string `json:"namespace"`
	Fingerprint string `json:"fingerprint"`
	Key         string `json:"key"`
	Self        bool   `json:"self"`
}

// ListNamespaceMarkers lists every owner marker under the base.
func (c *Client) ListNamespaceMarkers(ctx context.Context, s3 S3Config) ([]NamespaceMarker, error) {
	keys, err := c.listTree(ctx, s3, "")
	if err != nil {
		return nil, err
	}
	fp := c.fingerprint(ctx)
	var out []NamespaceMarker
	for _, k := range keys {
		rel, ok := s3.relToBase(k.Key)
		if !ok {
			continue
		}
		segs := strings.Split(rel, "/")
		if len(segs) == 3 && segs[1] == ownersDir && ownerMarkerRE.MatchString(segs[2]) {
			out = append(out, NamespaceMarker{Namespace: segs[0], Fingerprint: segs[2], Key: "s3://" + s3.Bucket + "/" + k.Key, Self: segs[2] == fp})
		}
	}
	return out, nil
}

// markerRecentWindow is how long after the last write to a namespace its markers
// count as live: a server that backs up at all writes within a week.
const markerRecentWindow = 7 * 24 * time.Hour

// ErrMarkerRecent marks a clear refused because the namespace is in use.
var ErrMarkerRecent = errors.New("recently active backup namespace")

// CheckMarkersClearable says whether a clear here would be refused, removing nothing:
// ErrMarkerRecent if so, a plain error if this location did not answer. A clear across
// several locations asks every one first, so that a refusal removes nothing anywhere.
func (c *Client) CheckMarkersClearable(ctx context.Context, s3 S3Config, fingerprint string, now time.Time) error {
	if !ownerMarkerRE.MatchString(fingerprint) {
		return fmt.Errorf("invalid fingerprint %q", fingerprint)
	}
	markers, err := c.ListNamespaceMarkers(ctx, s3)
	if err != nil {
		return err
	}
	return c.checkMarkersIdle(ctx, s3, markers, fingerprint, now)
}

func (c *Client) checkMarkersIdle(ctx context.Context, s3 S3Config, markers []NamespaceMarker, fingerprint string, now time.Time) error {
	for _, m := range markers {
		if m.Fingerprint != fingerprint {
			continue
		}
		keys, err := c.listTree(ctx, s3, m.Namespace+"/")
		if err != nil {
			return fmt.Errorf("%w: could not tell how recently %s was written to, so nothing was removed (--force to remove it anyway): %v", ErrMarkerRecent, m.Namespace, err)
		}
		for _, k := range keys {
			if !k.Modified.IsZero() && now.Sub(k.Modified) < markerRecentWindow {
				return fmt.Errorf("%w: %s was written to on %s, so %s may be a live server, and without its marker a prune here deletes its backups; nothing was removed. If that server is really gone: --force", ErrMarkerRecent, m.Namespace, k.Modified.Format("2006-01-02"), fingerprint)
			}
		}
	}
	return nil
}

// ClearNamespaceMarkers removes a fingerprint's markers (a server that no longer
// exists, a rebuild's old self), returning the keys removed. The marker is all that
// stops another server's prune from deleting that server's objects, so a namespace
// written to in the last week is refused without force: its marker may be a server
// that is still running. Markers are not rewritten, so their own date says nothing
// about that; the namespace's newest write does. All of them or none.
func (c *Client) ClearNamespaceMarkers(ctx context.Context, s3 S3Config, fingerprint string, force bool, now time.Time) ([]string, error) {
	if !ownerMarkerRE.MatchString(fingerprint) {
		return nil, fmt.Errorf("invalid fingerprint %q", fingerprint)
	}
	markers, err := c.ListNamespaceMarkers(ctx, s3)
	if err != nil {
		return nil, err
	}
	if !force {
		if err := c.checkMarkersIdle(ctx, s3, markers, fingerprint, now); err != nil {
			return nil, err
		}
	}
	var removed []string
	var errs []error
	for _, m := range markers {
		if m.Fingerprint != fingerprint {
			continue
		}
		if _, err := c.exec.Run(ctx, s3.AWS("s3 rm "+ssh.ShellQuote(m.Key))); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, m.Key)
	}
	return removed, errors.Join(errs...)
}
