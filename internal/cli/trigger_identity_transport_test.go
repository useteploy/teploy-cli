package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepositoryMappingIgnoresCheckoutTransport(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/5" || r.Header.Get("PRIVATE-TOKEN") != "tok" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":5,"http_url_to_repo":"http://gitlab.lan:8080/group/app.git"}`))
	}))
	defer api.Close()
	t.Setenv("TEPLOY_TRIGGER_API_BASE", api.URL)
	t.Setenv("TEPLOY_TRIGGER_FORGE", "gitlab")
	t.Setenv("TEPLOY_TRIGGER_ALLOW_HTTP", "1")
	t.Setenv("TEPLOY_TRIGGER_API_TOKEN", "tok")
	identity, _ := json.Marshal([]string{"gitlab", api.URL, "id:5"})

	check := func(remote string) error {
		clone, err := canonicalRepository(remote)
		if err != nil {
			t.Fatal(err)
		}
		return verifyRepositoryMapping(context.Background(), string(identity), clone, http.DefaultTransport)
	}
	// The checkout uses SSH on the forge's SSH port; the API reports HTTP.
	if err := check("ssh://git@gitlab.lan:2222/group/app.git"); err != nil {
		t.Fatalf("ssh checkout of the same repository refused: %v", err)
	}
	if err := check("ssh://git@gitlab.lan:2222/group/other.git"); err == nil {
		t.Fatal("checkout of another repository accepted")
	}
	if err := check("ssh://git@elsewhere.lan:2222/group/app.git"); err == nil {
		t.Fatal("checkout on another host accepted")
	}
}
