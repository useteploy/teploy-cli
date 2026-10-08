package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// Owns only containers it created. Legal incumbent accessory names stay in
// the same world and any attempt to remove them is a fixture failure.
type round3DRExecutor struct {
	*ssh.MockExecutor
	mu       sync.Mutex
	commands []string
	owners   map[string]string
}

func (e *round3DRExecutor) Run(ctx context.Context, command string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commands = append(e.commands, command)
	if strings.HasPrefix(command, "docker create ") {
		owner := regexp.MustCompile(`teploy.dr-owner=([0-9a-f]{32})`).FindStringSubmatch(command)
		if len(owner) != 2 {
			return "", errors.New("scratch lacks owner")
		}
		id := fmt.Sprintf("%064x", len(e.owners)+1)
		e.owners[id] = owner[1]
		return id, nil
	}
	if strings.HasPrefix(command, "docker inspect -f '{{index .Config.Labels") {
		for id, owner := range e.owners {
			if strings.HasSuffix(command, "'"+id+"'") {
				return owner, nil
			}
		}
		return "", errors.New("unknown scratch ID")
	}
	if strings.HasPrefix(command, "docker rm -f ") {
		for id := range e.owners {
			if strings.HasSuffix(command, "'"+id+"'") {
				return "", nil
			}
		}
		return "", fmt.Errorf("attempted incumbent/unknown cleanup: %s", command)
	}
	if strings.Contains(command, "SELECT COUNT(*)") {
		return "3", nil
	}
	return e.MockExecutor.Run(ctx, command)
}
func TestRound3DRNativeMariaDBAndPasswordStdinAtCutover(t *testing.T) {
	manifest := drTestManifest()
	manifest.Snapshots = manifest.Snapshots[:1]
	manifest.Snapshots[0].Engine = "mariadb"
	manifest.Snapshots[0].Method = "mariadb-dump"
	manifest.Snapshots[0].Image = "mariadb:11"
	cfg := drTestConfig()
	cfg.Volumes = nil
	cfg.Accessories = map[string]config.AccessoryConfig{"db": {Image: "mariadb:11", EnvLiteral: map[string]string{"MARIADB_ROOT_PASSWORD": "DUMMY-r3-secret", "MARIADB_DATABASE": "appdb"}, Volumes: map[string]string{"data": "/var/lib/mysql"}}}
	mock := drCutoverMock(drOKReceipt(), manifest)
	receipt, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(), BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: cfg})
	if err != nil || !receipt.OK {
		t.Fatalf("cutover: %v receipt=%+v calls=%v", err, receipt, mock.Calls)
	}
	nativeAdmin, nativeImport := false, false
	for _, command := range mock.Calls {
		if strings.Contains(command, "DUMMY-r3-secret") {
			t.Fatalf("password in argv: %s", command)
		}
		if strings.Contains(command, "exec mariadb-admin ") {
			nativeAdmin = true
		}
		if strings.Contains(command, "exec mariadb -u root") {
			nativeImport = true
		}
		if strings.Contains(command, "exec mysql ") || strings.Contains(command, "mysqladmin ping") {
			t.Fatal("MySQL alias required")
		}
	}
	if !nativeAdmin || !nativeImport || !strings.Contains(strings.Join(mock.Inputs, ""), "MYSQL_PWD=DUMMY-r3-secret\n") {
		t.Fatalf("native/private transport missing: %v inputs=%v", mock.Calls, mock.Inputs)
	}
}
func TestRound3DRValidationScratchNeverDeletesIncumbents(t *testing.T) {
	mock := drRestoreMock(drTestManifest())
	executor := &round3DRExecutor{MockExecutor: mock, owners: map[string]string{}}
	client := NewClient(executor, &bytes.Buffer{})
	manifest := drTestManifest()
	manifest.App = "demo"
	manifest.AppRun.Image = "demo:v1"
	manifest.State = json.RawMessage(`{"schema_version":2,"current_hash":"v1","image_ref":"demo:v1"}`)
	snap := manifest.Snapshots[0]
	snap.Engine = "mariadb"
	snap.Image = "mariadb:11"
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := client.validateEngineSnapshot(context.Background(), manifest, snap, "/var/tmp/private-r3")
			if result.Status != "pass" {
				t.Errorf("validation: %+v", result)
			}
		}()
	}
	wg.Wait()
	result := client.appCheck(context.Background(), manifest, BundleRestoreOptions{}, "/var/tmp/private-r3")
	if result.Status != "pass" {
		t.Fatal(result)
	}
	for _, command := range executor.commands {
		if strings.Contains(command, "demo-db-drcheck") || strings.Contains(command, "demo-dr-appcheck") {
			t.Fatalf("incumbent touched: %s", command)
		}
	}
	if len(executor.owners) != 3 {
		t.Fatalf("overlapping validators reused scratch: %+v", executor.owners)
	}
}
func TestRound3RestoreWorkspaceNeverWipesPriorInvocation(t *testing.T) {
	manifest := drTestManifest()
	mock := drRestoreMock(manifest)
	// Engine/app checks use owned IDs; both full restores retain distinct trees.
	executor := &round3DRExecutor{MockExecutor: mock, owners: map[string]string{}}
	client := NewClient(executor, &bytes.Buffer{})
	one, err := client.RestoreBundleIsolated(context.Background(), BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := client.RestoreBundleIsolated(context.Background(), BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatal(err)
	}
	if one.InvocationID == two.InvocationID || one.StagingPath == two.StagingPath {
		t.Fatal("workspace reused")
	}
	for _, command := range executor.commands {
		if strings.HasPrefix(command, "rm -rf") {
			t.Fatalf("prior invocation wiped: %s", command)
		}
	}
}
