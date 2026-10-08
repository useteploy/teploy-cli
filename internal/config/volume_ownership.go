package config

import (
	"fmt"
	"regexp"
	"strconv"
)

// VolumeOwnership initializes the root of a Teploy-managed named volume.
// Pointers require explicit IDs, including a deliberately selected zero.
type VolumeOwnership struct {
	UID  *uint32 `yaml:"uid" toml:"uid" json:"uid"`
	GID  *uint32 `yaml:"gid" toml:"gid" json:"gid"`
	Mode string  `yaml:"mode" toml:"mode" json:"mode"`
}

var volumeMode = regexp.MustCompile(`^0[0-7]{3}$`)

func (v VolumeOwnership) Validate() error {
	if v.UID == nil || v.GID == nil || *v.UID == ^uint32(0) || *v.GID == ^uint32(0) {
		return fmt.Errorf("volume ownership requires numeric uid/gid in 0..4294967294")
	}
	if !volumeMode.MatchString(v.Mode) {
		return fmt.Errorf("volume ownership mode must be a quoted four-digit octal mode, e.g. \"0700\"")
	}
	mode, _ := strconv.ParseUint(v.Mode, 8, 16)
	if mode&0002 != 0 {
		return fmt.Errorf("volume ownership mode must not be world-writable")
	}
	return nil
}

func ValidateVolumeOwnership(volumes map[string]string, owners map[string]VolumeOwnership) error {
	for name, permission := range owners {
		if err := ValidateIdentifier("volume", name); err != nil {
			return err
		}
		if _, ok := volumes[name]; !ok {
			return fmt.Errorf("volume_ownership.%s must name an entry in volumes", name)
		}
		if err := permission.Validate(); err != nil {
			return fmt.Errorf("volume_ownership.%s: %w", name, err)
		}
	}
	return nil
}
