//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// httpService is a busybox service that answers with body on port 8080.
func httpService(body string) string {
	return fmt.Sprintf(`    image: busybox:1.37
    stop_signal: SIGKILL
    command:
      - sh
      - -c
      - 'mkdir -p /www && echo %s > /www/index.html && exec httpd -f -p 8080 -h /www'
`, body)
}

const sleeper = `    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sleep", "3600"]
`

func linkRepo(t *testing.T, services map[string]string) (*repo, string) {
	t.Helper()
	r := newRepo(t, "")
	var b strings.Builder
	b.WriteString("services:\n")
	for _, name := range []string{"web", "api", "db"} {
		if def, ok := services[name]; ok {
			b.WriteString("  " + name + ":\n" + def)
		}
	}
	r.write("compose.yaml", b.String())
	return r, r.commit("Initial")
}

func execIn(container string, args ...string) (string, error) {
	out, err := exec.Command("docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func fetchFrom(container, url string) (string, error) {
	return execIn(container, "wget", "-q", "-T", "3", "-O", "-", url)
}

// TestLinks covers ARCHITECTURE 6.1 against a real daemon: each app calls
// another app's service by a name of its own choosing, the same name means
// different targets for different apps, nothing else is shared, a bare
// service name never resolves to another app, and removing a link or
// leaving the group cuts the connection at once.
func TestLinks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	repoAccounts, shaAccounts := linkRepo(t, map[string]string{"web": httpService("accounts-web"), "api": httpService("accounts-api"), "db": sleeper})
	repoBilling, shaBilling := linkRepo(t, map[string]string{"web": httpService("billing-web"), "api": httpService("billing-api")})
	repoShop, shaShop := linkRepo(t, map[string]string{"web": httpService("shop-web"), "db": sleeper})
	accounts, billing, shop := e.slug("ln-acc"), e.slug("ln-bil"), e.slug("ln-shop")
	e.createApp(accounts, repoAccounts.url())
	e.createApp(billing, repoBilling.url())
	e.createApp(shop, repoShop.url())

	var group struct {
		ID int64 `json:"id"`
	}
	if status := e.call(http.MethodPost, "/api/groups", map[string]any{"name": "it-" + e.suffix, "environment": "staging"}, &group); status != http.StatusCreated {
		t.Fatalf("create group: %d", status)
	}
	expose := func(slug string, services ...string) {
		t.Helper()
		if status := e.call(http.MethodPut, "/api/apps/"+slug+"/group", map[string]any{"group_id": group.ID, "services": services}, nil); status != http.StatusOK {
			t.Fatalf("join %s: %d", slug, status)
		}
	}
	type linkInfo struct {
		ID      int64  `json:"id"`
		Network string `json:"network"`
	}
	var networks []string
	t.Cleanup(func() {
		for _, n := range networks {
			_ = exec.Command("docker", "network", "rm", n).Run()
		}
	})
	link := func(slug, name, target, service string, services ...string) linkInfo {
		t.Helper()
		var l linkInfo
		if status := e.call(http.MethodPost, "/api/apps/"+slug+"/links", map[string]any{
			"name": name, "target_app": target, "target_service": service, "services": services,
		}, &l); status != http.StatusCreated {
			t.Fatalf("link %s %s -> %s/%s: %d", slug, name, target, service, status)
		}
		networks = append(networks, l.Network)
		return l
	}
	expose(accounts, "api", "db")
	expose(billing, "api")
	expose(shop)
	shopAuth := link(shop, "auth", accounts, "api", "web")
	link(shop, "accounts-db", accounts, "db", "web")
	link(accounts, "auth", billing, "api", "web")

	for slug, sha := range map[string]string{accounts: shaAccounts, billing: shaBilling, shop: shaShop} {
		if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
			t.Fatalf("deploy %s %s: %s\n%s", slug, d.Status, d.Error, e.logText(d.ID))
		}
	}

	shopWeb := "koroku-" + shop + "-web-1"
	accountsWeb := "koroku-" + accounts + "-web-1"
	// The same name, a different target for each app.
	if out, err := fetchFrom(shopWeb, "http://auth:8080/"); err != nil || out != "accounts-api" {
		t.Errorf("shop's web -> auth: %q, %v", out, err)
	}
	if out, err := fetchFrom(accountsWeb, "http://auth:8080/"); err != nil || out != "billing-api" {
		t.Errorf("accounts' web -> auth: %q, %v", out, err)
	}
	// Nothing else is reachable: billing's containers from shop, the bare
	// service names of the targets, accounts' web from shop.
	for _, name := range []string{"koroku-" + billing + "-api-1", "api", "koroku-" + accounts + "-web-1"} {
		if out, err := execIn(shopWeb, "ping", "-c", "1", "-W", "2", name); err == nil {
			t.Errorf("shop's web reaches %s:\n%s", name, out)
		}
	}
	// With shop's own db stopped, "db" must not resolve to accounts' db,
	// even though shop is linked to it (as accounts-db).
	if out, err := exec.Command("docker", "stop", "koroku-"+shop+"-db-1").CombinedOutput(); err != nil {
		t.Fatalf("stop shop's db: %v\n%s", err, out)
	}
	if out, err := execIn(shopWeb, "ping", "-c", "1", "-W", "2", "db"); err == nil {
		t.Errorf("with shop's db stopped, \"db\" still resolves from shop's web:\n%s", out)
	}
	if _, err := execIn(shopWeb, "ping", "-c", "1", "-W", "2", "accounts-db"); err != nil {
		t.Errorf("shop's web cannot reach accounts' db through its link: %v", err)
	}

	// The link networks are internal.
	internal, err := exec.Command("docker", "network", "inspect", "--format", "{{.Internal}} {{index .Labels \"io.koroku.link\"}}", shopAuth.Network).Output()
	if err != nil || strings.TrimSpace(string(internal)) != fmt.Sprintf("true %d", shopAuth.ID) {
		t.Errorf("link network: %q, %v", internal, err)
	}

	// Deleting a link cuts it without a deploy, and removes its network.
	if status := e.call(http.MethodDelete, fmt.Sprintf("/api/apps/%s/links/%d", shop, shopAuth.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete link: %d", status)
	}
	if out, err := fetchFrom(shopWeb, "http://auth:8080/"); err == nil {
		t.Errorf("shop's web still reaches auth after the link was deleted: %q", out)
	}
	if err := exec.Command("docker", "network", "inspect", shopAuth.Network).Run(); err == nil {
		t.Errorf("the link network is still there")
	}

	// billing leaving the group removes accounts' link to it.
	if status := e.call(http.MethodPut, "/api/apps/"+billing+"/group", map[string]any{"group_id": nil, "services": []string{}}, nil); status != http.StatusOK {
		t.Fatalf("leave: %d", status)
	}
	if out, err := fetchFrom(accountsWeb, "http://auth:8080/"); err == nil {
		t.Errorf("accounts' web still reaches billing after billing left: %q", out)
	}
	var links struct {
		Links []linkInfo `json:"links"`
	}
	e.call(http.MethodGet, "/api/apps/"+accounts+"/links", nil, &links)
	if len(links.Links) != 0 {
		t.Errorf("accounts still has links: %+v", links.Links)
	}

	// A restart of the target keeps the remaining link working.
	if out, err := exec.Command("docker", "restart", "koroku-"+accounts+"-db-1").CombinedOutput(); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if _, err := execIn(shopWeb, "ping", "-c", "1", "-W", "2", "accounts-db"); err != nil {
		t.Errorf("the link is gone after restarting the target: %v", err)
	}
}
