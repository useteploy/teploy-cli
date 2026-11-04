package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// The env/label/entrypoint fixtures below are the real image configs
// (registry config blobs read 2026-11-03).
func TestDetectEngine(t *testing.T) {
	pg := map[string]string{"PG_MAJOR": "17", "PG_VERSION": "17.11-1.pgdg13+2", "PGDATA": "/var/lib/postgresql/data"}
	for _, tc := range []struct {
		name       string
		image      string
		env        map[string]string
		labels     map[string]string
		argv       []string
		engine     string
		source     string
		wantErrSub string
	}{
		{name: "stock postgres", image: "postgres:17", env: pg, engine: EnginePostgres, source: "env"},
		{name: "pgvector", image: "pgvector/pgvector:pg17", env: pg, engine: EnginePostgres, source: "env"},
		{name: "timescale", image: "timescale/timescaledb:latest-pg17", env: map[string]string{"PG_MAJOR": "17", "PG_VERSION": "17.11"}, engine: EnginePostgres, source: "env"},
		{name: "postgis", image: "postgis/postgis:17-3.5", env: map[string]string{"PG_MAJOR": "17", "POSTGIS_MAJOR": "3"}, engine: EnginePostgres, source: "env"},
		{name: "bitnami postgresql", image: "bitnami/postgresql:latest", env: map[string]string{"BITNAMI_APP_NAME": "postgresql", "APP_VERSION": "18.6.0"}, engine: EnginePostgres, source: "env"},
		{name: "custom build FROM postgres", image: "registry.example:5000/acme/db:3", env: pg, engine: EnginePostgres, source: "env"},
		{name: "mysql", image: "mysql:8", env: map[string]string{"MYSQL_MAJOR": "8.4", "MYSQL_VERSION": "8.4.13-1.el9"}, engine: EngineMySQL, source: "env"},
		{name: "mariadb", image: "mariadb:11", env: map[string]string{"MARIADB_VERSION": "1:11.8.9+maria~ubu2404"}, engine: EngineMariaDB, source: "env"},
		{name: "bitnami mysql", image: "bitnami/mysql", env: map[string]string{"BITNAMI_APP_NAME": "mysql"}, engine: EngineMySQL, source: "env"},
		{name: "redis", image: "redis:7", env: map[string]string{"REDIS_VERSION": "7.4.12"}, engine: EngineRedis, source: "env"},
		{name: "valkey", image: "valkey/valkey:8", env: map[string]string{"VALKEY_VERSION": "8.1.11"}, engine: EngineValkey, source: "env"},
		{name: "bitnami valkey", image: "bitnami/valkey", env: map[string]string{"BITNAMI_APP_NAME": "valkey"}, engine: EngineValkey, source: "env"},
		{name: "mongo", image: "mongo:7", env: map[string]string{"MONGO_MAJOR": "7.0", "MONGO_VERSION": "7.0.45"}, engine: EngineMongo, source: "env"},
		{name: "dragonfly by OCI label", image: "docker.dragonflydb.io/dragonflydb/dragonfly:latest",
			labels: map[string]string{"org.opencontainers.image.source": "https://github.com/dragonflydb/dragonfly"},
			argv:   []string{"/usr/bin/tini", "--", "entrypoint.sh", "--proactor_threads=1"}, engine: EngineDragonfly, source: "image-label"},
		{name: "dragonfly by entrypoint", image: "mirror.local/kv:1", argv: []string{"/usr/bin/tini", "--", "entrypoint.sh", "dragonfly", "--logtostderr"}, engine: EngineDragonfly, source: "entrypoint"},
		{name: "nucleus by entrypoint", image: "ghcr.io/neutron-build/nucleus:v1.3.0", argv: []string{"nucleus", "start", "--data", "/data"}, engine: EngineNucleus, source: "entrypoint"},
		{name: "image name fallback", image: "registry.example:5000/postgres:16", engine: EnginePostgres, source: "image-name"},
		{name: "timescale name fallback", image: "timescale/timescaledb-ha:pg17", engine: EnginePostgres, source: "image-name"},
		{name: "label overrides env", image: "postgres:17", env: pg, labels: map[string]string{EngineLabel: "generic"}, engine: EngineGeneric, source: "label"},
		{name: "label names engine", image: "acme/thing:1", labels: map[string]string{EngineLabel: " Postgres "}, engine: EnginePostgres, source: "label"},
		{name: "unknown label refused", image: "postgres:17", labels: map[string]string{EngineLabel: "oracle"}, wantErrSub: "not a known engine"},
		{name: "plain files", image: "getmeili/meilisearch:v1", env: map[string]string{"MEILI_ENV": "production"}, engine: EngineGeneric, source: "none"},
		{name: "credentials alone are not markers", image: "acme/app:1", env: map[string]string{"MYSQL_ROOT_PASSWORD": "x", "POSTGRES_PASSWORD": "y"}, engine: EngineGeneric, source: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectEngine(tc.image, tc.env, tc.labels, tc.argv)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("want error containing %q, got %v (%+v)", tc.wantErrSub, err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Engine != tc.engine || got.Source != tc.source {
				t.Fatalf("DetectEngine = %+v, want %s from %s", got, tc.engine, tc.source)
			}
		})
	}
}

