package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// Engines an accessory can be classified as. Everything except
// EngineGeneric has an engine-aware snapshot path; EngineGeneric is a raw
// tar of the accessory directory and is only ever chosen for a container
// that no detector recognises as a database (G6).
const (
	EnginePostgres  = "postgres"
	EngineMySQL     = "mysql"
	EngineMariaDB   = "mariadb"
	EngineRedis     = "redis"
	EngineValkey    = "valkey"
	EngineDragonfly = "dragonfly"
	EngineMongo     = "mongo"
	EngineNucleus   = "nucleus"
	EngineGeneric   = "generic"
)

// EngineLabel is the container label that overrides detection
// (`teploy.engine=postgres`). Set it on images the detectors cannot
// recognise (a custom build that dropped the upstream env markers), or
// `generic` / `none` to assert that a container really is plain files. The
// accessory's `engine:` in teploy.yml sets it.
const EngineLabel = "teploy.engine"

var knownEngines = map[string]bool{
	EnginePostgres: true, EngineMySQL: true, EngineMariaDB: true, EngineRedis: true,
	EngineValkey: true, EngineDragonfly: true, EngineMongo: true, EngineNucleus: true,
	EngineGeneric: true,
}

// EngineDetection is an engine classification and the evidence it came
// from: "label", "env", "image-label", "entrypoint", "image-name" or
// "none" (generic, nothing matched).
type EngineDetection struct {
	Engine string `json:"engine"`
	Source string `json:"source"`
}

// IsDB reports whether the engine has a database-aware snapshot path.
func (d EngineDetection) IsDB() bool { return d.Engine != EngineGeneric && d.Engine != "" }

// DetectEngine classifies an accessory container. The image name alone is
// not enough: pgvector/pgvector, timescale/timescaledb, postgis/postgis and
// bitnami/postgresql are all postgres, and matching only a repository
// called "postgres" sent them through the raw-tar branch, which then
// verified as a "volume" (U3). The order is: explicit label, the version
// env the upstream images bake in, OCI image labels, the entrypoint
// binary, and only then the image name.
func DetectEngine(image string, env, labels map[string]string, entrypoint []string) (EngineDetection, error) {
	if v, ok := labels[EngineLabel]; ok {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "none" {
			v = EngineGeneric
		}
		if !knownEngines[v] {
			return EngineDetection{}, fmt.Errorf("container label %s=%q is not a known engine (postgres, mysql, mariadb, redis, valkey, dragonfly, mongo, nucleus, generic)", EngineLabel, v)
		}
		return EngineDetection{Engine: v, Source: "label"}, nil
	}
	if e := engineFromEnv(env); e != "" {
		// Images built FROM a database image inherit its version env even
		// when they run something else (postgres-backup-local, wal-g and
		// pgbackrest sidecars). When the container's command is known and
		// is not the engine's server, it is not that database.
		if bin := firstCommand(entrypoint, env); bin != "" && !serverBinaries[e][bin] {
			return EngineDetection{Engine: EngineGeneric, Source: "env-not-server"}, nil
		}
		return EngineDetection{Engine: e, Source: "env"}, nil
	}
	if strings.Contains(labels["org.opencontainers.image.source"], "dragonflydb/dragonfly") {
		return EngineDetection{Engine: EngineDragonfly, Source: "image-label"}, nil
	}
	if e := engineFromEntrypoint(entrypoint); e != "" {
		return EngineDetection{Engine: e, Source: "entrypoint"}, nil
	}
	if e := engineFromImageName(image); e != "" {
		return EngineDetection{Engine: e, Source: "image-name"}, nil
	}
	return EngineDetection{Engine: EngineGeneric, Source: "none"}, nil
}

// engineFromEnv reads the version variables the upstream images set in
// their Dockerfiles (and every image built FROM them inherits). Only
// image-defined markers are used — never credentials or database names a
// user may set on any container.
func engineFromEnv(env map[string]string) string {
	switch strings.ToLower(env["BITNAMI_APP_NAME"]) {
	case "postgresql", "postgresql-repmgr":
		return EnginePostgres
	case "mysql":
		return EngineMySQL
	case "mariadb", "mariadb-galera":
		return EngineMariaDB
	case "redis":
		return EngineRedis
	case "valkey":
		return EngineValkey
	case "mongodb":
		return EngineMongo
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if env[k] != "" {
				return true
			}
		}
		return false
	}
	switch {
	case has("PG_MAJOR", "PG_VERSION"):
		return EnginePostgres
	case has("MARIADB_VERSION", "MARIADB_MAJOR"):
		return EngineMariaDB
	case has("MYSQL_VERSION", "MYSQL_MAJOR"):
		return EngineMySQL
	case has("VALKEY_VERSION"):
		return EngineValkey
	case has("REDIS_VERSION"):
		return EngineRedis
	case has("MONGO_VERSION", "MONGO_MAJOR"):
		return EngineMongo
	}
	return ""
}

