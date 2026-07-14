package openbao

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/ssh"
)

// resolveSeal computes the SealSpec + credential env for a setup. Default
// (empty/static) generates + manages the key in the local store; awskms/transit
// come from opts. Shared by single-node and HA paths (all HA nodes use the same
// seal so each can auto-unseal independently before joining the quorum).
func (c *Client) resolveSeal(ctx context.Context, opts SetupOptions) (SealSpec, map[string]string, error) {
	seal := opts.Seal
	sealEnv := map[string]string{}
	for k, v := range opts.SealEnv {
		sealEnv[k] = v
	}
	if seal.Type == "" || seal.Type == SealStatic {
		sealKey, err := c.ensureSecret(ctx, opts.App, secretSealKey, GenerateSealKey)
		if err != nil {
			return SealSpec{}, nil, err
		}
		keyID, err := c.ensureSecret(ctx, opts.App, secretSealKeyID, GenerateKeyID)
		if err != nil {
			return SealSpec{}, nil, err
		}
		seal = SealSpec{Type: SealStatic, KeyID: keyID}
		sealEnv["BAO_SEAL_KEY"] = sealKey
	}
	return seal, sealEnv, nil
}

// haNodeName returns the container/alias for node i. Node 0 keeps the base
// accessory name (so all downstream ops — KV, db, agent — target the primary
// without knowing it's HA); followers get a -N suffix.
func haNodeName(app, accessory string, i int) string {
	base := accessories.ContainerName(app, accessory)
	if i == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, i)
}

// provisionHANodes creates a Raft quorum of opts.Replicas OpenBao nodes. Each
// node gets a raft config with its own node_id + cluster_addr and retry_join
// entries for every peer, so followers auto-join the leader once it's
// initialized. All nodes share the seal, so each auto-unseals on boot.
func (c *Client) provisionHANodes(ctx context.Context, opts SetupOptions, seal SealSpec, sealEnv map[string]string) error {
	n := opts.Replicas
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = haNodeName(opts.App, opts.Accessory, i)
	}
	fmt.Fprintf(c.out, "Provisioning %d-node OpenBao Raft cluster...\n", n)

	for i := 0; i < n; i++ {
		self := names[i]
		var retry []string
		for j := 0; j < n; j++ {
			if j != i {
				retry = append(retry, "http://"+names[j]+":8200")
			}
		}
		cfg := RenderServerConfig(ServerConfig{
			StoragePath:   "/openbao/data",
			ListenAddr:    "0.0.0.0:8200",
			ClusterAddr:   "http://" + self + ":8201",
			TLSDisable:    true,
			APIAddr:       "http://" + self + ":8200",
			Seal:          seal,
			AuditFilePath: "/openbao/data/audit.log",
			NodeID:        self,
			RetryJoin:     retry,
		})
		confPath := fmt.Sprintf("/deployments/%s/accessories/%s/config-%d.hcl", opts.App, opts.Accessory, i)
		if err := c.exec.Upload(ctx, strings.NewReader(cfg), confPath, "0644"); err != nil {
			return fmt.Errorf("uploading node %d config: %w", i, err)
		}
		nodeOpts := opts
		nodeOpts.Accessory = opts.Accessory // container name computed below
		if err := c.ensureNamedContainer(ctx, nodeOpts, self, confPath, sealEnv); err != nil {
			return fmt.Errorf("starting node %s: %w", self, err)
		}
	}
	return nil
}

