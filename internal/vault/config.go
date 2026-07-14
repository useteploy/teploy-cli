// Package vault integrates OpenBao (a Vault-compatible secrets manager) as a
// first-class Teploy accessory: provision, initialize, auto-unseal, and wire
// per-app least-privilege secret access. It reuses the accessory container
// plumbing for run/network/volume/restart and the age secret store for the
// seal key, root token, and AppRole credentials.
package vault

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// SealType selects the auto-unseal mechanism.
type SealType string

const (
	// SealStatic: key provided at boot (env://BAO_SEAL_KEY). Auto-unseals on
	// every restart with no ceremony. The universal self-host default; the key
	// is recoverable on the box (fine for the self-host threat model).
	SealStatic SealType = "static"
	// SealAWSKMS / SealTransit: key held off-box in a KMS or another Bao. The
	// upgrade for disk-theft/root-compromise threat models.
	SealAWSKMS  SealType = "awskms"
	SealTransit SealType = "transit"
)

// SealSpec configures the seal stanza.
type SealSpec struct {
	Type SealType
	// Static:
	KeyID string // current_key_id (stable UUID)
	// AWS KMS:
	KMSKeyID string
	KMSRegion string
	// Transit:
	TransitAddress string
	TransitKeyName string
	TransitMount   string
}

// ServerConfig is the input to the OpenBao server config.hcl.
type ServerConfig struct {
	StoragePath string   // container path for file storage, e.g. /openbao/data
	ListenAddr  string   // e.g. 0.0.0.0:8200
	TLSDisable  bool     // true only for private-mesh/dev; production terminates TLS
	TLSCertFile string   // container path (when TLSDisable is false)
	TLSKeyFile  string
	APIAddr     string
	Seal        SealSpec
	// AuditFilePath, when set, declares a file audit device (OpenBao 2.6+
	// manages audit devices declaratively in config, not via the API).
	AuditFilePath string
}

// RenderServerConfig produces the OpenBao config.hcl. disable_mlock is set
// because containers rarely grant IPC_LOCK; file storage keeps it single-binary
// with no external dependency (HA is a multi-container Raft upgrade, not here).
func RenderServerConfig(c ServerConfig) string {
	var b strings.Builder

	fmt.Fprintf(&b, "storage \"file\" {\n  path = %q\n}\n\n", c.StoragePath)

	b.WriteString("listener \"tcp\" {\n")
	fmt.Fprintf(&b, "  address = %q\n", c.ListenAddr)
	if c.TLSDisable {
		b.WriteString("  tls_disable = true\n")
	} else {
		fmt.Fprintf(&b, "  tls_cert_file = %q\n", c.TLSCertFile)
		fmt.Fprintf(&b, "  tls_key_file  = %q\n", c.TLSKeyFile)
	}
	b.WriteString("}\n\n")

	b.WriteString(renderSeal(c.Seal))

	if c.AuditFilePath != "" {
		fmt.Fprintf(&b, "\naudit \"file\" {\n  type      = \"file\"\n  file_path = %q\n}\n", c.AuditFilePath)
	}

	fmt.Fprintf(&b, "\napi_addr      = %q\n", c.APIAddr)
	b.WriteString("disable_mlock = true\n")
	return b.String()
}

func renderSeal(s SealSpec) string {
	var b strings.Builder
	switch s.Type {
	case SealAWSKMS:
		b.WriteString("seal \"awskms\" {\n")
		fmt.Fprintf(&b, "  region     = %q\n", s.KMSRegion)
		fmt.Fprintf(&b, "  kms_key_id = %q\n", s.KMSKeyID)
		b.WriteString("}\n")
	case SealTransit:
		b.WriteString("seal \"transit\" {\n")
		fmt.Fprintf(&b, "  address   = %q\n", s.TransitAddress)
		fmt.Fprintf(&b, "  key_name  = %q\n", s.TransitKeyName)
		fmt.Fprintf(&b, "  mount_path = %q\n", s.TransitMount)
		b.WriteString("}\n")
	default: // static
		b.WriteString("seal \"static\" {\n")
		fmt.Fprintf(&b, "  current_key_id = %q\n", s.KeyID)
		b.WriteString("  current_key    = \"env://BAO_SEAL_KEY\"\n")
		b.WriteString("}\n")
	}
	return b.String()
}

// GenerateSealKey returns a base64-encoded 32-byte key (AES-256) for the static
// seal.
func GenerateSealKey() (string, error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", fmt.Errorf("generating seal key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

// GenerateKeyID returns a stable UUID-shaped identifier for the seal key.
func GenerateKeyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating key id: %w", err)
	}
	// RFC-4122 v4 shape.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}
