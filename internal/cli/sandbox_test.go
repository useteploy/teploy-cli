package cli

// sandbox_test.go — the `teploy sandbox` commands. Parsing and validation
// first (nothing connects), then each way the engine cannot be reached and
// what is said about it.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSandboxCommandsAreOnTheRoot(t *testing.T) {
	root := NewRootCmd("test")
	group, _, err := root.Find([]string{"sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cmd := range group.Commands() {
		names = append(names, cmd.Name())
	}
	if got := strings.Join(names, " "); got != "cp exec fork host ls new resume rm stop" {
		t.Errorf("sandbox has the commands %s", got)
	}
	for _, word := range []string{"early access", "self-hosted"} {
		if !strings.Contains(group.Short, word) {
			t.Errorf("the group's short help does not say %q: %q", word, group.Short)
		}
	}
}

// The root's own flags reach the sandbox commands: --json prints nothing
// before the document, and --host is the server that is connected to.
func TestSandboxTakesTheRootsFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no identity, no servers.yml: the connection fails before any network
	t.Setenv("TEPLOY_SSH_KEY", "")
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"sandbox", "ls", "--host", "203.0.113.1", "--json"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "connecting to 203.0.113.1") {
		t.Fatalf("got %v, want the failure to connect to the server --host named", err)
	}
	if out.Len() != 0 {
		t.Fatalf("under --json, before any document, stdout held %q", out.String())
	}
}

func TestSandboxNeedsTheServer(t *testing.T) {
	for _, args := range [][]string{
		{"ls"}, {"new", "--image", "alpine"}, {"exec", sandboxTestID(0), "--", "true"},
		{"stop", sandboxTestID(0)}, {"resume", sandboxTestID(0)}, {"fork", sandboxTestID(0)},
		{"rm", sandboxTestID(0)}, {"cp", "a", sandboxTestID(0) + ":b"},
	} {
		h := newInstalledSandboxHarness(t)
		h.flags.Host = ""
		err := h.run(args...)
		if err == nil || !strings.Contains(err.Error(), "--host is required") {
			t.Errorf("%v: got %v, want the --host requirement", args, err)
		}
		if len(h.connected) != 0 {
			t.Errorf("%v: connected without --host", args)
		}
	}
}

// A request that is wrong is refused here, before anything reaches a server.
func TestSandboxNewValidatesItsFlags(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"new"}, "--image is required"},
		{[]string{"new", "--image", "alpine", "--ttl", "500ms"}, "is under a second"},
		{[]string{"new", "--image", "alpine", "--ttl", "25h"}, "over the engine's limit of 24h"},
		{[]string{"new", "--image", "alpine", "--network", "wild"}, "--network must be none, allowlist or open"},
		{[]string{"new", "--image", "alpine", "--env", "NOEQUALS"}, `--env "NOEQUALS" is not KEY=VALUE`},
		{[]string{"new", "--image", "alpine", "--env", "A=1", "--env", "A=2"}, `--env "A" is given twice`},
		{[]string{"new", "--image", "alpine", "--label", "x"}, `--label "x" is not KEY=VALUE`},
		{[]string{"new", "--image", "alpine", "--cpus", "-1"}, "must not be negative"},
		{[]string{"new", "--image", "alpine", "--memory-mb", "-5"}, "must not be negative"},
		{[]string{"new", "--image", "alpine", "--run-as", "admin"}, "--run-as must be default or root"},
		{[]string{"new", "--image", "alpine", "--on-expire", "pause"}, "--on-expire must be destroy or stop"},
		{[]string{"new", "--image", "alpine", "--driver", "firecracker"}, "--driver must be docker, or vm"},
		{[]string{"new", "--image", "alpine", "--addr", "http://127.0.0.1:7439"}, "is not host:port"},
		{[]string{"new", "--image", "alpine", "--addr", "7439"}, "is not host:port"},
	}
	for _, c := range cases {
		h := newInstalledSandboxHarness(t)
		err := h.run(c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want it to say %q", c.args, err, c.want)
		}
		if len(h.connected) != 0 {
			t.Errorf("%v: connected to a server with a request that is refused", c.args)
		}
	}

	// The engine takes 16 labels; the CLI adds one of its own.
	var many []string
	for i := 0; i < 16; i++ {
		many = append(many, "--label", "k"+string(rune('a'+i))+"=v")
	}
	h := newInstalledSandboxHarness(t)
	if err := h.run(append([]string{"new", "--image", "alpine"}, many...)...); err == nil || !strings.Contains(err.Error(), "at most 15 --label") {
		t.Errorf("16 labels: %v", err)
	}
}

