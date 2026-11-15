package cloud

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the credentials file, kept next to servers.yml.
const FileName = "cloud.yml"

// Credentials are what `teploy cloud login` stores.
type Credentials struct {
	BaseURL string `yaml:"base_url,omitempty"`
	OrgID   string `yaml:"org_id"`
	APIKey  string `yaml:"api_key"`
}

// ErrNotLoggedIn means there is no credentials file.
var ErrNotLoggedIn = errors.New("not logged in to Teploy Cloud: run `teploy cloud login`")

// LoadCredentials reads the credentials file at path.
func LoadCredentials(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Credentials
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.APIKey == "" || c.OrgID == "" {
		return nil, fmt.Errorf("%s is incomplete: run `teploy cloud login`", path)
	}
	return &c, nil
}

// SaveCredentials writes the credentials file atomically with mode 0600.
func SaveCredentials(path string, c *Credentials) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding credentials: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".cloud-*")
	if err != nil {
		return fmt.Errorf("creating temp credentials file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting credentials file permissions: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("writing credentials file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing credentials file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing credentials file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing credentials file: %w", err)
	}
	return nil
}

// RemoveCredentials deletes the credentials file. It reports whether a file
// was removed.
func RemoveCredentials(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("removing %s: %w", path, err)
	}
	return true, nil
}
