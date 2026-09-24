package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
)

// The disaster-recovery command family (C07). `teploy backup` remains the
// DATA-ONLY family (volume archives, single-accessory dumps); `teploy dr`
// is the whole-application bundle: state, releases, secrets-by-reference,
// routing identity, and consistency-labeled snapshots, restored to an
// isolated target and promoted only by an explicit cutover.
func newDRCmd(flags *Flags, version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dr",
		Short: "Disaster-recovery bundles: whole-app backup, isolated restore, explicit cutover",
		Long: "Create and restore versioned disaster-recovery bundles.\n" +
			"A bundle carries app state, release records, secret references (or\n" +
			"encrypted material you explicitly opt into), routing/TLS references and\n" +
			"engine-aware data snapshots — each labeled with the consistency it\n" +
			"actually achieved. Restores land in an ISOLATED staging area and are\n" +
			"validated before `teploy dr cutover` promotes them; live overwrite is\n" +
			"never the default.\n" +
			"For plain volume data without app state use `teploy backup create` — " +
			"that family is data-only and stays clearly named as such.",
	}

	cmd.AddCommand(newDRCreateCmd(flags, version))
	cmd.AddCommand(newDRListCmd(flags))
	cmd.AddCommand(newDRShowCmd(flags))
	cmd.AddCommand(newDRRestoreCmd(flags))
	cmd.AddCommand(newDRCutoverCmd(flags))
	return cmd
}

// drStoreFlags registers the bundle-store selection: an S3 target
// (--bucket) or a plain directory reachable on the server (--dir) for
// offline bundles.
func drStoreFlags(cmd *cobra.Command, bucket, region, endpoint, dir *string) {
	cmd.Flags().StringVar(bucket, "bucket", "", "S3 bucket for bundles")
	cmd.Flags().StringVar(region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	cmd.Flags().StringVar(dir, "dir", "", "store bundles in a directory on the server (offline bundles) instead of S3")
}

// drStore builds the BundleStore from the flags; exactly one target must
// be selected.
func drStore(bucket, region, endpoint, dir string) (backup.BundleStore, error) {
	switch {
	case dir != "" && bucket != "":
		return nil, fmt.Errorf("choose one bundle target: --bucket (S3) or --dir (server directory), not both")
	case dir != "":
		return backup.DirBundleStore{Root: dir}, nil
	case bucket != "":
		if err := backup.ValidateBucket(bucket); err != nil {
			return nil, err
		}
		if err := backup.ValidateRegion(region); err != nil {
			return nil, err
		}
		return backup.S3BundleStore{S3: s3Config(bucket, region, endpoint)}, nil
	}
	return nil, fmt.Errorf("a bundle target is required: --bucket (S3) or --dir (server directory)")
}

func newDRCreateCmd(flags *Flags, version string) *cobra.Command {
	var (
		bucket, region, endpoint, dir string
		includeSecrets, includeAgeKey bool
		stopApp                       bool
		quiescedVolumes               []string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a disaster-recovery bundle of the whole app",
		Long: "Captures app state, release records, the applied manifest, secret\n" +
			"references, routing/TLS references and data snapshots into one\n" +
			"schema-versioned bundle. Secret MATERIAL is included only with\n" +
			"--include-secrets (age ciphertexts, resolved .env, accessory\n" +
			"credentials), and the age key itself only with --include-age-key.\n" +
			"Each snapshot's consistency level is recorded: engine dumps are\n" +
			"engine-consistent; raw volume copies are crash-consistent unless the\n" +
			"app is stopped (--stop-app) or a volume is asserted quiesced\n" +
			"(--quiesced-volume NAME).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := drStore(bucket, region, endpoint, dir)
			if err != nil {
				return err
			}
			if includeAgeKey && !includeSecrets {
				return fmt.Errorf("--include-age-key requires --include-secrets (the key decrypts the material it travels with)")
			}
			appCfg, err := config.LoadApp(".")
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			executor, err := connectForApp(ctx, flags, appCfg)
			if err != nil {
				return err
			}
			defer executor.Close()
			client := backup.NewClient(executor, os.Stdout)
			m, err := client.CreateBundle(ctx, backup.BundleOptions{
				App:            appCfg.App,
				Config:         *appCfg,
				IncludeSecrets: includeSecrets,
				IncludeAgeKey:  includeAgeKey,
				StopApp:        stopApp,
				VolumeQuiesced: quiescedVolumes,
				Version:        version,
			}, store)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(m)
			}
			fmt.Printf("Bundle %s:\n", m.ID)
			for _, s := range m.Snapshots {
				note := ""
				if s.Notes != "" {
					note = " — " + s.Notes
				}
				fmt.Printf("  %-14s %-10s %-18s %s%s\n", s.Name, s.Engine, s.Consistency, s.Artifact, note)
			}
			fmt.Printf("  secrets: %s (%d keys)\n", m.Secrets.Mode, len(m.Secrets.Keys))
			return nil
		},
	}
	drStoreFlags(cmd, &bucket, &region, &endpoint, &dir)
	cmd.Flags().BoolVar(&includeSecrets, "include-secrets", false, "include encrypted secret material (age ciphertexts, resolved .env, accessory credentials) — never default")
	cmd.Flags().BoolVar(&includeAgeKey, "include-age-key", false, "also include /deployments/.age-key so a fresh host can decrypt (requires --include-secrets)")
	cmd.Flags().BoolVar(&stopApp, "stop-app", false, "stop app containers for quiesced volume snapshots (restarted after)")
	cmd.Flags().StringSliceVar(&quiescedVolumes, "quiesced-volume", nil, "volume NAME whose writer you stopped by hand (repeatable); recorded as quiesced, operator-asserted")
	return cmd
}

