package cli

import (
	"encoding/json"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
)

// deployConfigFromApp maps a loaded AppConfig onto the deploy engine's
// Config. Both deploy entry points (single-server and multi-server)
// build their Config here so the config→deploy seam is one tested
// mapping: an imported application port (AppConfig.Port) reaches
// deploy.Config.ContainerPort exactly once, observably.
func deployConfigFromApp(appCfg *config.AppConfig, image, version string, envFiles []string, volumes map[string]string, tlsCert, tlsKey string, tlsInternal bool, appliedManifest json.RawMessage, manifestSHA256 string) deploy.Config {
	return deploy.Config{
		App:             appCfg.App,
		Domain:          appCfg.Domain,
		Image:           image,
		Version:         version,
		EnvFiles:        envFiles,
		Volumes:         volumes,
		Processes:       appCfg.Processes,
		NoHealthcheck:   disabledHealthchecks(appCfg.Healthcheck),
		Health:          healthConfigFrom(appCfg.Health),
		KeepVersions:    appCfg.KeepVersions,
		Ingress:         appCfg.Ingress,
		Bind:            appCfg.Bind,
		ContainerPort:   appCfg.Port,
		Publish:         appCfg.Publish,
		StopTimeout:     appCfg.StopTimeout,
		Memory:          appCfg.Memory,
		CPU:             appCfg.CPU,
		Replicas:        appCfg.Replicas,
		PreDeploy:       appCfg.Hooks.PreDeploy,
		PostDeploy:      appCfg.Hooks.PostDeploy,
		AssetPath:       appCfg.Assets.Path,
		AssetKeepDays:   appCfg.Assets.KeepDays,
		TLSCert:         tlsCert,
		TLSKey:          tlsKey,
		TLSInternal:     tlsInternal,
		CaddyExtra:      appCfg.CaddyExtra,
		Cache:           appCfg.Cache,
		Firewall:        caddyFirewall(appCfg.Firewall),
		Access:          caddyAccess(appCfg.Access),
		ManifestSHA256:  manifestSHA256,
		AppliedManifest: appliedManifest,
		SourceRevision:  appCfg.SourceRevision,
	}
}
