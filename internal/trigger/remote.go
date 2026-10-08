package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// RemoteStore uses only the admitted executor; production passes a
// state.FencedExecutor so all journal changes use the application's lease.
type RemoteStore struct {
	Exec ssh.Executor
	App  string
}

func (s RemoteStore) Load(ctx context.Context) ([]Receipt, error) {
	if err := config.ValidateName(s.App); err != nil {
		return nil, err
	}
	output, err := s.Exec.Run(ctx, "python3 -c "+ssh.ShellQuote(loadReceipts)+" "+ssh.ShellQuote("/deployments/"+s.App))
	if err != nil {
		return nil, err
	}
	if len(output) > 8<<20 {
		return nil, fmt.Errorf("journal inventory exceeds bound; debt retained")
	}
	var receipts []Receipt
	d := json.NewDecoder(strings.NewReader(output))
	d.DisallowUnknownFields()
	if err = d.Decode(&receipts); err != nil {
		return nil, err
	}
	for _, r := range receipts {
		if err = ValidateReceipt(r); err != nil {
			return nil, err
		}
	}
	return receipts, nil
}
func (s RemoteStore) Save(ctx context.Context, r Receipt) error {
	if err := config.ValidateName(s.App); err != nil {
		return err
	}
	data, err := EncodeReceipt(r)
	if err != nil {
		return err
	}
	return s.Exec.RunInput(ctx, "python3 -c "+ssh.ShellQuote(saveReceipt)+" "+ssh.ShellQuote("/deployments/"+s.App)+" "+ssh.ShellQuote(r.Request.OperationKey+".json"), bytes.NewReader(data))
}

// EnrolledTarget proves identity on the target. Atomic link rather than a
// caller-supplied ID or an alias makes parallel first enrollment safe. The
// enrollment file is permanent; replacing a server without its enrollment
// necessarily creates a different target identity.
func EnrolledTarget(ctx context.Context, exec ssh.Executor, app string) (string, error) {
	if err := config.ValidateName(app); err != nil {
		return "", err
	}
	out, err := exec.Run(ctx, "python3 -c "+ssh.ShellQuote(enrollTarget))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out) + "/" + app
	if !targetID.MatchString(id) {
		return "", fmt.Errorf("invalid target enrollment")
	}
	return id, nil
}

const directoryCore = `import os,sys,json,stat,tempfile,secrets
flags=os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW
root=os.open('/deployments',flags)
s=os.fstat(root)
if s.st_uid not in (0,os.geteuid()) or s.st_mode & 0o022: raise RuntimeError('unsafe deployments authority')
`
const enrollTarget = directoryCore + `
try:
 fd=os.open('.teploy-enrollment-id',os.O_RDONLY|os.O_NOFOLLOW,dir_fd=root)
except FileNotFoundError:
 name='.enrollment-'+secrets.token_hex(16)
 fd=os.open(name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=root)
 try:
  os.write(fd,secrets.token_hex(16).encode());os.fsync(fd)
 finally: os.close(fd)
 try:
  os.link(name,'.teploy-enrollment-id',src_dir_fd=root,dst_dir_fd=root,follow_symlinks=False);os.fsync(root)
 except FileExistsError: pass
 finally: os.unlink(name,dir_fd=root)
 fd=os.open('.teploy-enrollment-id',os.O_RDONLY|os.O_NOFOLLOW,dir_fd=root)
s=os.fstat(fd)
if not stat.S_ISREG(s.st_mode) or s.st_mode & 0o077 or s.st_uid not in (0,os.geteuid()): raise RuntimeError('unsafe enrollment')
print(os.read(fd,64).decode())
`
const journalCore = directoryCore + `
app=os.open(sys.argv[1].split('/')[-1],flags,dir_fd=root)
s=os.fstat(app)
if s.st_uid not in (0,os.geteuid()) or s.st_mode & 0o022: raise RuntimeError('unsafe app authority')
try: os.mkdir('.operations',0o700,dir_fd=app);os.fsync(app)
except FileExistsError: pass
journal=os.open('.operations',flags,dir_fd=app)
s=os.fstat(journal)
if s.st_uid != os.geteuid() or s.st_mode & 0o077: raise RuntimeError('unsafe private journal')
`
const loadReceipts = journalCore + `
rows=[];size=0
for name in sorted(os.listdir(journal)):
 if not name.endswith('.json'): continue
 if len(rows)>=10000: raise RuntimeError('journal requires archival without deleting debt')
 fd=os.open(name,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=journal)
 try:
  s=os.fstat(fd)
  if not stat.S_ISREG(s.st_mode) or s.st_mode & 0o077 or s.st_uid!=os.geteuid(): raise RuntimeError('unsafe receipt')
  if s.st_size>65536: raise RuntimeError('oversized receipt')
  data=os.read(fd,65537);size+=len(data)
  if size>8388608: raise RuntimeError('journal exceeds bound; retain debt')
  row=json.loads(data)
  if name!=row['request']['operation_key']+'.json': raise RuntimeError('receipt filename mismatch')
  rows.append(row)
 finally: os.close(fd)
print(json.dumps(rows,separators=(',',':')))
`
const saveReceipt = journalCore + `
data=sys.stdin.buffer.read(65537)
if len(data)>65536: raise RuntimeError('receipt too large')
row=json.loads(data);name=sys.argv[2]
if name!=row['request']['operation_key']+'.json' or len(name)!=69 or any(c not in '0123456789abcdef' for c in name[:-5]): raise RuntimeError('bad receipt name')
try:
 fd=os.open(name,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=journal)
 try: old=json.loads(os.read(fd,65537))
 finally: os.close(fd)
 # The optional extra timestamp is an admission check, not domain identity.
 a=dict(old['request']);b=dict(row['request']);a.pop('expected_preview_updated_at',None);b.pop('expected_preview_updated_at',None)
 if a!=b or old.get('evidence')!=row.get('evidence'): raise RuntimeError('immutable receipt identity conflict')
 if old['completed'] and old!=row: raise RuntimeError('completed receipt is immutable')
except FileNotFoundError: pass
candidate='.pending-'+secrets.token_hex(16)
fd=os.open(candidate,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=journal)
try:
 with os.fdopen(fd,'wb') as f: f.write(data);f.flush();os.fsync(f.fileno())
 os.replace(candidate,name,src_dir_fd=journal,dst_dir_fd=journal);os.fsync(journal)
finally:
 try: os.unlink(candidate,dir_fd=journal)
 except FileNotFoundError: pass
`
