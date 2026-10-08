package state

import (
	"context"
	"fmt"
	"io"

	"github.com/useteploy/teploy/internal/ssh"
)

// WriteAtomicPrivate fsyncs a private sibling then atomically replaces the
// authority and fsyncs its directory. The caller supplies a fenced executor.
// A reply failure after rename is deliberately not interpreted as rollback.
func WriteAtomicPrivate(ctx context.Context, exec ssh.Executor, path string, input io.Reader) error {
	return exec.RunInput(ctx, "python3 -c "+ssh.ShellQuote(atomicPrivateScript)+" "+ssh.ShellQuote(path), input)
}

const atomicPrivateScript = `import os,sys,secrets,stat
path=sys.argv[1]
parts=path.split('/')
if not path.startswith('/deployments/') or any(p in ('.','..') for p in parts): raise RuntimeError('invalid authority path')
fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
for part in parts[1:-1]:
 nextfd=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd);os.close(fd);fd=nextfd
 s=os.fstat(fd)
 if s.st_uid not in (0,os.geteuid()) or s.st_mode & 0o022: raise RuntimeError('unsafe authority directory')
name=parts[-1];tmp='.atomic-'+secrets.token_hex(16)
data=sys.stdin.buffer.read(1048577)
if len(data)>1048576: raise RuntimeError('authority exceeds bound')
candidate=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=fd)
try:
 with os.fdopen(candidate,'wb') as f: f.write(data);f.flush();os.fsync(f.fileno())
 os.replace(tmp,name,src_dir_fd=fd,dst_dir_fd=fd);os.fsync(fd)
finally:
 try: os.unlink(tmp,dir_fd=fd)
 except FileNotFoundError: pass
`

// RequireLease rejects accidental reuse of a journal under an unfenced
// executor. It is useful at lifecycle entrypoints which already own a lease.
func RequireLease(lock *Lock) error {
	if lock == nil || lock.Owner() == "" {
		return fmt.Errorf("durable operation requires an app lease")
	}
	return nil
}
