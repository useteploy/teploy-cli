package docker

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// Ancestors must be owned by root or the SSH deployment administrator and
// not group/world writable. The SSH administrator has passwordless sudo and
// is trusted as root; its own processes and root can mutate ancestors. Root and sudo
// administrators are trusted; an app lease cannot fence unrelated root actors.
// All lookups are descriptor-relative O_NOFOLLOW, and fchown/fchmod use the
// exact directory fd admitted. Requires existing Python 3 on the Linux host.
const managedDirectoryPython = `import os, stat, sys
parts = sys.argv[1].strip('/').split('/')
operation = sys.argv[2]
uid, gid, mode = int(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5], 8)
actor = int(sys.argv[6]) if len(sys.argv) > 6 else os.getuid()
flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
fd = os.open('/', flags)
try:
 for index, part in enumerate(parts):
  if part in ('', '.', '..'): raise RuntimeError('invalid managed path')
  parent = os.fstat(fd)
  if parent.st_uid not in (0, actor) or parent.st_mode & 0o022:
   raise RuntimeError('unsafe mutable volume ancestor; requires trusted administrator ownership and no group/world write')
  final = index == len(parts)-1
  try:
   child = os.open(part, flags, dir_fd=fd)
  except FileNotFoundError:
   if operation == 'validate': sys.exit(0)
   os.mkdir(part, 0o700 if final and uid >= 0 else 0o755, dir_fd=fd)
   if not (final and uid >= 0): os.chown(part, actor, -1, dir_fd=fd, follow_symlinks=False)
   child = os.open(part, flags, dir_fd=fd)
  os.close(fd)
  fd = child
 if uid >= 0:
  current = os.fstat(fd)
  matches = (current.st_uid, current.st_gid, stat.S_IMODE(current.st_mode)) == (uid, gid, mode)
  if not matches:
   if os.listdir(fd): raise RuntimeError('populated volume ownership mismatch; explicit operator migration required')
   if operation != 'validate':
    os.fchown(fd, uid, gid)
    os.fchmod(fd, mode)
finally:
 os.close(fd)
`

func (c *Client) managedDirectory(ctx context.Context, dir, operation string, permission *config.VolumeOwnership, prefix string) error {
	uid, gid, mode := "-1", "-1", "0755"
	if permission != nil {
		if err := permission.Validate(); err != nil {
			return err
		}
		uid, gid, mode = strconv.FormatUint(uint64(*permission.UID), 10), strconv.FormatUint(uint64(*permission.GID), 10), permission.Mode
	}
	command := "python3 -c " + ssh.ShellQuote(managedDirectoryPython) + " " + ssh.ShellQuote(dir) + " " + operation + " " + uid + " " + gid + " " + mode
	_, err := c.exec.Run(ctx, prefix+"teploy_volume_actor=$(id -u); if [ \"$(id -u)\" = 0 ]; then "+command+" \"$teploy_volume_actor\"; else sudo -n "+command+" \"$teploy_volume_actor\"; fi")
	if err != nil {
		return fmt.Errorf("admitting managed directory %s (requires Python 3 and root/passwordless sudo): %w", dir, err)
	}
	return nil
}
func managedVolumePath(app, accessory, name string) (string, error) {
	for _, value := range []string{app, name} {
		if err := config.ValidateName(value); err != nil {
			return "", err
		}
	}
	root := path.Join("/deployments", app, "volumes")
	if accessory != "" {
		if err := config.ValidateName(accessory); err != nil {
			return "", err
		}
		root = path.Join("/deployments", app, "accessories", accessory)
	}
	return path.Join(root, name), nil
}
func (c *Client) ValidateManagedVolume(ctx context.Context, app, accessory, name string, permission config.VolumeOwnership, prefix ...string) error {
	dir, err := managedVolumePath(app, accessory, name)
	if err != nil {
		return err
	}
	return c.managedDirectory(ctx, dir, "validate", &permission, firstPrefix(prefix))
}
func (c *Client) ProvisionManagedVolume(ctx context.Context, app, accessory, name string, permission config.VolumeOwnership, prefix ...string) error {
	dir, err := managedVolumePath(app, accessory, name)
	if err != nil {
		return err
	}
	return c.managedDirectory(ctx, dir, "provision", &permission, firstPrefix(prefix))
}

// EnsureManagedDirectory is also used for legacy volumes without ownership
// metadata and accessory control directories. It never traverses a symlink.
func (c *Client) EnsureManagedDirectory(ctx context.Context, app, accessory, name string, prefix ...string) error {
	if err := config.ValidateName(app); err != nil {
		return err
	}
	dir := path.Join("/deployments", app)
	if accessory != "" {
		if err := config.ValidateName(accessory); err != nil {
			return err
		}
		dir = path.Join(dir, "accessories", accessory)
	} else if name != "" {
		dir = path.Join(dir, "volumes")
	}
	if name != "" {
		if err := config.ValidateName(name); err != nil {
			return err
		}
		dir = path.Join(dir, name)
	}
	return c.managedDirectory(ctx, dir, "provision", nil, firstPrefix(prefix))
}
func firstPrefix(prefix []string) string {
	if len(prefix) > 0 {
		return prefix[0]
	}
	return ""
}

// Retained for command fixtures; production uses the same descriptor helper.
func managedVolumeOwnershipScript(root, name string, permission config.VolumeOwnership) string {
	return "python3 -c " + ssh.ShellQuote(managedDirectoryPython) + " " + ssh.ShellQuote(path.Join(root, name)) + " provision " + fmt.Sprintf("%d %d %s", *permission.UID, *permission.GID, permission.Mode)
}
