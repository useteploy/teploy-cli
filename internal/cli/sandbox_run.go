package cli

// sandbox_run.go — the `teploy sandbox` commands that act on one sandbox:
// exec, stop, resume, fork, rm and cp.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/sandbox"
	"github.com/useteploy/teploy/internal/ssh"
)

// exec

type sandboxExecOptions struct {
	daemon  sandboxDaemonOptions
	timeout time.Duration
	env     []string
	workdir string
}

// sandboxExecCapture bounds each output stream in a --json exec's document.
const sandboxExecCapture = 1 << 20

func newSandboxExecCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var o sandboxExecOptions
	cmd := &cobra.Command{
		Use:   "exec <id> -- <command>",
		Short: "Run a command in a sandbox",
		Long: "Run a command in a running sandbox, through sh -c in its /work, and stream its\n" +
			"output. The `--` ends teploy's own flags. Each word after it is passed as the\n" +
			"argument your shell made of it, so quoting survives; shell syntax (a pipe, a\n" +
			"redirect) goes inside one quoted word: -- 'ls | wc -l'.\n\n" +
			"Exit status: the command's own, as ssh passes it on (0, or the status it exited\n" +
			"with; a command its --timeout ended exits 124). 255 is a failure of teploy's own:\n" +
			"it could not reach the engine, the sandbox could not run the command, or the stream\n" +
			"ended before the exit was known.\n\n" +
			"With --json, stdout carries {machine_interface, server, host, run_id, exit_code,\n" +
			"timed_out, duration_ms, stdout, stderr, truncated}: the output is captured (at most\n" +
			"1 MiB a stream) instead of streamed. The document is printed only once the engine\n" +
			"has taken the command; an exec it refused prints the error envelope on stderr alone.\n" +
			"exit_code is null when the stream ended before the command's exit was known. A\n" +
			"command that exits nonzero is error-envelope code command-failed.\n\n" +
			"Examples:\n" +
			"  teploy sandbox exec 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod -- echo hello\n" +
			"  teploy sandbox exec 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod --timeout 10m -- make test",
		// Everything after the `--` is the command, flags and all: teploy's
		// own flags end at the `--`, so one id must come before it.
		// A usage error is a failure of this command's own too (255, like
		// every other): the status is the command's or teploy's, with no
		// third thing.
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
				return &sandboxExecError{err: errors.New("usage: teploy sandbox exec <id> -- <command> (the `--` ends teploy's own flags)")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxExec(cmd.Context(), flags, env, o, args[0], args[1:])
		},
	}
	bindSandboxDaemonFlags(cmd, &o.daemon)
	cmd.Flags().DurationVar(&o.timeout, "timeout", 0, "how long the command may run (default 2m)")
	cmd.Flags().StringArrayVar(&o.env, "env", nil, "KEY=VALUE in the command's environment (repeatable)")
	cmd.Flags().StringVar(&o.workdir, "workdir", "", "directory to run in, relative to the sandbox's /work (/work itself when not given; /work/… may be written in full)")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &sandboxExecError{err: err} })
	return cmd
}

func runSandboxExec(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxExecOptions, id string, words []string) error {
	err := execInSandbox(ctx, flags, env, o, id, words)
	if err == nil {
		return nil
	}
	// The command's own status is teploy's. Anything else is a failure of
	// this command's own, and must not read as one: 255.
	var command *sandboxCommandError
	if errors.As(err, &command) {
		return err
	}
	return &sandboxExecError{err: err}
}

