package backup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/useteploy/teploy/internal/ssh"
)

// MaxObjectStartBytes caps a ranged read: enough for an age header and its first
// chunk, which is what proving a recovery key needs, and small enough to pass through
// a JSON envelope.
const MaxObjectStartBytes = 256 << 10

// ErrKeyOutsideTarget refuses a key that is not under this server's own prefix or one
// of its previous prefixes.
var ErrKeyOutsideTarget = errors.New("the key is outside this server's backup target")

const objectStartBodyMark = "TEPLOY-OBJECT-START-BODY"

// ObjectStart is the first bytes of a stored object, exactly as stored: nothing here
// decrypts.
type ObjectStart struct {
	Key  string
	Data []byte
	// TotalSize is the whole object's size, nil when the store's reply did not say.
	TotalSize *int64
	Encrypted bool
}

// CheckOwnKey refuses a key outside this config's bucket and its current and previous
// prefixes, and any key with a segment a store or client might normalize away.
func (s3 S3Config) CheckOwnKey(key string) error {
	bucket, k, err := splitS3URI(key)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeyOutsideTarget, err)
	}
	if bucket != s3.Bucket {
		return fmt.Errorf("%w: it is in bucket %q, and this server's target is %q", ErrKeyOutsideTarget, bucket, s3.Bucket)
	}
	if strings.IndexFunc(k, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: it contains control characters", ErrKeyOutsideTarget)
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: it has an empty, . or .. segment", ErrKeyOutsideTarget)
		}
	}
	for _, loc := range s3.locations() {
		if loc.Prefix == "" || strings.HasPrefix(k, loc.Prefix+"/") {
			return nil
		}
	}
	return fmt.Errorf("%w: it is not under the prefix %q or a previous prefix", ErrKeyOutsideTarget, s3.Prefix)
}

// ReadObjectStart reads the first n bytes of an object through the stored target. The
// body sits in a private temp file only long enough to encode it, because aws writes
// its reply to stdout and the object to a file. The name is one the janitor knows, if
// a kill leaves it behind.
func (c *Client) ReadObjectStart(ctx context.Context, s3 S3Config, key string, n int) (ObjectStart, error) {
	var none ObjectStart
	if n < 1 || n > MaxObjectStartBytes {
		return none, fmt.Errorf("--bytes must be between 1 and %d (256 KiB)", MaxObjectStartBytes)
	}
	if err := s3.CheckOwnKey(key); err != nil {
		return none, err
	}
	if err := c.ensureAWSCLI(ctx); err != nil {
		return none, err
	}
	bucket, k, err := splitS3URI(key)
	if err != nil {
		return none, err
	}
	q := ssh.ShellQuote
	get := s3.AWS("s3api get-object --bucket " + q(bucket) + " --key " + q(k) + " --range " + q(fmt.Sprintf("bytes=0-%d", n-1)) + ` --output json "$d/o"`)
	script := "umask 077\n" +
		"d=$(mktemp -d " + q(backupTmpDir()+"/teploy-backup.XXXXXXXX") + ") || exit 3\n" +
		`trap 'rm -rf -- "$d"' EXIT` + "\n" +
		get + ` > "$d/m" || exit $?` + "\n" +
		fmt.Sprintf(`[ "$(wc -c < "$d/o")" -le %d ] || { echo "the store ignored the requested range" >&2; exit 4; }`, n) + "\n" +
		`cat "$d/m"` + "\n" +
		`printf '\n%s\n' ` + objectStartBodyMark + "\n" +
		`base64 < "$d/o"`
	// Base64 of the cap, plus aws's reply: well under this.
	res := ssh.RunDetailedLimit(ctx, c.exec, "sh -c "+q(script), 1<<20)
	if res.Failed() {
		reason := oneLine(res.ExitErrorText())
		if reason == "" {
			reason = fmt.Sprintf("the command exited with status %d and gave no reason", res.ExitCode)
		}
		return none, fmt.Errorf("reading %s: %s", key, reason)
	}
	if res.Truncated {
		return none, fmt.Errorf("reading %s: the reply was larger than the requested range", key)
	}
	meta, body, ok := strings.Cut(string(res.Stdout), objectStartBodyMark)
	if !ok {
		return none, fmt.Errorf("reading %s: unrecognized output", key)
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return none, fmt.Errorf("reading %s: unreadable body: %w", key, err)
	}
	var reply struct {
		ContentLength *int64 `json:"ContentLength"`
		ContentRange  string `json:"ContentRange"`
	}
	if err := json.Unmarshal([]byte(meta), &reply); err != nil {
		return none, fmt.Errorf("reading %s: unexpected reply: %w", key, err)
	}
	if reply.ContentLength != nil && *reply.ContentLength != int64(len(data)) {
		return none, fmt.Errorf("reading %s: the store said %d bytes and sent %d", key, *reply.ContentLength, len(data))
	}
	if len(data) == 0 {
		return none, fmt.Errorf("reading %s: the store sent no bytes", key)
	}
	return ObjectStart{Key: key, Data: data, TotalSize: contentRangeTotal(reply.ContentRange), Encrypted: strings.HasSuffix(k, ageSuffix)}, nil
}

// contentRangeTotal reads "bytes 0-9/1234".
func contentRangeTotal(s string) *int64 {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return nil
	}
	total, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil {
		return nil
	}
	return &total
}
