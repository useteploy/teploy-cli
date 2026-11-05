package caddy

import (
	"reflect"
	"testing"
)

const inspectFixture = `{
	email ops@teploy-fixture.dev
}

# TEPLOY BEGIN blog
# TEPLOY GENERATION 4
blog.dev, www.blog.dev {
	# TEPLOY WEBHOOK BEGIN blog
	@teploy_hook_blog {
		method POST
		path /teploy-webhook/blog
	}
	handle @teploy_hook_blog {
		reverse_proxy host.docker.internal:9999
	}
	# TEPLOY WEBHOOK END blog
	reverse_proxy blog-web-abc:3000
}
# TEPLOY END blog

# TEPLOY BEGIN lb
lb.dev {
	reverse_proxy {
		to lb-web-a-1:8080 lb-web-a-2:8080
		health_uri /health
	}
}
# TEPLOY END lb

# TEPLOY BEGIN internal-app
private.dev {
	tls internal
	reverse_proxy http://internal-app-web-a:80
}
# TEPLOY END internal-app

http://plain.dev, http://plain2.dev:80 {
	reverse_proxy 127.0.0.1:3000
}

custom.dev {
	tls /etc/caddy/tls/c.pem /etc/caddy/tls/k.pem
	file_server
}

:8080 {
	respond "hello"
}
`

func TestInspectRoutes(t *testing.T) {
	sites, err := InspectRoutes(inspectFixture)
	if err != nil {
		t.Fatalf("InspectRoutes: %v", err)
	}
	byHost := map[string]RouteSite{}
	for _, s := range sites {
		for _, h := range s.Hosts {
			byHost[h] = s
		}
	}
	cases := []struct {
		host, owner, tls string
		upstreams        []string
	}{
		{"blog.dev", "blog", SiteTLSACME, []string{"blog-web-abc:3000"}},
		{"www.blog.dev", "blog", SiteTLSACME, []string{"blog-web-abc:3000"}},
		{"lb.dev", "lb", SiteTLSACME, []string{"lb-web-a-1:8080", "lb-web-a-2:8080"}},
		{"private.dev", "internal-app", SiteTLSInternal, []string{"internal-app-web-a:80"}},
		{"plain.dev", "", SiteTLSHTTPOnly, []string{"127.0.0.1:3000"}},
		{"plain2.dev", "", SiteTLSHTTPOnly, []string{"127.0.0.1:3000"}},
		{"custom.dev", "", SiteTLSCustom, nil},
	}
	for _, c := range cases {
		s, ok := byHost[c.host]
		if !ok {
			t.Errorf("%s: no site", c.host)
			continue
		}
		if s.Owner != c.owner || s.TLS != c.tls || !reflect.DeepEqual(s.Upstreams, c.upstreams) {
			t.Errorf("%s: got owner=%q tls=%q upstreams=%v", c.host, s.Owner, s.TLS, s.Upstreams)
		}
	}
	if _, ok := byHost[""]; ok {
		t.Error("an address with no host must not produce a host")
	}
}

func TestInspectRoutes_UnreadableFileIsAnError(t *testing.T) {
	for name, content := range map[string]string{
		"unterminated region": "# TEPLOY BEGIN a\na.dev {\n}\n",
		"top-level import":    "import sites/*\n",
		"unbalanced":          "a.dev {\n",
	} {
		if _, err := InspectRoutes(content); err == nil {
			t.Errorf("%s: a half-read route view must be an error", name)
		}
	}
}

func TestRegionHosts(t *testing.T) {
	got := RegionHosts("# TEPLOY BEGIN blog\n# TEPLOY GENERATION 2\nBlog.dev, https://www.blog.dev {\n\treverse_proxy a:1\n}\n# TEPLOY END blog")
	if !reflect.DeepEqual(got, []string{"blog.dev", "www.blog.dev"}) {
		t.Errorf("RegionHosts = %v", got)
	}
}