// execInSandbox runs the command and prints what became of it.
func execInSandbox(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxExecOptions, id string, words []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Ctrl-C ends the command: closing the stream ends it on the engine's side.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()
	timeoutSec, err := wholeSeconds(o.timeout, "--timeout", 0)
	if err != nil {
		return err
	}
	wait := o.timeout
	if wait == 0 {
		wait = time.Duration(sandbox.DefaultExecTimeoutSec) * time.Second
	}
	commandEnv, err := parseKeyValue(o.env, "--env")
	if err != nil {
		return err
	}
	workdir := ""
	if strings.TrimSpace(o.workdir) != "" {
		if workdir, err = sandbox.ValidWorkDir(o.workdir); err != nil {
			return fmt.Errorf("--workdir: %w", err)
		}
	}
	// An id that is not one is refused before anything is opened.
	if _, err := sandbox.ValidID(id); err != nil {
		return err
	}
	d, err := openSandboxDaemon(ctx, env, flags, o.daemon)
	if err != nil {
		return err
	}
	defer d.Close()
	req := sandbox.ExecRequest{Cmd: sandboxCommand(words), Cwd: workdir, TimeoutSec: timeoutSec, Env: commandEnv}

	// Under --json the output is captured for the document; otherwise it is
	// streamed as it arrives.
	var stdout, stderr sandboxCapture
	var print func(stream sandbox.Stream, chunk string)
	if flags.JSON {
		print = func(stream sandbox.Stream, chunk string) {
			if stream == sandbox.Stderr {
				stderr.write(chunk)
				return
			}
			stdout.write(chunk)
		}
	} else {
		print = func(stream sandbox.Stream, chunk string) {
			if stream == sandbox.Stderr {
				fmt.Fprint(env.errOut, chunk)
				return
			}
			fmt.Fprint(env.out, chunk)
		}
	}
	started := false // the engine answered 200: the command is the engine's
	start := env.now()
	result, err := d.client.ExecStream(ctx, id, req, print, func() { started = true })

	// The document is printed once the engine has taken the command, and
	// only then: it says how the command ended, even when that is "not
	// known" (a stream that died before the exit; exit_code is null). An
	// exec the engine refused, or that was never answered, has nothing to
	// report on: a document with a null exit_code would say "it may have
	// finished", and nothing ran. Only the error envelope is printed.
	if flags.JSON && started {
		if err := writeJSONTo(env.out, sandboxExecEnvelope(d, id, result, err, env.now().Sub(start), stdout, stderr)); err != nil {
			return err
		}
	}
	if err != nil {
		return d.execFailure(ctx, id, err, started)
	}
	return sandboxExecStatus(result, wait)
}

// sandboxCommand builds the command line the engine runs (sh -c) from the
// words after the `--`. Each is quoted, so the arguments the sandbox's
// command receives are the words as this shell received them: a shell has
// already taken the user's quotes away, and joining with spaces would lose
// an argument's own spaces. Shell syntax reaches the sandbox the way it
// reaches any program: inside one quoted word (`-- 'ls | wc -l'`).
func sandboxCommand(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = ssh.ShellQuote(word)
	}
	return strings.Join(quoted, " ")
}

// execFailure is the failure of an exec itself (not of the command it
// ran). started says whether the engine answered 200, i.e. took the
// command.
//
// An exec the engine refused before the stream (404, 409, 503...), or
// that was not sent at all, ran nothing: OutcomeKnown says so, and the
// error is explained like that of any other request about a run. Anything
// else after the request left leaves the command's fate unknown: the
// stream is the only word on how it ended, and no exit arrived (the
// stream died, or no answer came at all). That is not reported as a
// failure: it is the unknown-outcome class.
func (d *sandboxDaemon) execFailure(ctx context.Context, id string, err error, started bool) error {
	if sandbox.OutcomeKnown(err) {
		return d.explainRun(ctx, id, err)
	}
	if started {
		return &sandboxUnknownOutcomeError{err: fmt.Errorf(
			"the stream from sandbox %s on %s ended before its command's exit arrived, so whether the command finished is unknown (%v).\nClosing the stream ends the command on the engine's side",
			id, d.server, err)}
	}
	return &sandboxUnknownOutcomeError{err: fmt.Errorf(
		"the command for sandbox %s on %s was sent, but no answer from the engine arrived (%v), so whether it ran is unknown.\nClosing the connection ends the command on the engine's side; it is not sent again",
		id, d.server, err)}
}

// How `sandbox exec` ends, as an exit status (exitStatus, root.go):
//
//   - the command exited 0: 0;
//   - it exited with another status, or its timeout ended it: that
//     status, as ssh and docker exec pass a command's status on;
//   - anything else is a failure of teploy's own, and 255: the engine
//     could not be reached or refused, the sandbox could not run the
//     command, the stream ended before the exit. 255 because a caller that
//     branches on a command's status must never read one of those as the
//     command having exited with it. A command that itself exits 255
//     cannot be told from them by the status alone; under --json the
//     classes can (command-failed, and the document on stdout).
const sandboxExecFailure = 255

