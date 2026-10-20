package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/config"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
	"golang.org/x/term"
)

// `teploy server backup-target` (D3): one backup target per server, stored
// outside /deployments and read at run time by every backup command, cron
// line and DR invocation.

func newServerBackupTargetCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup-target",
		Short: "Manage the server's stored backup target",
		Long: "One S3 target per server: bucket, region, endpoint and an optional\n" +
			"key prefix, stored at /etc/teploy/backup-target.json (0600, outside\n" +
			"/deployments). Credentials, when given, are age-encrypted with the\n" +
			"server's own identity and are used whether or not an endpoint is set;\n" +
			"without them the server's ambient AWS credential chain applies.\n\n" +
			"A non-root user who cannot write /etc/teploy gets\n" +
			"~/.config/teploy/backup-target.json instead, and the command says so.\n\n" +
			"Setting a target rewrites teploy-managed backup cron lines to call\n" +
			"only `teploy backup run` (no bucket, no credentials) — hand-written\n" +
			"lines under a teploy marker are left alone and reported.",
	}
	cmd.AddCommand(newServerBackupTargetSetCmd(flags))
	cmd.AddCommand(newServerBackupTargetShowCmd(flags))
	cmd.AddCommand(newServerBackupTargetUnsetCmd(flags))
	return cmd
}

