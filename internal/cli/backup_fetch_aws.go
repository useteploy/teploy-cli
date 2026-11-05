package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/useteploy/teploy/internal/backup"
)

// localAWSCopy downloads one object with this machine's aws CLI, the same
// transport the servers use. Credentials come from the environment and go to the
// child process only: TEPLOY_S3_* are the names the rest of the CLI reads.
func localAWSCopy(ctx context.Context, key, dest, region, endpoint string) error {
	if _, err := exec.LookPath("aws"); err != nil {
		return fmt.Errorf("the aws CLI is not on this machine — %s", backup.AWSCLIRemedy)
	}
	args := []string{"s3", "cp", key, dest, "--region", region, "--only-show-errors"}
	if endpoint != "" {
		args = append(args, "--endpoint-url", endpoint)
	}
	c := exec.CommandContext(ctx, "aws", args...)
	env := os.Environ()
	if v := os.Getenv("TEPLOY_S3_ACCESS_KEY"); v != "" {
		env = append(env, "AWS_ACCESS_KEY_ID="+v)
	}
	if v := os.Getenv("TEPLOY_S3_SECRET_KEY"); v != "" {
		env = append(env, "AWS_SECRET_ACCESS_KEY="+v)
	}
	c.Env = env
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("downloading %s: %w: %s", key, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