// sandboxCommandError is how the command itself ended badly: it exited
// with code, or its timeout ended it. The error is of the machine error
// class command-failed; envelopeMessage is what the envelope says of it.
type sandboxCommandError struct {
	code            int
	envelopeMessage string
	err             error
}

func (e *sandboxCommandError) Error() string { return e.err.Error() }
func (e *sandboxCommandError) Unwrap() error { return e.err }
func (e *sandboxCommandError) ExitCode() int { return e.code }

// sandboxExecError is any other failure of `sandbox exec`: the command's
// status is not known, or there was none.
type sandboxExecError struct{ err error }

func (e *sandboxExecError) Error() string { return e.err.Error() }
func (e *sandboxExecError) Unwrap() error { return e.err }
func (e *sandboxExecError) ExitCode() int { return sandboxExecFailure }

// sandboxExecStatus is the command's own outcome as an error: nonzero is
// nonzero, a timeout says so, and -1 is the sandbox failing to run it.
func sandboxExecStatus(result sandbox.ExecResult, wait time.Duration) error {
	timedOut := fmt.Errorf("the command timed out after %s", wait)
	switch {
	case result.TimedOut && result.ExitCode > 0:
		// The engine ends a command at its timeout with its status (124,
		// as coreutils timeout does): it is passed on like any other.
		return &sandboxCommandError{code: result.ExitCode, envelopeMessage: "the command timed out", err: timedOut}
	case result.TimedOut:
		return timedOut // no status to pass on: teploy's own failure
	case result.ExitCode == -1:
		return errors.New("the sandbox could not run the command; its reason is in its stderr")
	case result.ExitCode > 255:
		return fmt.Errorf("the command exited with %d, which is not a status this CLI can pass on", result.ExitCode)
	case result.ExitCode != 0:
		return &sandboxCommandError{code: result.ExitCode, envelopeMessage: fmt.Sprintf("the command exited with status %d", result.ExitCode), err: fmt.Errorf("exit status %d", result.ExitCode)}
	}
	return nil
}

// sandboxCapture holds an exec stream for a --json document, up to
// sandboxExecCapture bytes.
type sandboxCapture struct {
	text      strings.Builder
	truncated bool
}

func (c *sandboxCapture) write(chunk string) {
	if c.text.Len() >= sandboxExecCapture {
		c.truncated = true
		return
	}
	if room := sandboxExecCapture - c.text.Len(); len(chunk) > room {
		chunk = chunk[:room]
		c.truncated = true
	}
	c.text.WriteString(chunk)
}

// stop

func newSandboxStopCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var o sandboxDaemonOptions
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a sandbox and keep its filesystem",
		Long: "Stop a sandbox: the engine commits its filesystem, removes its container and keeps\n" +
			"the sandbox, under its own id, as stopped. It costs disk and nothing else until it\n" +
			"is resumed, forked or removed, or its retention passes (14 days by default).\n\n" +
			"With --json, the envelope is {machine_interface, server, host, run}.\n\n" +
			"Example:\n  teploy sandbox stop 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxStop(cmd.Context(), flags, env, o, args[0])
		},
	}
	bindSandboxDaemonFlags(cmd, &o)
	return cmd
}

// stop stops a run. The engine answers 409 to a stop repeated after it
// succeeded; the contract says to read the run to confirm, and a run that
// is already stopped is what was asked for.
func (d *sandboxDaemon) stop(ctx context.Context, id string) (run sandbox.Run, already bool, err error) {
	run, err = d.client.Stop(ctx, id)
	if sandbox.IsConflict(err) {
		if current, findErr := d.client.Find(ctx, id); findErr == nil && current.State == "stopped" {
			return current, true, nil
		}
	}
	return run, false, err
}

func keptUntil(run sandbox.Run) string {
	if run.RetainUntil == nil {
		return ""
	}
	return " (kept until " + sandboxTime(*run.RetainUntil) + ")"
}

func runSandboxStop(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxDaemonOptions, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d, err := openSandboxDaemon(ctx, env, flags, o)
	if err != nil {
		return err
	}
	defer d.Close()
	run, already, err := d.stop(ctx, id)
	if err != nil {
		return d.explainRun(ctx, id, err)
	}
	if flags.JSON {
		return writeJSONTo(env.out, sandboxRunEnvelope(d, run))
	}
	if already {
		fmt.Fprintf(env.out, "Sandbox %s on %s is already stopped%s\n", run.ID, d.server, keptUntil(run))
		return nil
	}
	fmt.Fprintf(env.out, "Stopped %s on %s%s\n\n  teploy sandbox resume %s --host %s\n", run.ID, d.server, keptUntil(run), run.ID, d.server)
	return nil
}

