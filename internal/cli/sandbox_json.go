package cli

// Machine envelopes of `teploy sandbox ... --json` (capability sandbox-v1,
// contracts/schema/sandbox-*.schema.json). Every document carries
// machine_interface at its root like the rest of the CLI. Field names are
// the CLI's snake_case, not the engine's camelCase wire names, so the
// contract does not move when the engine adds or renames a field. An
// optional field of the engine's is left out when the engine left it out:
// absent is not zero.

import (
	"encoding/base64"
	"time"
	"unicode/utf8"

	"github.com/useteploy/teploy/internal/sandbox"
)

type sandboxRunDTO struct {
	ID          string            `json:"id"`
	Image       string            `json:"image"`
	Network     string            `json:"network"`
	User        string            `json:"user"`
	State       string            `json:"state"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	OnExpire    string            `json:"on_expire"`
	Labels      map[string]string `json:"labels,omitempty"`
	Usage       sandboxUsageDTO   `json:"usage"`
	StoppedAt   *time.Time        `json:"stopped_at,omitempty"`
	RetainUntil *time.Time        `json:"retain_until,omitempty"`
	ForkedFrom  string            `json:"forked_from,omitempty"`
	Lease       *sandboxLeaseDTO  `json:"lease,omitempty"`
}

type sandboxUsageDTO struct {
	RunningSec int        `json:"running_sec"`
	CPUSeconds *float64   `json:"cpu_seconds,omitempty"`
	RxBytes    *int64     `json:"rx_bytes,omitempty"`
	TxBytes    *int64     `json:"tx_bytes,omitempty"`
	DiskBytes  *int64     `json:"disk_bytes,omitempty"`
	SampledAt  *time.Time `json:"sampled_at,omitempty"`
}

type sandboxLeaseDTO struct {
	Holder     string    `json:"holder"`
	Generation uint64    `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func sandboxRunRow(run sandbox.Run) sandboxRunDTO {
	row := sandboxRunDTO{
		ID: run.ID, Image: run.Image, Network: run.Network, User: run.User, State: run.State,
		CreatedAt: run.CreatedAt, ExpiresAt: run.ExpiresAt, OnExpire: run.OnExpire,
		Labels: run.Labels, StoppedAt: run.StoppedAt, RetainUntil: run.RetainUntil, ForkedFrom: run.ForkedFrom,
	}
	row.Usage.RunningSec = run.Usage.RunningSec
	if run.Usage.CPUSeconds != nil {
		row.Usage.CPUSeconds = run.Usage.CPUSeconds
	}
	if run.Usage.RxBytes != nil {
		row.Usage.RxBytes = run.Usage.RxBytes
	}
	if run.Usage.TxBytes != nil {
		row.Usage.TxBytes = run.Usage.TxBytes
	}
	if run.Usage.DiskBytes != nil {
		row.Usage.DiskBytes = run.Usage.DiskBytes
	}
	if run.Usage.SampledAt != nil {
		row.Usage.SampledAt = run.Usage.SampledAt
	}
	if run.Lease != nil {
		row.Lease = &sandboxLeaseDTO{Holder: run.Lease.Holder, Generation: run.Lease.Generation, ExpiresAt: run.Lease.ExpiresAt}
	}
	return row
}

// sandboxRunResultDTO is `new`, `stop`, `resume` and `fork --json`.
type sandboxRunResultDTO struct {
	MachineInterface int           `json:"machine_interface"`
	Server           string        `json:"server"`
	Host             string        `json:"host"`
	Run              sandboxRunDTO `json:"run"`
}

func sandboxRunEnvelope(d *sandboxDaemon, run sandbox.Run) sandboxRunResultDTO {
	return sandboxRunResultDTO{MachineInterface: MachineInterface, Server: d.server, Host: d.host, Run: sandboxRunRow(run)}
}

// sandboxRunsDTO is `ls --json`. runs are oldest first.
type sandboxRunsDTO struct {
	MachineInterface int             `json:"machine_interface"`
	Server           string          `json:"server"`
	Host             string          `json:"host"`
	Driver           string          `json:"driver"`
	Runs             []sandboxRunDTO `json:"runs"`
}

func sandboxRunsEnvelope(d *sandboxDaemon, driver string, runs []sandbox.Run) sandboxRunsDTO {
	rows := make([]sandboxRunDTO, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, sandboxRunRow(run))
	}
	return sandboxRunsDTO{MachineInterface: MachineInterface, Server: d.server, Host: d.host, Driver: driver, Runs: rows}
}