func TestResolveEngineReadsContainerHints(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect -f '{{json .Config.Labels}}", Output: `{"teploy.engine":"valkey"}` + "\nnull\n" + `["valkey-server"]` + "\n"},
	)
	det, err := NewClient(mock, io.Discard).ResolveEngine(context.Background(), "app", "cache", "acme/kv:1", nil)
	if err != nil || det.Engine != EngineValkey || det.Source != "label" {
		t.Fatalf("ResolveEngine = %+v, %v", det, err)
	}
	// An absent container still classifies from image and env.
	det, err = NewClient(ssh.NewMockExecutor("h"), io.Discard).ResolveEngine(context.Background(), "app", "db", "pgvector/pgvector:pg17", map[string]string{"PG_MAJOR": "17"})
	if err != nil || det.Engine != EnginePostgres {
		t.Fatalf("ResolveEngine without container = %+v, %v", det, err)
	}
}

// G6: no detected database ever takes the raw-tar branch; each has an
// engine-native method, and the planner refuses engines it cannot back up.
func TestPlanAccessoryDump_DatabasesNeverTar(t *testing.T) {
	for _, eng := range []string{EnginePostgres, EngineMySQL, EngineMariaDB, EngineRedis, EngineValkey, EngineDragonfly, EngineMongo} {
		plan, err := planAccessoryDump("app", "db", EngineDetection{Engine: eng, Source: "env"}, nil, "/tmp/w")
		if err != nil {
			t.Fatalf("%s: %v", eng, err)
		}
		if plan.method == "tar" || plan.ext == ".tar.gz" || strings.Contains(plan.cmd, "tar -czf") {
			t.Errorf("%s planned a raw tar: %+v", eng, plan)
		}
		if plan.consistency == consistencyCrash {
			t.Errorf("%s claims only crash consistency", eng)
		}
		if plan.minBytes <= 23 {
			t.Errorf("%s floor %d would accept a gzip of nothing (23 bytes)", eng, plan.minBytes)
		}
	}
	if _, err := planAccessoryDump("app", "db", EngineDetection{Engine: "cockroach"}, nil, "/tmp/w"); err == nil {
		t.Fatal("an engine without a method must be refused, not tarred")
	}
	plan, err := planAccessoryDump("app", "db", EngineDetection{Engine: EngineNucleus, Source: "entrypoint"}, nil, "/tmp/w")
	if err != nil || plan.engine != EngineNucleus || plan.method != "tar" || plan.consistency != consistencyCrash {
		t.Fatalf("nucleus keeps its crash-consistent tar, labelled as nucleus: %+v %v", plan, err)
	}
}