// The engine speaks plain HTTP, so the bearer token would travel to
// whatever --addr names, unencrypted. An address that is not the server's
// loopback is refused, before anything is connected, and is taken with
// --allow-remote-addr.
func TestSandboxAddrOffTheLoopbackNeedsTheOptIn(t *testing.T) {
	for _, addr := range []string{"10.0.0.5:7439", "attacker.example:443", "localhost:7439", "[fd00::5]:7439"} {
		h := newInstalledSandboxHarness(t)
		err := h.run("ls", "--addr", addr)
		for _, want := range []string{"engine address \"" + addr + "\" is not on the server's loopback", "the bearer token would be sent there in cleartext", "pass --allow-remote-addr", "only on a network you trust"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("--addr %s: the error does not say %q: %v", addr, want, err)
			}
		}
		if len(h.connected) != 0 || h.link.dials != 0 {
			t.Errorf("--addr %s: the server was connected to (%d) or the address dialed (%d) for an address that was refused", addr, len(h.connected), h.link.dials)
		}
		h = newInstalledSandboxHarness(t)
		h.flags.JSON = true
		if err := h.run("ls", "--addr", addr); err == nil || classifyMachineError(err) != codeInternal || h.out.Len() != 0 {
			t.Errorf("--addr %s under --json: %v (%s), stdout %q", addr, err, classifyMachineError(err), h.out.String())
		}
	}

	// With the opt-in the address is used.
	h := newInstalledSandboxHarness(t)
	if err := h.run("ls", "--addr", "10.0.0.5:7439", "--allow-remote-addr"); err != nil || !strings.Contains(h.out.String(), "No sandboxes on prod") {
		t.Errorf("with --allow-remote-addr: %v, output %q", err, h.out.String())
	}
	if len(h.connected) != 1 {
		t.Errorf("connected to %v", h.connected)
	}

	// Every command but `host` (which installs at the loopback itself) has it.
	for _, cmd := range newSandboxCmd(&Flags{}).Commands() {
		if cmd.Name() == "host" {
			continue
		}
		if cmd.Flags().Lookup("allow-remote-addr") == nil {
			t.Errorf("`sandbox %s` has no --allow-remote-addr", cmd.Name())
		}
	}
}

func TestSandboxExecTakesTheCommandAfterTheDash(t *testing.T) {
	id := sandboxTestID(0)
	for _, args := range [][]string{{"exec"}, {"exec", id}, {"exec", id, "--"}, {"exec", id, "echo", "hi"}} {
		h := newInstalledSandboxHarness(t)
		err := h.run(args...)
		if err == nil || !strings.Contains(err.Error(), "usage: teploy sandbox exec <id> -- <command>") {
			t.Errorf("%v: got %v, want the usage", args, err)
		}
		if len(h.connected) != 0 {
			t.Errorf("%v: connected with no command to run", args)
		}
	}

	// After the `--` every word is the command, flags of its own included;
	// before it, teploy's flags.
	h := newInstalledSandboxHarness(t)
	h.engine.runs = append(h.engine.runs, h.engine.newRun("alpine"))
	if err := h.run("exec", "--timeout", "30s", "--env", "A=b", id, "--", "printf", "%s  %s", "hello", "world"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); got != "hello  world" {
		t.Errorf("output %q", got)
	}
	if sent := h.engine.execs[0]; sent.Cmd != `'printf' '%s  %s' 'hello' 'world'` || sent.TimeoutSec != 30 || sent.Env["A"] != "b" {
		t.Errorf("the engine was asked %+v", sent)
	}

	// A word's own spaces and quotes survive the shell the command runs
	// under: an argument is passed as the argument this shell received.
	h.out.Reset()
	if err := h.run("exec", id, "--", "echo", "a  b", "it's"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); got != "a  b it's\n" {
		t.Errorf("output %q, want the arguments as they were given", got)
	}
	if got := h.engine.execs[1].Cmd; got != `'echo' 'a  b' 'it'"'"'s'` {
		t.Errorf("the command line sent was %s", got)
	}

	// One word is a command line of its own: shell syntax in it works, as
	// with ssh, and is sent as written (not quoted into one program name).
	h.out.Reset()
	if err := h.run("exec", id, "--", "printf 'a\\nb\\n' | wc -l | tr -d ' '"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); got != "2\n" {
		t.Errorf("a pipe in one word gave %q, want 2", got)
	}
	if got := h.engine.execs[2].Cmd; got != "printf 'a\\nb\\n' | wc -l | tr -d ' '" {
		t.Errorf("one word was sent as %s, want it as written", got)
	}

	// --workdir is relative to the sandbox's /work, which may be written
	// in full: /work is the work root itself (the default), /work/build
	// is build, and the sandbox's root is not somewhere to run in.
	for _, c := range []struct{ workdir, cwd string }{
		{"", ""}, {"/work", ""}, {"/work/", ""}, {"build", "build"}, {"/build", "build"}, {"/work/build", "build"},
	} {
		h.out.Reset()
		if err := h.run("exec", "--workdir", c.workdir, id, "--", "true"); err != nil {
			t.Fatalf("--workdir %q: %v", c.workdir, err)
		}
		if got := h.engine.execs[len(h.engine.execs)-1].Cwd; got != c.cwd {
			t.Errorf("--workdir %q sent cwd %q, want %q", c.workdir, got, c.cwd)
		}
	}
	// (A leading slash elsewhere names the sandbox's root and is dropped,
	// so `/etc` would be `etc` in /work: the rule the files route has.)
	for _, workdir := range []string{"/", "/work/../etc", "build/..", "/work/.."} {
		before := h.engine.execCount()
		if err := h.run("exec", "--workdir", workdir, id, "--", "true"); err == nil || !strings.Contains(err.Error(), "--workdir: ") || h.engine.execCount() != before {
			t.Errorf("--workdir %q: %v (%d execs sent; want none)", workdir, err, h.engine.execCount()-before)
		}
	}
}