func newDRListCmd(flags *Flags) *cobra.Command {
	var bucket, region, endpoint, dir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List DR bundles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := drStore(bucket, region, endpoint, dir)
			if err != nil {
				return err
			}
			appCfg, err := config.LoadApp(".")
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			executor, err := connectForApp(ctx, flags, appCfg)
			if err != nil {
				return err
			}
			defer executor.Close()
			ids, err := store.ListIDs(ctx, executor, appCfg.App)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(ids)
			}
			if len(ids) == 0 {
				fmt.Println("No DR bundles found")
				return nil
			}
			for _, id := range ids {
				fmt.Println(id)
			}
			return nil
		},
	}
	drStoreFlags(cmd, &bucket, &region, &endpoint, &dir)
	return cmd
}

func newDRShowCmd(flags *Flags) *cobra.Command {
	var bucket, region, endpoint, dir string
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a DR bundle's manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := drStore(bucket, region, endpoint, dir)
			if err != nil {
				return err
			}
			appCfg, err := config.LoadApp(".")
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			executor, err := connectForApp(ctx, flags, appCfg)
			if err != nil {
				return err
			}
			defer executor.Close()
			data, err := store.FetchManifest(ctx, executor, appCfg.App, args[0])
			if err != nil {
				return err
			}
			m, err := backup.ParseBundleManifest(data)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(m)
			}
			fmt.Printf("Bundle %s (app %s, created %s by %s)\n", m.ID, m.App, m.CreatedAt.Format("2006-01-02 15:04:05 MST"), m.CreatedBy)
			for _, s := range m.Snapshots {
				fmt.Printf("  %-14s %-10s %-18s %s\n", s.Name, s.Engine, s.Consistency, s.Artifact)
				if s.Notes != "" {
					fmt.Printf("      %s\n", s.Notes)
				}
			}
			fmt.Printf("  secrets: %s (%d keys, age-key included: %v)\n", m.Secrets.Mode, len(m.Secrets.Keys), m.Secrets.AgeKeyIncluded)
			fmt.Printf("  routing: domain=%s ingress=%s\n", m.Routing.Domain, m.Routing.IngressMode)
			fmt.Println("  recovery plan:")
			for _, step := range m.Recovery.Steps {
				manual := ""
				if step.Manual {
					manual = " [manual]"
				}
				fmt.Printf("    %-20s %s%s\n", step.Action, step.Summary, manual)
			}
			return nil
		},
	}
	drStoreFlags(cmd, &bucket, &region, &endpoint, &dir)
	return cmd
}