// waitForPeers polls raft list-peers until the cluster reports the expected
// node count (or times out). Confirms the followers joined the quorum.
func (c *Client) waitForPeers(ctx context.Context, leader, root string, want int, timeout time.Duration) error {
	fmt.Fprintf(c.out, "Waiting for %d-node quorum to form...\n", want)
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.bao(ctx, leader, root, "operator raft list-peers -format=json")
		if err == nil {
			var res struct {
				Data struct {
					Config struct {
						Servers []struct {
							NodeID string `json:"node_id"`
						} `json:"servers"`
					} `json:"config"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(extractJSON(out)), &res) == nil && len(res.Data.Config.Servers) >= want {
				fmt.Fprintf(c.out, "Raft quorum ready: %d nodes.\n", len(res.Data.Config.Servers))
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("raft quorum did not reach %d nodes within %s (followers may still be joining)", want, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ListPeers returns the raft cluster's node IDs (for `secret status` in HA).
func (c *Client) ListPeers(ctx context.Context, app, accessory string) ([]string, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return nil, err
	}
	container := accessories.ContainerName(app, accessory)
	out, err := c.bao(ctx, container, root, "operator raft list-peers -format=json")
	if err != nil {
		return nil, fmt.Errorf("listing raft peers: %s", truncate(out, 160))
	}
	var res struct {
		Data struct {
			Config struct {
				Servers []struct {
					NodeID string `json:"node_id"`
					Leader bool   `json:"leader"`
				} `json:"servers"`
			} `json:"config"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &res); err != nil {
		return nil, fmt.Errorf("parsing raft peers: %w", err)
	}
	peers := make([]string, 0, len(res.Data.Config.Servers))
	for _, s := range res.Data.Config.Servers {
		id := s.NodeID
		if s.Leader {
			id += " (leader)"
		}
		peers = append(peers, id)
	}
	return peers, nil
}

// ensureNamedContainer runs one OpenBao node with an explicit container name
// (HA nodes), reusing the volume-chown + seal-env plumbing of ensureContainer.
func (c *Client) ensureNamedContainer(ctx context.Context, opts SetupOptions, container, confPath string, sealEnv map[string]string) error {
	if out, err := c.exec.Run(ctx, fmt.Sprintf("docker inspect -f '{{.State.Status}}' %s 2>/dev/null", ssh.ShellQuote(container))); err == nil && strings.TrimSpace(out) == "running" {
		return nil
	}
	if _, err := c.exec.Run(ctx, "docker network inspect teploy >/dev/null 2>&1 || docker network create teploy"); err != nil {
		return fmt.Errorf("ensuring teploy network: %w", err)
	}
	c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(container)+" >/dev/null 2>&1 || true")

	var envBuf strings.Builder
	for k, v := range sealEnv {
		fmt.Fprintf(&envBuf, "%s=%s\n", k, v)
	}
	envFile := fmt.Sprintf("/deployments/%s/accessories/%s/.seal-env", opts.App, opts.Accessory)
	if err := c.exec.Upload(ctx, strings.NewReader(envBuf.String()), envFile, "0600"); err != nil {
		return fmt.Errorf("writing seal env: %w", err)
	}

	dataVol := container + "-data"
	if _, err := c.exec.Run(ctx, "docker volume create "+ssh.ShellQuote(dataVol)+" >/dev/null"); err != nil {
		return fmt.Errorf("creating data volume: %w", err)
	}
	chown := "docker run --rm --user 0:0 --entrypoint chown -v " + ssh.ShellQuote(dataVol) + ":/openbao/data " + ssh.ShellQuote(opts.Image) + " -R 100:1000 /openbao/data"
	if _, err := c.exec.Run(ctx, chown); err != nil {
		return fmt.Errorf("preparing data volume: %w", err)
	}
	run := strings.Join([]string{
		"docker run --detach --restart always",
		"--name " + ssh.ShellQuote(container),
		"--network teploy",
		"--network-alias " + ssh.ShellQuote(container),
		"--label teploy.app=" + ssh.ShellQuote(opts.App),
		"--label teploy.role=accessory",
		"--label teploy.accessory=" + ssh.ShellQuote(opts.Accessory),
		"--cap-add IPC_LOCK",
		"--env-file " + ssh.ShellQuote(envFile),
		"-v " + ssh.ShellQuote(confPath) + ":/openbao/config.hcl:ro",
		"-v " + ssh.ShellQuote(dataVol) + ":/openbao/data",
		"--log-opt max-size=10m",
		ssh.ShellQuote(opts.Image),
		"server -config=/openbao/config.hcl",
	}, " ")
	if _, err := c.exec.Run(ctx, run); err != nil {
		return fmt.Errorf("starting node container: %w", err)
	}
	return nil
}