// resume

func newSandboxResumeCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var (
		o   sandboxDaemonOptions
		ttl time.Duration
	)
	cmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a stopped sandbox",
		Long: "Resume a stopped sandbox under its own id with a new deadline. Its filesystem is\n" +
			"what stop kept.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, run}.\n\n" +
			"Example:\n  teploy sandbox resume 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod --ttl 2h",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxResume(cmd.Context(), flags, env, o, ttl, args[0])
		},
	}
	bindSandboxDaemonFlags(cmd, &o)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the sandbox lives after this (default 30m, at most 24h)")
	return cmd
}

// resume is stop's counterpart: a resume of a running run is a 409, and a
// run that is already running is what was asked for.
func (d *sandboxDaemon) resume(ctx context.Context, id string, req sandbox.ResumeRequest) (run sandbox.Run, already bool, err error) {
	run, err = d.client.Resume(ctx, id, req)
	if sandbox.IsConflict(err) {
		if current, findErr := d.client.Find(ctx, id); findErr == nil && current.State == "running" {
			return current, true, nil
		}
	}
	return run, false, err
}

func runSandboxResume(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxDaemonOptions, ttl time.Duration, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ttlSec, err := wholeSeconds(ttl, "--ttl", sandbox.MaxTTL)
	if err != nil {
		return err
	}
	d, err := openSandboxDaemon(ctx, env, flags, o)
	if err != nil {
		return err
	}
	defer d.Close()
	run, already, err := d.resume(ctx, id, sandbox.ResumeRequest{TTLSec: ttlSec})
	if err != nil {
		return d.explainRun(ctx, id, err)
	}
	if flags.JSON {
		return writeJSONTo(env.out, sandboxRunEnvelope(d, run))
	}
	if already {
		fmt.Fprintf(env.out, "Sandbox %s on %s is already running (until %s)\n", run.ID, d.server, sandboxTime(run.ExpiresAt))
		return nil
	}
	fmt.Fprintf(env.out, "Resumed %s on %s (running until %s)\n", run.ID, d.server, sandboxTime(run.ExpiresAt))
	return nil
}

// fork

type sandboxForkOptions struct {
	daemon sandboxDaemonOptions
	ttl    time.Duration
	labels []string
	driver string
}

func newSandboxForkCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var o sandboxForkOptions
	cmd := &cobra.Command{
		Use:   "fork <id>",
		Short: "Start a new sandbox from this one's filesystem",
		Long: "Start a new sandbox from a sandbox's filesystem and settings. The source is left\n" +
			"as it is, running or stopped.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, run}.\n\n" +
			"Example:\n  teploy sandbox fork 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxFork(cmd.Context(), flags, env, o, args[0])
		},
	}
	bindSandboxDaemonFlags(cmd, &o.daemon)
	cmd.Flags().DurationVar(&o.ttl, "ttl", 0, "how long the new sandbox lives (default 30m, at most 24h)")
	cmd.Flags().StringArrayVar(&o.labels, "label", nil, "KEY=VALUE label to set on the new sandbox, on top of the source's (repeatable)")
	cmd.Flags().StringVar(&o.driver, "driver", "docker", "docker (default), or vm to opt into the experimental vm driver")
	return cmd
}