func newDRRestoreCmd(flags *Flags) *cobra.Command {
	var bucket, region, endpoint, dir string
	cmd := &cobra.Command{
		Use:   "restore <id>",
		Short: "Restore a DR bundle to an ISOLATED target and validate it (live state untouched)",
		Long: "Downloads the bundle into /var/tmp/teploy-dr staging, boots scratch\n" +
			"engines and a scratch app container against the STAGED data, and checks\n" +
			"the restored copy is actually usable. Writes a receipt with measured\n" +
			"RPO/RTO. Nothing under /deployments is touched — promotion happens only\n" +
			"via `teploy dr cutover`.\n" +
			"Missing secret keys (references mode) and undecryptable material fail\n" +
			"BEFORE any mutation.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := drStore(bucket, region, endpoint, dir)
			if err != nil {
				return err
			}
			appCfg, err := config.LoadApp(".")
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			executor, err := connectForApp(ctx, flags, appCfg)
			if err != nil {
				return err
			}
			defer executor.Close()
			client := backup.NewClient(executor, os.Stdout)
			receipt, err := client.RestoreBundleIsolated(ctx, backup.BundleRestoreOptions{
				App:    appCfg.App,
				ID:     args[0],
				Config: *appCfg,
			}, store)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(receipt)
			}
			printReceipt(receipt)
			if !receipt.OK {
				return fmt.Errorf("restore validation failed — see the receipt; nothing live was touched")
			}
			return nil
		},
	}
	drStoreFlags(cmd, &bucket, &region, &endpoint, &dir)
	return cmd
}

func printReceipt(r *backup.RestoreReceipt) {
	fmt.Printf("Restore receipt for bundle %s:\n", r.BundleID)
	fmt.Printf("  staging: %s\n", r.StagingPath)
	fmt.Printf("  RPO: %ds (data age at restore time)\n", r.RPOSeconds)
	fmt.Printf("  RTO: %ds (measured restore + validation)\n", r.RTOSeconds)
	for _, ck := range r.Checks {
		fmt.Printf("  check %-28s %-7s %s %s\n", ck.Name, ck.Status, ck.Metric, ck.Detail)
	}
	fmt.Printf("  next: %s\n", r.NextStep)
}

func newDRCutoverCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cutover <id>",
		Short: "Explicitly promote a validated staged restore over the live app",
		Long: "The mutation step — never run by accident. Requires a staged restore\n" +
			"that PASSED validation (`teploy dr restore`). Stops live containers,\n" +
			"restores engine dumps into fresh accessories, promotes staged volumes\n" +
			"with two-phase recovery (pre-cutover originals are kept), and installs\n" +
			"the bundle's state/release records/secrets. Finishes by pointing at\n" +
			"`teploy deploy` to bring the app container and routing live.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appCfg, err := config.LoadApp(".")
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			executor, err := connectForApp(ctx, flags, appCfg)
			if err != nil {
				return err
			}
			defer executor.Close()
			client := backup.NewClient(executor, os.Stdout)
			receipt, err := client.CutoverBundle(ctx, backup.BundleRestoreOptions{
				App:    appCfg.App,
				ID:     args[0],
				Config: *appCfg,
			})
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(receipt)
			}
			fmt.Printf("Cutover receipt for bundle %s: %d path(s) promoted, originals kept in %v\n",
				receipt.BundleID, len(receipt.Promoted), receipt.RecoveryDirs)
			return nil
		},
	}
	return cmd
}
