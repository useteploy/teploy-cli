package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/ssh"
)

// backupHeadDTO is `backup head --json` (backup-setup-v1). Data is the object's first
// bytes exactly as stored: ciphertext for an encrypted backup. Nothing here decrypts.
type backupHeadDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Host             string `json:"host"`
	Key              string `json:"key"`
	Encrypted        bool   `json:"encrypted"`
	RequestedBytes   int    `json:"requested_bytes"`
	Length           int    `json:"length"`
	// TotalSize is null when the store's reply did not say.
	TotalSize  *int64 `json:"total_size"`
	DataBase64 string `json:"data_base64"`
}

func newBackupHeadDTO(host string, requested int, o backup.ObjectStart) backupHeadDTO {
	return backupHeadDTO{
		MachineInterface: MachineInterface, Host: host, Key: o.Key, Encrypted: o.Encrypted, RequestedBytes: requested,
		Length: len(o.Data), TotalSize: o.TotalSize, DataBase64: base64.StdEncoding.EncodeToString(o.Data),
	}
}

func newBackupHeadCmd(flags *Flags) *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "head <key>",
		Short: "Read the first bytes of a backup object through the server's stored target",
		Long: "A ranged read of one object, as stored, printed as base64: it never decrypts.\n" +
			"Used to prove a recovery key somewhere the key is, and the server's credentials\n" +
			"are not. The key is the s3:// URI from `teploy backup list` or `backup status`,\n" +
			"and must be under this server's prefix or a previous prefix. --bytes is at most\n" +
			"262144 (256 KiB). --host names the server.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			if n < 1 || n > backup.MaxObjectStartBytes {
				return fmt.Errorf("--bytes must be between 1 and %d (256 KiB)", backup.MaxObjectStartBytes)
			}
			host, user, key, err := configResolveServerForTarget(flags)
			if err != nil {
				return err
			}
			executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer executor.Close()
			rctx, rcancel := context.WithTimeout(ctx, 2*time.Minute)
			defer rcancel()
			dto, err := readBackupHead(rctx, executor, args[0], n)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(dto)
			}
			fmt.Println(dto.DataBase64)
			return nil
		},
	}
	cmd.Flags().IntVar(&n, "bytes", 0, "how many bytes to read from the start of the object (at most 262144)")
	return cmd
}

func readBackupHead(ctx context.Context, exec ssh.Executor, key string, n int) (backupHeadDTO, error) {
	target, err := backup.ReadTarget(ctx, exec)
	if err != nil {
		return backupHeadDTO{}, err
	}
	if !TargetConfigured(target) {
		return backupHeadDTO{}, fmt.Errorf("no backup target is configured for this server, so no key belongs to it")
	}
	o, err := backup.NewClient(exec, io.Discard).ReadObjectStart(ctx, TargetS3(target), key, n)
	if err != nil {
		return backupHeadDTO{}, err
	}
	return newBackupHeadDTO(exec.Host(), n, o), nil
}
