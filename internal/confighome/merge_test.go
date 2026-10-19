package confighome

import (
	"strings"
	"testing"
)

func TestMerge3KeyLevel(t *testing.T) {
	base := []byte("app: blog\nimage: nginx:1\nenv:\n  A: \"1\"\n  B: \"1\"\n")
	mine := []byte("app: blog\nimage: nginx:2\nenv:\n  A: \"2\"\n  B: \"1\"\n")
	theirs := []byte("app: blog\nimage: nginx:1\ndomain: blog.example.com\nenv:\n  A: \"1\"\n  B: \"3\"\n")
	merged, conflicts, err := Merge3(base, mine, theirs)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("conflicts=%v err=%v", conflicts, err)
	}
	for path, want := range map[string]string{"image": "nginx:2", "domain": "blog.example.com", "env.A": "2", "env.B": "3"} {
		if v, _ := GetPath(merged, path); v != want {
			t.Errorf("%s = %v, want %s", path, v, want)
		}
	}
}

func TestMerge3ReportsOverlap(t *testing.T) {
	base := []byte("app: blog\nimage: nginx:1\n")
	mine := []byte("app: blog\nimage: nginx:2\n")
	theirs := []byte("app: blog\nimage: nginx:3\n")
	merged, conflicts, err := Merge3(base, mine, theirs)
	if err != nil || len(conflicts) != 1 || conflicts[0] != "image" {
		t.Fatalf("conflicts=%v err=%v", conflicts, err)
	}
	resolved, err := SetPath(merged, "image", "nginx:3", false)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := GetPath(resolved, "image"); v != "nginx:3" {
		t.Fatalf("resolved image = %v", v)
	}
}

func TestDiff(t *testing.T) {
	d := Diff("a", "b", []byte("x\ny\nz\n"), []byte("x\nY\nz\n"))
	if !strings.Contains(d, "-y\n") || !strings.Contains(d, "+Y\n") || !strings.Contains(d, " x\n") {
		t.Fatalf("diff:\n%s", d)
	}
	if Diff("a", "b", []byte("same\n"), []byte("same")) != "" {
		t.Fatal("equal inputs must diff empty")
	}
}

func TestEditPathKeepsCommentsAndOrder(t *testing.T) {
	doc := []byte("# blog\napp: blog\nimage: nginx:1 # pinned\nenv:\n  A: \"1\"\n")
	v := `"nginx:2"`
	out, err := EditPath(doc, "image", &v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# blog\napp: blog\nimage: \"nginx:2\" # pinned\n") {
		t.Fatalf("comments/order not kept:\n%s", out)
	}
	b := "debug"
	if out, err = EditPath(out, "env.LOG_LEVEL", &b); err != nil {
		t.Fatal(err)
	}
	if out, err = EditPath(out, "env.A", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetPath(out, "env.LOG_LEVEL"); got != "debug" {
		t.Fatalf("env.LOG_LEVEL = %v\n%s", got, out)
	}
	if _, ok := GetPath(out, "env.A"); ok {
		t.Fatalf("env.A not removed:\n%s", out)
	}
	if _, err := EditPath(out, "image.tag", &b); err == nil {
		t.Fatal("setting under a scalar must fail")
	}
}

func TestChangedKeys(t *testing.T) {
	got := ChangedKeys([]byte("app: a\nimage: x:1\nenv:\n  A: 1\n  B: 2\n"), []byte("app: a\nimage: x:2\nenv:\n  A: 1\n  C: 3\ndomain: d.example.com\n"))
	want := "domain,env.B,env.C,image"
	if strings.Join(got, ",") != want {
		t.Fatalf("ChangedKeys = %v, want %s", got, want)
	}
}