func runSandboxFork(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxForkOptions, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := sandboxDriverFlag(o.driver); err != nil {
		return err
	}
	ttlSec, err := wholeSeconds(o.ttl, "--ttl", sandbox.MaxTTL)
	if err != nil {
		return err
	}
	extra, err := parseKeyValue(o.labels, "--label")
	if err != nil {
		return err
	}
	d, err := openSandboxDaemon(ctx, env, flags, o.daemon)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.checkDriver(ctx, o.driver); err != nil {
		return err
	}
	source, err := d.client.Find(ctx, id)
	if err != nil {
		return d.explainRun(ctx, id, err)
	}
	// The engine gives a fork its source's labels unless the body names
	// them. They are named, so the fork gets its own run label (a forked
	// copy would otherwise carry the label of the run it came from).
	label, err := env.newRunLabel()
	if err != nil {
		return err
	}
	labels := make(map[string]string, len(source.Labels)+len(extra)+1)
	for key, value := range source.Labels {
		labels[key] = value
	}
	for key, value := range extra {
		labels[key] = value
	}
	labels[sandboxRunLabel] = label
	run, err := d.client.Fork(ctx, source.ID, sandbox.ForkRequest{TTLSec: ttlSec, Labels: labels})
	if err != nil {
		if sandbox.OutcomeKnown(err) {
			return d.explainRun(ctx, source.ID, err)
		}
		return d.unknownOutcome("fork", label, err)
	}
	if flags.JSON {
		return writeJSONTo(env.out, sandboxRunEnvelope(d, run))
	}
	fmt.Fprintf(env.out, "Forked %s from %s on %s (running until %s)\n\n  teploy sandbox exec %s --host %s -- <command>\n",
		run.ID, source.ID, d.server, sandboxTime(run.ExpiresAt), run.ID, d.server)
	return nil
}

// rm

func newSandboxRMCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var (
		o   sandboxDaemonOptions
		yes bool
	)
	cmd := &cobra.Command{
		Use:   "rm <id>",
		Short: "Destroy a sandbox",
		Long: "Destroy a sandbox now and lose its filesystem. Asks first unless --yes.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, run_id}; --json\n" +
			"needs --yes.\n\n" +
			"Example:\n  teploy sandbox rm 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxRemove(cmd.Context(), flags, env, o, yes, args[0])
		},
	}
	bindSandboxDaemonFlags(cmd, &o)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask before destroying the sandbox")
	return cmd
}

func runSandboxRemove(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxDaemonOptions, yes bool, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if flags.JSON && !yes {
		return errors.New("--json needs --yes: a sandbox is never destroyed on a prompt in machine mode")
	}
	d, err := openSandboxDaemon(ctx, env, flags, o)
	if err != nil {
		return err
	}
	defer d.Close()
	// What will be destroyed, read first: a sandbox that is not there is
	// said so before anything is asked.
	run, err := d.client.Find(ctx, id)
	if err != nil {
		return d.explainRun(ctx, id, err)
	}
	if !yes && !env.confirm(fmt.Sprintf("Destroy sandbox %s on %s (%s, %s)? Its filesystem is lost.", run.ID, d.server, run.Image, run.State)) {
		return errSandboxAborted
	}
	if err := d.client.Destroy(ctx, run.ID); err != nil {
		if sandbox.OutcomeKnown(err) {
			return d.explainRun(ctx, run.ID, err)
		}
		return &sandboxUnknownOutcomeError{err: fmt.Errorf(
			"the request to destroy sandbox %s on %s did not finish, so whether it happened is unknown (%v).\nRun the command again to be sure: a sandbox that is already gone is reported as not found",
			run.ID, d.server, err)}
	}
	if flags.JSON {
		return writeJSONTo(env.out, sandboxDestroyEnvelope(d, run.ID))
	}
	fmt.Fprintf(env.out, "Destroyed %s on %s\n", run.ID, d.server)
	return nil
}

// cp

// Which way a copy goes, in the --json document.
const (
	sandboxCopyTo   = "to_sandbox"
	sandboxCopyFrom = "from_sandbox"
)

func newSandboxCPCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var o sandboxDaemonOptions
	cmd := &cobra.Command{
		Use:   "cp <id>:<path> <local> | cp <local> <id>:<path>",
		Short: "Copy a file to or from a sandbox",
		Long: "Copy one file between this machine and a running sandbox. The sandbox's side is\n" +
			"<id>:<path>, relative to the sandbox's /work (a leading slash is allowed, and /work/\n" +
			"may be written in full: 01J…:/work/app.tar.gz is 01J…:app.tar.gz). At most 64 MiB.\n" +
			"With the docker driver a write creates the file's directory when it is\n" +
			"missing.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, run_id, path,\n" +
			"direction, bytes}.\n\n" +
			"Examples:\n" +
			"  teploy sandbox cp app.tar.gz 01JCV5ZK3Q7X9B2D4E6F8A0B1C:app.tar.gz --host prod\n" +
			"  teploy sandbox cp 01JCV5ZK3Q7X9B2D4E6F8A0B1C:out/report.pdf ./report.pdf --host prod",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxCopy(cmd.Context(), flags, env, o, args)
		},
	}
	bindSandboxDaemonFlags(cmd, &o)
	return cmd
}

