package docker

// InspectSiblings (Programme B sibling discovery): the batched second-round
// read — container port binds and image labels in one command, parsed by the
// real decoder over a mock executor.

import (
	"context"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestInspectSiblings_PortsAndImagesOneCommand(t *testing.T) {
	mock := ssh.NewMockExecutor("203.0.113.7",
		ssh.MockCommand{Match: "docker inspect --format", Output: "/observe-web-7cf98cae\t" +
			`{"3000/tcp":[{"HostIp":"100.108.123.49","HostPort":"3000"},{"HostIp":"::","HostPort":"3000"}],"9229/tcp":null}` + "\n" +
			"/forgejo\t" + `{}` + "\n" +
			"\n__TEPLOY_SIBLINGS_IMAGES__\n" +
			`sha256:1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b` + "\t" +
			`["ghcr.io/useteploy/observe:7cf98cae"]` + "\t" +
			`{"dev.teploy.product":"observe","org.opencontainers.image.version":"1.4.0"}` + "\n" +
			`sha256:9f8e7d6c5b4a9f8e7d6c5b4a9f8e7d6c5b4a9f8e7d6c5b4a9f8e7d6c5b4a9f8e` + "\t" +
			`["codeberg.org/forgejo/forgejo:11"]` + "\t" +
			`{"org.opencontainers.image.source":"https://codeberg.org/forgejo/forgejo"}` + "\n"},
	)
	got, err := NewClient(mock).InspectSiblings(context.Background(),
		[]string{"observe-web-7cf98cae", "forgejo"},
		[]string{"ghcr.io/useteploy/observe:7cf98cae"})
	if err != nil {
		t.Fatalf("InspectSiblings: %v", err)
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("the batched read must be ONE command, got %d: %v", len(mock.Calls), mock.Calls)
	}
	observe := got.Ports["observe-web-7cf98cae"]
	if len(observe) != 2 {
		t.Fatalf("observe port binds = %+v, want the tailnet and v6 binds", observe)
	}
	if observe[0] != (PortBinding{HostIP: "100.108.123.49", HostPort: 3000, ContainerPort: 3000, Proto: "tcp"}) {
		t.Errorf("observe first bind = %+v", observe[0])
	}
	if len(got.Ports["forgejo"]) != 0 {
		t.Errorf("forgejo publishes nothing: %+v", got.Ports["forgejo"])
	}
	// The unpublished 9229/tcp (null bindings) yields no hint — only host
	// binds count.
	for _, b := range observe {
		if b.ContainerPort == 9229 {
			t.Errorf("an exposed-but-unpublished port is not a hint: %+v", b)
		}
	}

	byRef, ok := got.Images["ghcr.io/useteploy/observe:7cf98cae"]
	if !ok || byRef.Labels["dev.teploy.product"] != "observe" || byRef.Labels["org.opencontainers.image.version"] != "1.4.0" {
		t.Errorf("image facts by requested ref = %+v", byRef)
	}
	byID, ok := got.Images["sha256:1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b"]
	if !ok || byID.ID != byRef.ID {
		t.Errorf("image facts must also key by full ID: %+v", byID)
	}
}

func TestInspectSiblings_ShortIDRefResolves(t *testing.T) {
	mock := ssh.NewMockExecutor("203.0.113.7",
		ssh.MockCommand{Match: "docker image inspect", Output: "__TEPLOY_SIBLINGS_IMAGES__\n" +
			`sha256:1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b` + "\t" +
			`["ghcr.io/useteploy/dash:v0.3.1"]` + "\t" +
			`{"dev.teploy.product":"dash"}` + "\n"},
	)
	got, err := NewClient(mock).InspectSiblings(context.Background(), nil,
		[]string{"1a2b3c4d5e6f"})
	if err != nil {
		t.Fatalf("InspectSiblings: %v", err)
	}
	facts, ok := got.Images["1a2b3c4d5e6f"]
	if !ok || facts.Labels["dev.teploy.product"] != "dash" {
		t.Fatalf("docker ps's 12-hex image form must resolve to the image's labels: %+v", got.Images)
	}
	if len(got.Images) != 2 { // the short ref and the full ID
		t.Errorf("image keys = %v", keysOf(got.Images))
	}
}

func TestInspectSiblings_MissingObjectsAreSkipped(t *testing.T) {
	// One container vanished between the ps read and the inspect, and one
	// image is gone: the found objects still answer.
	mock := ssh.NewMockExecutor("203.0.113.7",
		ssh.MockCommand{Match: "docker inspect --format", Output: "/caddy\t{}\n" +
			"\n__TEPLOY_SIBLINGS_IMAGES__\n"},
	)
	got, err := NewClient(mock).InspectSiblings(context.Background(),
		[]string{"caddy", "vanished"}, []string{"gone:tag"})
	if err != nil {
		t.Fatalf("InspectSiblings: %v", err)
	}
	if _, ok := got.Ports["vanished"]; ok {
		t.Errorf("a vanished container must be absent, not fabricated")
	}
	if len(got.Ports["caddy"]) != 0 {
		t.Errorf("caddy = %+v", got.Ports["caddy"])
	}
	if len(got.Images) != 0 {
		t.Errorf("a vanished image must be absent: %+v", got.Images)
	}
}

func TestInspectSiblings_NothingAsked(t *testing.T) {
	mock := ssh.NewMockExecutor("203.0.113.7")
	got, err := NewClient(mock).InspectSiblings(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("InspectSiblings with nothing to ask: %v", err)
	}
	if len(mock.Calls) != 0 {
		t.Errorf("no containers means no second round trip: %v", mock.Calls)
	}
	if got.Ports == nil || got.Images == nil {
		t.Errorf("empty maps, not nils: %+v", got)
	}
}

func TestInspectSiblings_UDPAndLoopbackBinds(t *testing.T) {
	mock := ssh.NewMockExecutor("203.0.113.7",
		ssh.MockCommand{Match: "docker inspect --format", Output: "/dns\t" +
			`{"53/udp":[{"HostIp":"127.0.0.1","HostPort":"53"}]}` + "\n"},
	)
	got, err := NewClient(mock).InspectSiblings(context.Background(), []string{"dns"}, nil)
	if err != nil {
		t.Fatalf("InspectSiblings: %v", err)
	}
	binds := got.Ports["dns"]
	if len(binds) != 1 || binds[0] != (PortBinding{HostIP: "127.0.0.1", HostPort: 53, ContainerPort: 53, Proto: "udp"}) {
		t.Errorf("udp loopback bind = %+v", binds)
	}
}

func keysOf(m map[string]ImageFacts) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