func TestSandboxCopyArgs(t *testing.T) {
	id := sandboxTestID(0)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"cp", id + ":a", id + ":b"}, "both paths are in a sandbox"},
		{[]string{"cp", "a", "b"}, "neither path is in a sandbox"},
		{[]string{"cp", "a", id + ":../x"}, "must stay inside the sandbox's /work"},
		{[]string{"cp", "a", id + ":a//b"}, "must stay inside the sandbox's /work"},
		{[]string{"cp", "a", id + ":"}, "the path is empty"},
	} {
		h := newInstalledSandboxHarness(t)
		err := h.run(c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want it to say %q", c.args, err, c.want)
		}
		if len(h.connected) != 0 {
			t.Errorf("%v: connected with paths that are refused", c.args)
		}
	}

	// A local path with a colon in it is still local: the sandbox's side
	// is told by an id before the colon.
	h := newInstalledSandboxHarness(t)
	if err := h.run("cp", `C:\dir:name`, id+":b"); err == nil || !strings.Contains(err.Error(), `reading C:\dir:name`) {
		t.Errorf("a local path with a colon: %v", err)
	}
}

func TestSandboxHostRefusesTheVMDriver(t *testing.T) {
	h := newSandboxHarness(t)
	err := h.run("host", "prod", "--driver", "vm")
	for _, want := range []string{"installs the docker driver only", "vm driver is experimental", "pass --driver vm"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want it to say %q", err, want)
		}
	}
	if !errors.Is(err, errSandboxEngineUnavailable) {
		t.Errorf("the refusal is not in the engine-unavailable class: %v", err)
	}
	if len(h.connected) != 0 {
		t.Error("connected for a driver this command cannot install")
	}
}

// In machine mode nothing waits on a prompt: the commands that would ask
// need --yes.
func TestSandboxJSONNeedsYesWhereAPromptWouldBe(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	h.flags.JSON = true
	h.flags.Host = sandboxTestHost
	for _, args := range [][]string{{"rm", sandboxTestID(0)}, {"host", "prod"}} {
		h.out.Reset()
		err := h.run(args...)
		if err == nil || !strings.Contains(err.Error(), "--json needs --yes") {
			t.Errorf("%v: got %v, want the --yes requirement", args, err)
		}
	}
	if h.out.Len() != 0 {
		t.Errorf("stdout held %q", h.out.String())
	}
}

// The CLI targets the docker driver. The vm driver is opted into per
// command, on an engine that runs it.
func TestSandboxDriverChecks(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	err := h.run("new", "--image", "alpine", "--driver", "vm")
	for _, want := range []string{"the vm driver is not available on prod", "runs the docker driver", "`teploy sandbox host` does not install it"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want it to say %q", err, want)
		}
	}
	if !errors.Is(err, errSandboxEngineUnavailable) || len(h.engine.created) != 0 {
		t.Errorf("a sandbox was made on the wrong driver, or the refusal is of no class: %v (%d created)", err, len(h.engine.created))
	}

	h = newInstalledSandboxHarness(t)
	h.engine.driver = "vm"
	err = h.run("new", "--image", "alpine")
	for _, want := range []string{"the sandbox engine on prod runs the experimental vm driver", "Pass --driver vm"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want it to say %q", err, want)
		}
	}
	if len(h.engine.created) != 0 {
		t.Fatal("a sandbox was made on the vm driver without the opt-in")
	}
	if err := h.run("new", "--image", "alpine", "--driver", "vm"); err != nil {
		t.Fatalf("with the opt-in: %v", err)
	}
	// A fork makes a run on the same driver, and needs the opt-in as well.
	if err := h.run("fork", sandboxTestID(0)); err == nil || !strings.Contains(err.Error(), "Pass --driver vm") {
		t.Errorf("fork without the opt-in: %v", err)
	}
	if err := h.run("fork", sandboxTestID(0), "--driver", "vm"); err != nil {
		t.Fatalf("fork with the opt-in: %v", err)
	}

	h = newInstalledSandboxHarness(t)
	h.engine.driver = "gvisor"
	if err := h.run("new", "--image", "alpine"); err == nil || !strings.Contains(err.Error(), `reports a driver this CLI does not know ("gvisor")`) {
		t.Errorf("a driver this CLI does not know: %v", err)
	}
}