// sandboxExecDTO is `exec --json`. exit_code is the command's (-1 when the
// sandbox itself could not run it), or null when the stream ended before
// the command's exit was known. stdout and stderr hold at most 1 MiB each;
// truncated says a stream was longer.
//
// The output is had from the engine byte for byte (it is asked for base64
// when it is a release that speaks it), and stdout and stderr are it as
// text. JSON strings are UTF-8, so a stream holding bytes that are not is
// also given as stdout_base64 or stderr_base64: the exact bytes, present
// only then (absent means the text is already byte for byte).
type sandboxExecDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Server           string `json:"server"`
	Host             string `json:"host"`
	RunID            string `json:"run_id"`
	ExitCode         *int   `json:"exit_code"`
	TimedOut         bool   `json:"timed_out"`
	DurationMS       int64  `json:"duration_ms"`
	Stdout           string `json:"stdout"`
	Stderr           string `json:"stderr"`
	StdoutBase64     string `json:"stdout_base64,omitempty"`
	StderrBase64     string `json:"stderr_base64,omitempty"`
	Truncated        bool   `json:"truncated"`
}

func sandboxExecEnvelope(d *sandboxDaemon, id string, result sandbox.ExecResult, err error, elapsed time.Duration, stdout, stderr sandboxCapture) sandboxExecDTO {
	dto := sandboxExecDTO{
		MachineInterface: MachineInterface, Server: d.server, Host: d.host, RunID: id,
		DurationMS: elapsed.Milliseconds(), Stdout: stdout.text.String(), Stderr: stderr.text.String(),
		Truncated: stdout.truncated || stderr.truncated,
	}
	// encoding/json writes each byte of stdout that is not valid UTF-8 as
	// U+FFFD; the bytes themselves go beside it.
	if !utf8.ValidString(dto.Stdout) {
		dto.StdoutBase64 = base64.StdEncoding.EncodeToString([]byte(dto.Stdout))
	}
	if !utf8.ValidString(dto.Stderr) {
		dto.StderrBase64 = base64.StdEncoding.EncodeToString([]byte(dto.Stderr))
	}
	if err == nil {
		code := result.ExitCode
		dto.ExitCode, dto.TimedOut = &code, result.TimedOut
	}
	return dto
}

// sandboxDestroyDTO is `rm --yes --json`.
type sandboxDestroyDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Server           string `json:"server"`
	Host             string `json:"host"`
	RunID            string `json:"run_id"`
}

func sandboxDestroyEnvelope(d *sandboxDaemon, id string) sandboxDestroyDTO {
	return sandboxDestroyDTO{MachineInterface: MachineInterface, Server: d.server, Host: d.host, RunID: id}
}

// sandboxCopyDTO is `cp --json`. direction is to_sandbox or from_sandbox.
type sandboxCopyDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Server           string `json:"server"`
	Host             string `json:"host"`
	RunID            string `json:"run_id"`
	Path             string `json:"path"`
	Direction        string `json:"direction"`
	Bytes            int    `json:"bytes"`
}

func sandboxCopyEnvelope(d *sandboxDaemon, id, filePath, direction string, bytes int) sandboxCopyDTO {
	return sandboxCopyDTO{MachineInterface: MachineInterface, Server: d.server, Host: d.host, RunID: id, Path: filePath, Direction: direction, Bytes: bytes}
}

// sandboxHostResultDTO is `sandbox host --yes --json`: what was done
// (action: install, upgrade, start or unchanged) and the engine as it is
// now. from is the version an upgrade replaced.
type sandboxHostResultDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Server           string `json:"server"`
	Host             string `json:"host"`
	OK               bool   `json:"ok"`
	Action           string `json:"action"`
	Version          string `json:"version"`
	From             string `json:"from,omitempty"`
	Addr             string `json:"addr"`
	TokenFile        string `json:"token_file"`
}
