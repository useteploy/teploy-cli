package backup

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func homeMock(targetRepo string) *ssh.MockExecutor {
	mock := ssh.NewMockExecutor("1.2.3.4")
	if targetRepo != "" {
		repo := "/deployments/.teploy/apps/blog/config.git"
		mock.Files[repo+"/HEAD"] = []byte("ref: refs/heads/teploy\n")
		mock.Files[repo+"/refs/heads/teploy"] = []byte(targetRepo + "\n")
	}
	return mock
}

func homeManifest(home, commit string) *BundleManifest {
	m := &BundleManifest{App: "blog", State: []byte(`{"schema_version":3,"generation":2,"current_hash":"v1"}`)}
	if home != "" {
		m.ConfigHome = &ConfigHomeRecord{Home: home, Commit: commit, Member: ConfigHomeMember}
	}
	return m
}

func TestResolveCutoverHome(t *testing.T) {
	cases := []struct {
		name                     string
		bundleHome, bundleCommit string
		targetRepo               string
		targetHasState           bool
		flag                     string
		final                    string
		install, archive         bool
		refused                  string
	}{
		{name: "fresh target takes the bundle's teploy home", bundleHome: "teploy", bundleCommit: headA, final: "teploy", install: true},
		{name: "fresh target, v1 bundle is folder", final: "folder"},
		{name: "same teploy head in place", bundleHome: "teploy", bundleCommit: headA, targetRepo: headA, targetHasState: true, final: "teploy"},
		{name: "teploy head moved needs a choice", bundleHome: "teploy", bundleCommit: headA, targetRepo: headB, targetHasState: true, refused: "config home mismatch"},
		{name: "bundle wins over a moved head", bundleHome: "teploy", bundleCommit: headA, targetRepo: headB, targetHasState: true, flag: "bundle", final: "teploy", install: true, archive: true},
		{name: "keep the target's config", bundleHome: "teploy", bundleCommit: headA, targetRepo: headB, targetHasState: true, flag: "current", final: "teploy"},
		{name: "folder bundle over a teploy target needs a choice", bundleHome: "folder", targetRepo: headB, targetHasState: true, refused: "--home=bundle"},
		{name: "folder bundle wins: target config archived", bundleHome: "folder", targetRepo: headB, targetHasState: true, flag: "bundle", final: "folder", archive: true},
		{name: "teploy bundle over an existing folder app needs a choice", bundleHome: "teploy", bundleCommit: headA, targetHasState: true, refused: "mismatch"},
		{name: "bad flag", flag: "both", refused: "--home must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(homeMock(tc.targetRepo), &bytes.Buffer{})
			got, err := c.resolveCutoverHome(context.Background(), homeManifest(tc.bundleHome, tc.bundleCommit), BundleRestoreOptions{App: "blog", Home: tc.flag}, tc.targetHasState)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("got %+v, %v; want refusal %q", got, err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.final != tc.final || got.installConfig != tc.install || got.archiveTarget != tc.archive {
				t.Fatalf("got %+v, want final=%s install=%v archive=%v", got, tc.final, tc.install, tc.archive)
			}
		})
	}
}

func TestBundleManifestSchemaVersions(t *testing.T) {
	for _, v := range []string{"1", "2"} {
		if _, err := ParseBundleManifest([]byte(`{"schema_version":` + v + `,"kind":"dr","id":"20260919-120000","app":"blog"}`)); err != nil {
			t.Fatalf("schema %s must parse: %v", v, err)
		}
	}
	if _, err := ParseBundleManifest([]byte(`{"schema_version":3,"kind":"dr","id":"20260919-120000","app":"blog"}`)); err == nil {
		t.Fatal("a newer schema must be refused")
	}
	if _, err := ParseBundleManifest([]byte(`{"schema_version":2,"kind":"dr","id":"20260919-120000","app":"blog","config_home":{"home":"teploy"}}`)); err == nil {
		t.Fatal("a teploy-home bundle without its config member must be refused")
	}
	if _, err := ParseBundleManifest([]byte(`{"schema_version":2,"kind":"dr","id":"20260919-120000","app":"blog","config_home":{"home":"teploy","member":"../../etc/x"}}`)); err == nil {
		t.Fatal("an arbitrary config member path must be refused")
	}
}
