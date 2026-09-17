package cli

// secretinput.go — the stdin secret-input contract (audit UPSTREAM-1, from
// teploy-dash A23). Secret values used to travel only in argv (`env set
// KEY=value`, `template --var k=v`, `kv set KEY VALUE`), where they are
// visible to any local process listing (`ps aux`, /proc/<pid>/cmdline) for
// the life of the invocation. Callers that pipe secrets over stdin instead
// (teploy-dash already does this for registry login) need a flag that says
// where the value comes from. Three commands take one:
//
//	teploy env set KEY --stdin               # value = stdin verbatim
//	teploy kv set KEY --stdin [--ttl N]      # value = stdin verbatim
//	teploy template deploy NAME --var-stdin  # JSON object {"name":"value"}
//
// The argv forms keep working unchanged for interactive use.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// maxSecretStdinBytes bounds a single stdin payload. Values are env entries,
// kv entries, or template variables — all small; the bound keeps a piped
// mistake (e.g. an unbounded stream) from consuming memory.
const maxSecretStdinBytes = 1 << 20 // 1 MiB

// readSecretValue reads one secret value from r verbatim. No trailing
// newline is stripped: this is a machine-to-machine contract (the caller
// feeds the exact bytes); interactive users should use the argv forms.
//
// NUL is rejected: it can never appear in an argv value (exec forbids it),
// so accepting it here would create values the argv path cannot round-trip
// and that corrupt .env files and SQL literals downstream.
func readSecretValue(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSecretStdinBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading value from stdin: %w", err)
	}
	if len(data) > maxSecretStdinBytes {
		return "", fmt.Errorf("stdin value exceeds the %d byte limit", maxSecretStdinBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("stdin value contains a NUL byte")
	}
	return string(data), nil
}

// readVarMapStdin reads template variable values from r as a JSON object
// with string values ({"API_KEY":"...", ...}) for --var-stdin. Entries
// override --var flags with the same name. An empty object is accepted (a
// no-op merge) so a caller with no secret vars can use one code path.
func readVarMapStdin(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSecretStdinBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading variables from stdin: %w", err)
	}
	if len(data) > maxSecretStdinBytes {
		return nil, fmt.Errorf("--var-stdin payload exceeds the %d byte limit", maxSecretStdinBytes)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing --var-stdin JSON object: %w", err)
	}
	for k, v := range m {
		if strings.ContainsRune(k, 0) || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("--var-stdin entry %q contains a NUL byte", k)
		}
	}
	return m, nil
}
