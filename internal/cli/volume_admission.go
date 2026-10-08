package cli

import (
	"context"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"sort"
)

// First validate every explicit root without mutation, then provision. This
// precedes accessory startup, agent startup and migration stop/copy.
func admitDeploymentVolumes(ctx context.Context, exec ssh.Executor, cfg *config.AppConfig) error {
	dk := docker.NewClient(exec)
	for _, name := range volumeKeys(cfg.VolumeOwnership) {
		if err := dk.ValidateManagedVolume(ctx, cfg.App, "", name, cfg.VolumeOwnership[name]); err != nil {
			return err
		}
	}
	for _, accessory := range sortedAccessoryNames(cfg.Accessories) {
		acc := cfg.Accessories[accessory]
		for _, name := range volumeKeys(acc.VolumeOwnership) {
			if err := dk.ValidateManagedVolume(ctx, cfg.App, accessory, name, acc.VolumeOwnership[name]); err != nil {
				return err
			}
		}
	}
	for _, name := range volumeKeys(cfg.VolumeOwnership) {
		if err := dk.ProvisionManagedVolume(ctx, cfg.App, "", name, cfg.VolumeOwnership[name]); err != nil {
			return err
		}
	}
	for _, accessory := range sortedAccessoryNames(cfg.Accessories) {
		acc := cfg.Accessories[accessory]
		for _, name := range volumeKeys(acc.VolumeOwnership) {
			if err := dk.ProvisionManagedVolume(ctx, cfg.App, accessory, name, acc.VolumeOwnership[name]); err != nil {
				return err
			}
		}
	}
	return nil
}

func volumeKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