// sandboxCopyArg is one operand of `cp`: a sandbox's file, or a local one.
// A sandbox's is told by an id before the colon, so a local path with a
// colon in it stays local.
type sandboxCopyArg struct {
	id, path  string
	inSandbox bool
	local     string
}

func parseSandboxCopyArg(arg string) (sandboxCopyArg, error) {
	head, tail, found := strings.Cut(arg, ":")
	if !found {
		return sandboxCopyArg{local: arg}, nil
	}
	id, idErr := sandbox.ValidID(head)
	if idErr != nil {
		return sandboxCopyArg{local: arg}, nil
	}
	path, pathErr := sandbox.ValidWorkPath(tail)
	if pathErr != nil {
		return sandboxCopyArg{}, fmt.Errorf("%q: %w", arg, pathErr)
	}
	return sandboxCopyArg{id: id, path: path, inSandbox: true}, nil
}

func runSandboxCopy(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxDaemonOptions, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	from, err := parseSandboxCopyArg(args[0])
	if err != nil {
		return err
	}
	to, err := parseSandboxCopyArg(args[1])
	if err != nil {
		return err
	}
	if from.inSandbox == to.inSandbox {
		if from.inSandbox {
			return errors.New("both paths are in a sandbox; one must be on this machine")
		}
		return errors.New("neither path is in a sandbox: write one as <id>:<path>")
	}
	d, err := openSandboxDaemon(ctx, env, flags, o)
	if err != nil {
		return err
	}
	defer d.Close()

	var result sandboxCopyDTO
	if from.inSandbox {
		content, err := d.client.GetFile(ctx, from.id, from.path)
		if sandbox.IsNotFound(err) {
			return fmt.Errorf("sandbox %s on %s has no file %s", from.id, d.server, from.path)
		}
		if err != nil {
			return d.explainRun(ctx, from.id, err)
		}
		target, err := writeSandboxLocalCopy(to.local, path.Base(from.path), content)
		if err != nil {
			return err
		}
		result = sandboxCopyEnvelope(d, from.id, from.path, sandboxCopyFrom, len(content))
		if !flags.JSON {
			fmt.Fprintf(env.out, "Copied %s from %s:%s on %s to %s\n", sandboxSize(len(content)), from.id, from.path, d.server, target)
		}
	} else {
		content, err := readSandboxLocalCopy(from.local)
		if err != nil {
			return err
		}
		if err := d.client.PutFile(ctx, to.id, to.path, content); err != nil {
			return d.explainRun(ctx, to.id, err)
		}
		result = sandboxCopyEnvelope(d, to.id, to.path, sandboxCopyTo, len(content))
		if !flags.JSON {
			fmt.Fprintf(env.out, "Copied %s from %s to %s:%s on %s\n", sandboxSize(len(content)), from.local, to.id, to.path, d.server)
		}
	}
	if flags.JSON {
		return writeJSONTo(env.out, result)
	}
	return nil
}

// readSandboxLocalCopy reads a local file for a copy into a sandbox.
func readSandboxLocalCopy(name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; `cp` copies one file", name)
	}
	if info.Size() > sandbox.MaxFileBytes {
		return nil, fmt.Errorf("%s is %s; the engine takes at most %s in one write", name, sandboxSize(int(info.Size())), sandboxSize(sandbox.MaxFileBytes))
	}
	content, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return content, nil
}

// writeSandboxLocalCopy writes a copy from a sandbox whole or not at all:
// a temporary file beside the target, then a rename over it. A target that
// is a directory receives the file under its own name.
//
// A file that is already there keeps its mode, as `cp` leaves it: only what
// the file holds changes. A new one is 0644 (the engine's files route says
// nothing of the sandbox file's mode).
func writeSandboxLocalCopy(target, name string, content []byte) (string, error) {
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, name)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".teploy-*")
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", target, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("writing %s: %w", target, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", fmt.Errorf("writing %s: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("writing %s: %w", target, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return "", fmt.Errorf("writing %s: %w", target, err)
	}
	return target, nil
}

func sandboxSize(bytes int) string {
	switch size := int64(bytes); {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}