func TestPlanAccessoryDump_EngineCommands(t *testing.T) {
	plan := func(eng string, env map[string]string) accessoryDumpPlan {
		t.Helper()
		p, err := planAccessoryDump("app", "db", EngineDetection{Engine: eng}, env, "/tmp/w")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pg := plan(EnginePostgres, map[string]string{"POSTGRES_DB": "appdb", "POSTGRES_USER": "u"})
	if !strings.Contains(pg.cmd, "pg_dump -Fc -U 'u' 'appdb' > '/tmp/w/dump.pgdump'") || !strings.Contains(pg.cmd, "= PGDMP") || pg.ext != ".pgdump" || strings.Contains(pg.cmd, " | ") {
		t.Errorf("postgres must dump custom format into a file and check its magic: %s", pg.cmd)
	}
	my := plan(EngineMySQL, map[string]string{"MYSQL_ROOT_PASSWORD": "pw"})
	if !strings.Contains(my.cmd, "mysqldump --single-transaction --routines --triggers -u root") || !strings.Contains(my.cmd, "Dump completed") {
		t.Errorf("mysql dump must be consistent, carry routines/triggers and check its trailer: %s", my.cmd)
	}
	if strings.Contains(my.cmd, "pw") {
		t.Errorf("password in the command text: %s", my.cmd)
	}
	if md := plan(EngineMariaDB, nil); !strings.Contains(md.cmd, "mariadb-dump --single-transaction --routines --triggers") {
		t.Errorf("mariadb uses mariadb-dump with the same flags: %s", md.cmd)
	}
	if vk := plan(EngineValkey, nil); !strings.Contains(vk.cmd, "valkey-cli bgsave") || strings.Contains(vk.cmd, "redis-cli") || !strings.Contains(vk.cmd, "REDIS|VALKE") {
		t.Errorf("valkey uses valkey-cli and checks the RDB magic: %s", vk.cmd)
	}
	df := plan(EngineDragonfly, nil)
	if !strings.Contains(df.cmd, "redis-cli save rdb teploy-snapshot-") || strings.Contains(df.cmd, "bgsave") || strings.Contains(df.cmd, "/data/dump.rdb") {
		t.Errorf("dragonfly must SAVE RDB (its BGSAVE writes .dfs, never dump.rdb): %s", df.cmd)
	}
}

// A detected pgvector accessory is dumped with pg_dump even though its
// image is not called "postgres" — the U3 false green.
func TestAccessoryBackup_PgvectorIsDumpedNotTarred(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' pg_dump -Fc", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "2123\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	res, err := NewClient(mock, io.Discard).AccessoryBackup(context.Background(), "myapp", "db", "pgvector/pgvector:pg17",
		map[string]string{"PG_MAJOR": "17", "POSTGRES_DB": "appdb"}, S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != EnginePostgres || res.Consistency != consistencyEngineDump || res.SizeBytes != 2123 || !strings.HasSuffix(res.Key, ".pgdump") {
		t.Fatalf("unexpected result %+v", res)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "tar -czf") {
			t.Fatalf("a postgres-family accessory was tarred: %s", c)
		}
	}
}

// The deploy-ovh incident: a dump that "succeeds" with an implausibly small
// artifact (gzip of an empty stream) must fail and never be uploaded.
func TestAccessoryBackup_RefusesImplausiblySmallArtifact(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "23\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	var buf bytes.Buffer
	_, err := NewClient(mock, &buf).AccessoryBackup(context.Background(), "myapp", "db", "mysql:8",
		map[string]string{"MYSQL_VERSION": "8.4"}, S3Config{Bucket: "b", Region: "us-east-1"})
	if err == nil || !strings.Contains(err.Error(), "implausibly small (23 bytes") {
		t.Fatalf("expected an implausibly-small refusal, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "aws s3 cp") {
			t.Fatalf("a refused artifact was uploaded: %s", c)
		}
	}
}