// serverBinaries are the commands that run each engine's server.
var serverBinaries = map[string]map[string]bool{
	EnginePostgres: {"postgres": true, "postmaster": true},
	EngineMySQL:    {"mysqld": true},
	EngineMariaDB:  {"mariadbd": true, "mysqld": true},
	EngineRedis:    {"redis-server": true, "redis-stack-server": true},
	EngineValkey:   {"valkey-server": true, "redis-server": true},
	EngineMongo:    {"mongod": true},
}

// firstCommand is the first non-wrapper token of Entrypoint+Cmd ("" when
// unknown or when it is only flags, which the upstream entrypoints prepend
// the server to). Bitnami images start their server through run.sh.
func firstCommand(argv []string, env map[string]string) string {
	for _, a := range argv {
		base := path.Base(a)
		if entrypointWrappers[base] || (env["BITNAMI_APP_NAME"] != "" && base == "run.sh") {
			continue
		}
		if strings.HasPrefix(a, "-") {
			return ""
		}
		if env["BITNAMI_APP_NAME"] != "" && strings.HasPrefix(a, "/opt/bitnami/") {
			return ""
		}
		return base
	}
	return ""
}

// entrypointWrappers are init shims that precede the real server binary.
var entrypointWrappers = map[string]bool{
	"tini": true, "--": true, "dumb-init": true, "gosu": true, "su-exec": true,
	"docker-entrypoint.sh": true, "entrypoint.sh": true,
}

// engineFromEntrypoint classifies by the first non-wrapper binary of
// Entrypoint+Cmd: Nucleus and Dragonfly images carry no version env.
func engineFromEntrypoint(argv []string) string {
	for _, a := range argv {
		base := path.Base(a)
		if entrypointWrappers[base] {
			continue
		}
		switch base {
		case "postgres":
			return EnginePostgres
		case "mysqld":
			return EngineMySQL
		case "mariadbd":
			return EngineMariaDB
		case "redis-server":
			return EngineRedis
		case "valkey-server":
			return EngineValkey
		case "dragonfly":
			return EngineDragonfly
		case "mongod":
			return EngineMongo
		case "nucleus":
			return EngineNucleus
		}
		return ""
	}
	return ""
}

// engineFromImageName is the last-resort match on the repository's final
// path component.
func engineFromImageName(image string) string {
	switch imageName(image) {
	case "postgres", "postgresql", "pgvector", "timescaledb", "timescaledb-ha", "postgis":
		return EnginePostgres
	case "mysql":
		return EngineMySQL
	case "mariadb":
		return EngineMariaDB
	case "redis", "redis-stack-server":
		return EngineRedis
	case "valkey":
		return EngineValkey
	case "dragonfly":
		return EngineDragonfly
	case "mongo", "mongodb":
		return EngineMongo
	case "nucleus":
		return EngineNucleus
	}
	return ""
}

// ResolveEngine classifies the accessory container <app>-<name>: the running container's
// labels and entrypoint are read best-effort (an absent container — the
// teploy.yml fallback — still classifies from image and env).
func (c *Client) ResolveEngine(ctx context.Context, app, name, image string, env map[string]string) (EngineDetection, error) {
	container := ContainerName(app, name)
	if e := c.engineOverride[container]; e != "" {
		return EngineDetection{Engine: e, Source: "policy"}, nil
	}
	labels, argv := c.inspectEngineHints(ctx, container)
	return DetectEngine(image, env, labels, argv)
}

// SetEngineOverride forces a container's engine (an adopted policy entry's
// engine:, which has no label to live on).
func (c *Client) SetEngineOverride(container, engine string) error {
	if engine == "" {
		return nil
	}
	if !knownEngines[engine] {
		return fmt.Errorf("unknown engine %q (postgres, mysql, mariadb, redis, valkey, dragonfly, mongo)", engine)
	}
	if c.engineOverride == nil {
		c.engineOverride = map[string]string{}
	}
	c.engineOverride[container] = engine
	return nil
}

// inspectEngineHints reads the container's labels and Entrypoint+Cmd.
func (c *Client) inspectEngineHints(ctx context.Context, container string) (map[string]string, []string) {
	out, err := c.exec.Run(ctx, fmt.Sprintf("docker inspect -f '{{json .Config.Labels}}\n{{json .Config.Entrypoint}}\n{{json .Config.Cmd}}' %s", ssh.ShellQuote(container)))
	if err != nil {
		return nil, nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var labels map[string]string
	var argv []string
	if len(lines) > 0 {
		_ = json.Unmarshal([]byte(lines[0]), &labels)
	}
	for _, l := range lines[1:] {
		var part []string
		if json.Unmarshal([]byte(l), &part) == nil {
			argv = append(argv, part...)
		}
	}
	return labels, argv
}