// backupTargetDTO is the `server backup-target show --json` envelope
// (backup-target-v1). It never carries secret material — only whether any
// is stored.
type backupTargetDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Host             string `json:"host"`
	Configured       bool   `json:"configured"`
	Bucket           string `json:"bucket,omitempty"`
	Region           string `json:"region,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	Prefix           string `json:"prefix,omitempty"`
	HasCredentials   bool   `json:"has_credentials"`
	Path             string `json:"path,omitempty"`
}

func newServerBackupTargetSetCmd(flags *Flags) *cobra.Command {
	var (
		bucket        string
		region        string
		endpoint      string
		prefix        string
		noCredentials bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Store the server's backup target (credentials on stdin)",
		Long: "Stores the target and (unless --no-credentials) the S3 credentials,\n" +
			"age-encrypted, on the server. Credentials are read from stdin as TWO\n" +
			"lines: the access key, then the secret key. With a terminal, both are\n" +
			"prompted with echo off.\n\n" +
			"Example:\n" +
			"  printf 'ACCESS\\nSECRET\\n' | teploy server backup-target set --host prod --bucket b --endpoint https://s3.example.com",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerBackupTargetSet(flags, bucket, region, endpoint, prefix, noCredentials)
		},
	}
	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "key prefix under the bucket (optional)")
	cmd.Flags().BoolVar(&noCredentials, "no-credentials", false, "store no credentials — use the server's ambient AWS credential chain")
	return cmd
}

func runServerBackupTargetSet(flags *Flags, bucket, region, endpoint, prefix string, noCredentials bool) error {
	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}
	if endpoint != "" {
		if err := backup.ValidateEndpoint(endpoint); err != nil {
			return err
		}
	}
	if err := backup.ValidatePrefix(prefix); err != nil {
		return err
	}

	accessKey, secretKey, err := readBackupTargetCredentials(os.Stdin, noCredentials)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()

	// Encrypt first (all failure modes leave the old target untouched).
	credsArmor := ""
	if accessKey != "" {
		if err := (secret.NewManager(executor)).EnsureAge(ctx); err != nil {
			return err
		}
		credsArmor, err = backup.EncryptCredentials(ctx, executor, accessKey, secretKey)
		if err != nil {
			return err
		}
	}
	t := backup.Target{Bucket: bucket, Region: region, Endpoint: endpoint, Prefix: prefix, HasCredentials: credsArmor != ""}
	path, fallback, err := backup.WriteTarget(ctx, executor, t, credsArmor)
	if err != nil {
		return err
	}
	t.Path = path

	fmt.Printf("Backup target stored: s3://%s at %s\n", backupTargetLabel(t), path)
	fmt.Printf("  Credentials: %s\n", credentialMode(t))
	if fallback {
		fmt.Println("  /etc/teploy is not writable by this user — the target lives in ~/.config/teploy instead")
	}

	// Best-effort reachability check through the exact run-time path
	// (stored creds decrypt + aws). A failure warns; it does not refuse the
	// set (the endpoint may be reachable only from cron's network, etc).
	checkReachability(ctx, executor, t)

	rewritten, kept, err := rewriteBackupCrons(ctx, executor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not rewrite backup cron lines: %v\n", err)
	} else {
		for _, m := range rewritten {
			fmt.Printf("  Rewrote cron marker %s to `backup run` (bucket/credentials no longer embedded)\n", m)
		}
		for _, m := range kept {
			fmt.Printf("  Cron marker %s was left alone (hand-written command) — check it still matches this target\n", m)
		}
	}

	recordBackupTargetEvent(ctx, executor, nil)
	return nil
}

func credentialMode(t backup.Target) string {
	if t.HasCredentials {
		return "stored, age-encrypted on the server"
	}
	return "none stored — the server's ambient AWS credential chain is used"
}

func checkReachability(ctx context.Context, executor ssh.Executor, t backup.Target) {
	cfg := TargetS3(t)
	client := backup.NewClient(executor, io.Discard)
	if err := client.EnsureAWSCLIPublic(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not verify the target: %v\n", err)
		return
	}
	res := ssh.RunDetailed(ctx, executor, cfg.AWS("s3 ls s3://"+cfg.Bucket+"/"))
	if res.Failed() {
		fmt.Fprintf(os.Stderr, "Warning: the stored target did not answer `aws s3 ls` (%s) — check the bucket, endpoint and credentials\n", oneLine(res.ExitErrorText()))
		return
	}
	fmt.Println("  Reachability: aws s3 ls answered")
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// readBackupTargetCredentials reads the two-line credential pair. With a
// terminal both values are prompted (echo off); otherwise stdin must carry
// exactly two lines. An empty access key with --no-credentials (or empty
// stdin lines declined interactively) stores no credentials.
func readBackupTargetCredentials(r io.Reader, noCredentials bool) (accessKey, secretKey string, err error) {
	if noCredentials {
		return "", "", nil
	}
	if stdinIsTerminal() {
		fmt.Print("S3 access key (empty for none): ")
		accessKey, err = readHiddenLine()
		if err != nil {
			return "", "", err
		}
		if accessKey == "" {
			return "", "", nil
		}
		fmt.Print("S3 secret key: ")
		secretKey, err = readHiddenLine()
		if err != nil {
			return "", "", err
		}
		fmt.Println()
		if secretKey == "" {
			return "", "", fmt.Errorf("a secret key is required when an access key is given")
		}
		return accessKey, secretKey, nil
	}
	lines, err := readTwoLines(r)
	if err != nil {
		return "", "", err
	}
	if lines[0] == "" && lines[1] == "" {
		return "", "", fmt.Errorf("credentials are required on stdin (two lines: access key, then secret key) — or pass --no-credentials to use the server's ambient AWS chain")
	}
	if lines[0] == "" || lines[1] == "" {
		return "", "", fmt.Errorf("stdin must carry BOTH the access key and the secret key, one per line")
	}
	return lines[0], lines[1], nil
}

// readHiddenLine reads one line from the terminal with echo off.
func readHiddenLine() (string, error) {
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readTwoLines(r io.Reader) ([2]string, error) {
	var lines [2]string
	buf := make([]byte, 0, 512)
	var got [2]string
	idx := 0
	ch := make([]byte, 1)
	for n, err := r.Read(ch); err == nil && n > 0; n, err = r.Read(ch) {
		if ch[0] == '\n' {
			got[idx] = strings.TrimSuffix(string(buf), "\r")
			buf = buf[:0]
			idx++
			if idx == 2 {
				return got, nil
			}
			continue
		}
		if len(buf) < 512 {
			buf = append(buf, ch[0])
		}
	}
	if idx == 1 && len(buf) > 0 {
		got[1] = strings.TrimSuffix(string(buf), "\r")
		return got, nil
	}
	if idx == 0 && len(buf) > 0 {
		got[0] = strings.TrimSuffix(string(buf), "\r")
		return got, nil
	}
	return lines, fmt.Errorf("stdin must carry two lines: the access key, then the secret key")
}

func newServerBackupTargetShowCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the stored backup target (never credentials)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerBackupTargetShow(flags)
		},
	}
	return cmd
}

func runServerBackupTargetShow(flags *Flags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()

	target, err := backup.ReadTarget(ctx, executor)
	if err != nil {
		return err
	}
	if flags.JSON {
		return writeBackupTargetJSON(os.Stdout, executor.Host(), target)
	}
	if !TargetConfigured(target) {
		fmt.Println("No backup target configured — set one with `teploy server backup-target set`")
		return nil
	}
	fmt.Printf("Backup target: s3://%s\n", backupTargetLabel(target))
	if target.Endpoint != "" {
		fmt.Printf("  Endpoint: %s\n", target.Endpoint)
	}
	fmt.Printf("  Region: %s\n", target.Region)
	fmt.Printf("  Credentials: %s\n", credentialMode(target))
	fmt.Printf("  Stored at: %s\n", target.Path)
	return nil
}

func writeBackupTargetJSON(w io.Writer, host string, target backup.Target) error {
	dto := backupTargetDTO{
		MachineInterface: MachineInterface,
		Host:             host,
		HasCredentials:   target.HasCredentials,
		Path:             target.Path,
	}
	if TargetConfigured(target) {
		dto.Configured = true
		dto.Bucket = target.Bucket
		dto.Region = target.Region
		dto.Endpoint = target.Endpoint
		dto.Prefix = target.Prefix
	}
	return json.NewEncoder(w).Encode(dto)
}

func newServerBackupTargetUnsetCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset",
		Short: "Remove the stored backup target",
		Long: "Removes the stored target (both credentials and settings). Scheduled\n" +
			"backup jobs will fail at run time until a new target is set — remove\n" +
			"their cron entries first if you are retiring backups.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerBackupTargetUnset(flags)
		},
	}
	return cmd
}

func runServerBackupTargetUnset(flags *Flags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()

	removed, err := backup.UnsetTarget(ctx, executor)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Println("No backup target configured")
		return nil
	}
	for _, p := range removed {
		fmt.Printf("Removed %s\n", p)
	}
	fmt.Println("Scheduled backup jobs will fail at run time until a new target is set.")
	recordBackupTargetEvent(ctx, executor, nil)
	return nil
}

// configResolveServerForTarget resolves the target server for the
// backup-target family: --host names (or overrides) the server.
func configResolveServerForTarget(flags *Flags) (host, user, key string, err error) {
	if flags.Host == "" {
		return "", "", "", fmt.Errorf("--host is required (the backup target is per server)")
	}
	return config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
}

// rewriteBackupCrons rewrites teploy-GENERATED backup cron lines (the
// pre-D3 forms, which embed the bucket — and for custom endpoints, the
// credentials) to the `backup run` form. Lines under a teploy marker whose
// command is not one of the generated shapes are hand-written and are left
// untouched (reported instead). The whole crontab is reinstalled over
// stdin, so unrelated lines — including their % characters — round-trip
// byte-for-byte.
func rewriteBackupCrons(ctx context.Context, executor ssh.Executor) (rewritten, kept []string, err error) {
	lines, err := backup.ReadCrontab(ctx, executor)
	if err != nil {
		return nil, nil, err
	}
	bin := scheduledTeployPath()
	supported := serverBinarySupportsBackupRun(ctx, executor, bin)
	changed := false
	var policies = map[string]*backup.KindPolicy{}
	for i, line := range lines {
		m := backup.FindCronMarker(line)
		if m == nil {
			continue
		}
		switch m.Form {
		case backup.CronFormRun:
			// Already the D3 form.
		case backup.CronFormCustom:
			kept = append(kept, m.Tag)
		case backup.CronFormLegacy:
			if !supported {
				kept = append(kept, m.Tag)
				fmt.Fprintf(os.Stderr, "Warning: %s embeds the old bucket/credentials, but %s does not support `backup run` yet — upgrade the server binary and re-run `teploy server backup-target set`\n", m.Tag, bin)
				continue
			}
			// Migrate any retention baked into the legacy line.
			if m.KeepLast > 0 || m.MaxAgeDays > 0 {
				key := m.App
				if m.Accessory != "" {
					key = m.App + "/" + m.Accessory
				}
				policies[key] = &backup.KindPolicy{KeepLast: m.KeepLast, MaxAgeDays: m.MaxAgeDays}
			}
			lines[i] = m.Cron + " " + buildScheduledBackupRunCmd(bin, m.App, m.Accessory) + " # [" + m.Tag + "]"
			rewritten = append(rewritten, m.Tag)
			changed = true
		}
	}
	if !changed {
		return rewritten, kept, nil
	}
	if err := executor.RunInput(ctx, "crontab -", strings.NewReader(strings.Join(lines, "\n")+"\n")); err != nil {
		return nil, nil, fmt.Errorf("installing the rewritten crontab: %w", err)
	}
	// Persist migrated retention (best effort — a failure to write the
	// policy keeps the lines rewritten; retention just stays as-was).
	for key, kp := range policies {
		app, acc, _ := strings.Cut(key, "/")
		policy, err := backup.ReadPolicy(ctx, executor, app)
		if err != nil {
			continue
		}
		if acc == "" {
			p := kp
			existing := policy.Volumes
			if existing != nil && existing.KeepLast == 0 && existing.MaxAgeDays == 0 {
				existing.KeepLast, existing.MaxAgeDays = p.KeepLast, p.MaxAgeDays
			} else if existing == nil {
				policy.Volumes = p
			}
		} else {
			if policy.Accessories == nil {
				policy.Accessories = map[string]*backup.KindPolicy{}
			}
			if policy.Accessories[acc] == nil {
				policy.Accessories[acc] = kp
			}
		}
		if err := backup.WritePolicy(ctx, executor, app, policy); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not persist migrated retention for %s: %v\n", key, err)
		}
	}
	return rewritten, kept, nil
}

// legacyKeepLastRE pulls the keep-last count out of a legacy generated
// volumes line (`... | head -n -N ...`).
var legacyKeepLastRE = regexp.MustCompile(`head -n -([0-9]+)`)

// recordBackupTargetEvent records backup_target.set for every app known
// on the server: the event log is per app, and the target is shared by
// all of them (detail carries no bucket or credential material).
func recordBackupTargetEvent(ctx context.Context, executor ssh.Executor, err error) {
	apps, appsErr := serverAppNames(ctx, executor)
	if appsErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not list apps for the backup_target.set event: %v\n", appsErr)
		return
	}
	for _, app := range apps {
		events.Record(ctx, executor, events.Event{App: app, Kind: events.KindBackupTarget, Outcome: events.Outcome(err)})
	}
}

// serverAppNames lists every app known on the server: .teploy apps with
// history/config plus every deployed state directory.
func serverAppNames(ctx context.Context, executor ssh.Executor) ([]string, error) {
	res := ssh.RunDetailed(ctx, executor, `for d in /deployments/.teploy/apps/*; do [ -d "$d" ] && basename -- "$d"; done; for f in /deployments/*/state.json; do [ -e "$f" ] && basename -- "$(dirname -- "$f")"; done`)
	if res.Err != nil {
		return nil, res.Err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%s", res.ExitErrorText())
	}
	seen := map[string]bool{}
	var apps []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if app := strings.TrimSpace(line); app != "" && !seen[app] {
			seen[app] = true
			apps = append(apps, app)
		}
	}
	sort.Strings(apps)
	return apps, nil
}