// pipefail must fail when EITHER side fails (POSIX sh, no set -o pipefail).
func TestPipefail(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "m")
	for _, tc := range []struct {
		producer, consumer string
		ok                 bool
	}{
		{"printf data", "cat >/dev/null", true},
		{"false", "cat >/dev/null", false},
		{"printf data", "false", false},
		{"cat " + filepath.Join(dir, "missing"), "cat >/dev/null", false},
	} {
		err := exec.Command("sh", "-c", pipefail(tc.producer, tc.consumer, marker)).Run()
		if (err == nil) != tc.ok {
			t.Errorf("pipefail(%q | %q): err=%v, want ok=%v", tc.producer, tc.consumer, err, tc.ok)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		// the last case left its marker; a following success must clear it
		if err := exec.Command("sh", "-c", pipefail("true", "cat >/dev/null", marker)).Run(); err != nil {
			t.Errorf("a stale marker from an earlier run failed a good pipe: %v", err)
		}
	}
}

// The dragonfly restore saves the current dataset BEFORE loading, loads
// with DFLY LOAD, reloads the pre-restore snapshot on failure, and
// persists the restored dataset.
func TestDragonflyLoadScriptOrder(t *testing.T) {
	s := dragonflyLoadScript("app-cache", "/tmp/r/restore.rdb.gz", "/tmp/r", "/var/tmp/pre/pre-restore.rdb")
	pre := strings.Index(s, "save rdb teploy-pre-restore-")
	load := strings.Index(s, "dfly load teploy-restore-")
	back := strings.Index(s, "dfly load teploy-pre-restore-")
	persist := strings.LastIndex(s, "redis-cli save 2>&1")
	if pre < 0 || load < 0 || back < 0 || persist < 0 || !(pre < load && load < back && back < persist) {
		t.Fatalf("unexpected order pre=%d load=%d back=%d persist=%d:\n%s", pre, load, back, persist, s)
	}
	if !strings.Contains(s, "'/var/tmp/pre/pre-restore.rdb'") {
		t.Error("the pre-restore snapshot must be copied out to the recovery path")
	}
}

// verify-backup classifies a timescale accessory as postgres (restored and
// queried), never as a volume archive that passes on files>0.
func TestVerifyBackup_TimescaleIsVerifiedAsPostgres(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "umask 077; mktemp -d", Output: "/var/tmp/teploy-verify.test\n"},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "timescale/timescaledb:latest-pg17\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "PG_MAJOR=17\nPOSTGRES_PASSWORD=pw\nPOSTGRES_DB=appdb\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-11-03 04:00:02  14421 20261103-040000-0123456789abcdef.pgdump\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "docker run -d", Output: ""},
		ssh.MockCommand{Match: "for i in $(seq", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'teploy-backup-verify-", Output: "2; 3079 17060 EXTENSION - timescaledb \n"},
		ssh.MockCommand{Match: "docker exec", Output: "170004\n"},
		ssh.MockCommand{Match: "if [", Output: ""},
	)
	res, err := NewClient(mock, io.Discard).VerifyBackup(context.Background(), "myapp", "db", "", S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "postgres" || res.Engine != EnginePostgres || res.Detection != "env" || !strings.HasSuffix(res.S3Key, ".pgdump") {
		t.Fatalf("unexpected classification %+v", res)
	}
	var pre, post bool
	for _, c := range mock.Calls {
		pre = pre || strings.Contains(c, "timescaledb_pre_restore()")
		post = post || strings.Contains(c, "timescaledb_post_restore()")
		if strings.Contains(c, "find ") && strings.Contains(c, "wc -l") {
			t.Fatalf("verified as a volume archive: %s", c)
		}
	}
	if !pre || !post {
		t.Fatalf("a timescale archive must be restored between pre_restore/post_restore: %v", mock.Calls)
	}
}
