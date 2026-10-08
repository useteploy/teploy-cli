// Package trigger owns durable cross-delivery operation admission. A delivery
// ledger is deliberately not an authority for deployment completion.
package trigger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxRequestBytes = 32 << 10
const Domain = "teploy-trigger-operation-v1\x00"

type Request struct {
	SchemaVersion            int        `json:"schema_version"`
	OperationKey             string     `json:"operation_key"`
	TargetID                 string     `json:"target_id"`
	SourceID                 string     `json:"source_id"`
	RepositoryID             string     `json:"repository_id"`
	Ref                      string     `json:"ref"`
	Commit                   string     `json:"commit"`
	ExecutionBindingDigest   string     `json:"execution_binding_digest"`
	Action                   string     `json:"action"`
	PreviewIdentity          string     `json:"preview_identity,omitempty"`
	ExpectedGeneration       uint64     `json:"expected_generation"`
	ExpectedPreviewUpdatedAt *time.Time `json:"expected_preview_updated_at,omitempty"`
}
type Result struct {
	SchemaVersion       int        `json:"schema_version"`
	OperationKey        string     `json:"operation_key"`
	Publication         string     `json:"publication"`
	Reconciliation      string     `json:"reconciliation"`
	Generation          *uint64    `json:"generation,omitempty"`
	ReleaseHash         string     `json:"release_hash,omitempty"`
	ImageDigest         string     `json:"image_digest,omitempty"`
	AuthorityObservedAt *time.Time `json:"authority_observed_at,omitempty"`
	ErrorCode           string     `json:"error_code,omitempty"`
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
var targetID = regexp.MustCompile(`^[0-9a-f]{32}/[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Key uses Go encoding/json's compact string-array encoding (including its
// HTML escaping), UTF-8, no trailing newline. Generation is a decimal STRING
// within the array; this prevents language-specific JSON number formatting.
func (r Request) Key() string {
	b, _ := json.Marshal([]string{r.TargetID, r.SourceID, r.RepositoryID, r.Ref, r.Commit, r.ExecutionBindingDigest, r.Action, r.PreviewIdentity, strconv.FormatUint(r.ExpectedGeneration, 10)})
	h := sha256.Sum256(append([]byte(Domain), b...))
	return hex.EncodeToString(h[:])
}
func (r Request) Validate() error {
	if r.SchemaVersion != 1 || !targetID.MatchString(r.TargetID) || !commitID.MatchString(r.Commit) || strings.Trim(r.Commit, "0") == "" || !hex64.MatchString(r.ExecutionBindingDigest) {
		return fmt.Errorf("invalid immutable trigger identity")
	}
	for _, s := range []string{r.SourceID, r.RepositoryID, r.Ref, r.PreviewIdentity} {
		if len(s) > 4096 || strings.ContainsAny(s, "\x00\r\n") {
			return fmt.Errorf("invalid trigger identity field")
		}
	}
	if r.SourceID == "" || r.RepositoryID == "" || r.Ref == "" {
		return fmt.Errorf("missing source, repository or exact ref")
	}
	if _, err := ParseRepositoryIdentity(r.RepositoryID); err != nil {
		return err
	}
	switch r.Action {
	case "deploy":
		if r.PreviewIdentity != "" {
			return fmt.Errorf("deploy cannot name a preview")
		}
	case "preview_create", "preview_destroy":
		if r.PreviewIdentity == "" {
			return fmt.Errorf("preview ownership identity required")
		}
		if r.Action == "preview_destroy" && r.ExpectedGeneration == 0 {
			return fmt.Errorf("destroy requires an observed generation")
		}
	default:
		return fmt.Errorf("invalid trigger action")
	}
	if !hex64.MatchString(r.OperationKey) || r.OperationKey != r.Key() {
		return fmt.Errorf("operation key does not bind canonical fields")
	}
	return nil
}
func Decode(in io.Reader) (Request, error) {
	var r Request
	b, err := io.ReadAll(io.LimitReader(in, MaxRequestBytes+1))
	if err != nil {
		return r, err
	}
	if len(b) > MaxRequestBytes {
		return r, fmt.Errorf("trigger request exceeds %d bytes", MaxRequestBytes)
	}
	// Reject duplicate JSON object keys as well as unknown fields: accepting
	// ambiguous requests would give different consumers different identities.
	d := json.NewDecoder(strings.NewReader(string(b)))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return r, fmt.Errorf("trigger request must be an object")
	}
	seen := map[string]bool{}
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return r, e
		}
		k, ok := t.(string)
		if !ok || seen[k] {
			return r, fmt.Errorf("duplicate trigger field")
		}
		seen[k] = true
		var v json.RawMessage
		if e = d.Decode(&v); e != nil {
			return r, e
		}
	}
	if _, err = d.Token(); err != nil {
		return r, err
	}
	if _, err = d.Token(); err != io.EOF {
		return r, fmt.Errorf("trailing trigger JSON")
	}
	for _, key := range []string{"schema_version", "operation_key", "target_id", "source_id", "repository_id", "ref", "commit", "execution_binding_digest", "action", "expected_generation"} {
		if !seen[key] {
			return r, fmt.Errorf("missing required trigger field")
		}
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		return r, err
	}
	for _, v := range raw {
		if string(v) == "null" {
			return r, fmt.Errorf("null trigger fields are forbidden")
		}
	}
	d = json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&r); err != nil {
		return r, err
	}
	return r, r.Validate()
}
func Initial(r Request) Result {
	key := r.OperationKey
	if !hex64.MatchString(key) {
		key = ""
	}
	return Result{SchemaVersion: 1, OperationKey: key, Publication: "not_started", Reconciliation: "required"}
}

func ValidCommit(commit string) bool {
	return commitID.MatchString(commit) && strings.Trim(commit, "0") != ""
}
