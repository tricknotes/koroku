package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeContainers plays the docker commands the link code runs.
type fakeContainers struct {
	mu sync.Mutex
	// projects lists the containers per Compose project as
	// "<id>\t<name>\t<service>\t<networks>" lines.
	projects map[string][]string
	// networks is what `docker network ls` lists.
	networks []string
	// missing networks fail `docker network inspect`.
	missing map[string]bool
	// members are the container IDs `docker network inspect` lists.
	members map[string][]string
}

func (f *fakeContainers) set(project string, lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.projects == nil {
		f.projects = map[string][]string{}
	}
	f.projects[project] = lines
}

func (f *fakeContainers) answer(args []string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case args[0] == "ps":
		for _, arg := range args {
			if project, ok := strings.CutPrefix(arg, "label=com.docker.compose.project="); ok {
				return strings.Join(f.projects[project], "\n"), true, nil
			}
		}
	case args[0] == "network" && args[1] == "ls":
		return strings.Join(f.networks, "\n"), true, nil
	case args[0] == "network" && args[1] == "inspect":
		network := args[len(args)-1]
		if f.missing[network] {
			return "", true, errors.New("no such network")
		}
		if strings.Contains(strings.Join(args, " "), "range") {
			return strings.Join(f.members[network], " "), true, nil
		}
	}
	return "", false, nil
}

func newLinkHarness(t *testing.T) (*harness, *fakeContainers) {
	h := newHarness(t, nil)
	fc := &fakeContainers{missing: map[string]bool{}, members: map[string][]string{}}
	h.mu.Lock()
	h.dockerOutput = fc.answer
	h.mu.Unlock()
	return h, fc
}

func (h *harness) group(name string) store.Group {
	h.t.Helper()
	ctx := context.Background()
	groups, _ := h.store.ListGroups(ctx)
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	g, err := h.store.CreateGroup(ctx, name, store.EnvironmentStaging, h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return g
}

func (h *harness) expose(app store.App, group string, services ...string) {
	h.t.Helper()
	g := h.group(group)
	if _, err := h.store.SetAppGroup(context.Background(), app.ID, &g.ID, services, h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) link(consumer store.App, name string, target store.App, service string, services ...string) store.Link {
	h.t.Helper()
	l, err := h.store.CreateLink(context.Background(), consumer.ID, name, target.ID, service, services, h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

func (h *harness) network(l store.Link) string {
	h.t.Helper()
	id, err := h.store.InstallationID(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return l.Network(id)
}

func (h *harness) calls(prefix ...string) []string {
	var out []string
	for _, c := range h.runner.Calls() {
		if c.Name == "docker" && len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix) {
			out = append(out, strings.Join(c.Args, " "))
		}
	}
	return out
}

// upCalls returns the arguments after "up" of each compose up call.
func (h *harness) upCalls() []string {
	var out []string
	for _, c := range h.runner.Calls() {
		if c.Name != "docker" {
			continue
		}
		if sub, _ := composeSubcommand(c.Args); sub == "up" {
			i := slices.Index(c.Args, "up")
			out = append(out, strings.Join(c.Args[i:], " "))
		}
	}
	return out
}

// linkedPair makes "accounts" expose web and "shop" link "auth" to it.
func linkedPair(h *harness) (accounts, shop store.App, l store.Link) {
	accounts = h.createApp("accounts")
	shop = h.createApp("shop")
	h.expose(accounts, "team", "web")
	h.expose(shop, "team")
	l = h.link(h.reload(shop.Slug), "auth", h.reload(accounts.Slug), "web", "web")
	return h.reload(accounts.Slug), h.reload(shop.Slug), l
}

func TestDeployAttachesTheConsumer(t *testing.T) {
	h, fc := newLinkHarness(t)
	h.start()
	_, _, l := linkedPair(h)
	network := h.network(l)
	fc.missing[network] = true
	fc.set("koroku-shop",
		"c-web\tkoroku-shop-web-1\tweb\tkoroku-shop_default",
		"c-db\tkoroku-shop-db-1\tdb\tkoroku-shop_default,"+network+",koroku_link_otherins_1")

	d := h.deploy("shop", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("status = %s: %s\n%s", d.Status, d.Error, h.log("shop", d.ID))
	}
	ups := h.upCalls()
	if len(ups) != 2 || !strings.HasPrefix(ups[0], "up --no-start") || !strings.Contains(ups[1], "--wait") {
		t.Errorf("up calls = %v, want create then start", ups)
	}
	// The consumer's service joins without a name.
	if got := h.calls("network", "connect"); !slices.Equal(got, []string{"network connect " + network + " c-web"}) {
		t.Errorf("connect calls = %v", got)
	}
	// db does not use the link; another installation's network is left alone.
	if got := h.calls("network", "disconnect"); !slices.Equal(got, []string{"network disconnect --force " + network + " c-db"}) {
		t.Errorf("disconnect calls = %v", got)
	}
	created := h.calls("network", "create")
	if len(created) != 1 || !strings.Contains(created[0], "--internal") || !strings.Contains(created[0], "io.koroku.link=1") {
		t.Errorf("network create calls = %v", created)
	}
	var order []string
	for _, c := range h.runner.Calls() {
		if sub, _ := composeSubcommand(c.Args); sub == "up" {
			order = append(order, "up")
		} else if len(c.Args) > 1 && c.Args[1] == "connect" {
			order = append(order, "connect")
		}
	}
	if strings.Join(order, ",") != "up,connect,up" {
		t.Errorf("order = %v", order)
	}
}

func TestDeployAttachesTheTargetUnderTheLinkName(t *testing.T) {
	h, fc := newLinkHarness(t)
	h.start()
	_, _, l := linkedPair(h)
	network := h.network(l)
	fc.set("koroku-accounts", "a-web\tkoroku-accounts-web-1\tweb\tkoroku-accounts_default")

	if d := h.deploy("accounts", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", d.Error)
	}
	if got := h.calls("network", "connect"); !slices.Equal(got, []string{"network connect --alias auth " + network + " a-web"}) {
		t.Errorf("connect calls = %v", got)
	}
}

// A rollback to a kept release recreates containers too, so it attaches the
// links like a deploy (ARCHITECTURE 6.1, 7.1).
func TestRollbackAttachesTheConsumer(t *testing.T) {
	h, fc := newLinkHarness(t)
	h.start()
	_, _, l := linkedPair(h)
	network := h.network(l)
	fc.set("koroku-shop", "c-web\tkoroku-shop-web-1\tweb\tkoroku-shop_default")

	first := h.deploy("shop", shaA)
	second := h.deploy("shop", shaB)
	if first.Status != store.StatusSucceeded || second.Status != store.StatusSucceeded {
		t.Fatalf("deploys: %s %s", first.Status, second.Status)
	}

	h.runner.Reset()
	rb := h.rollback("shop", first.ID)
	if rb.Status != store.StatusSucceeded {
		t.Fatalf("rollback %s: %s\n%s", rb.Status, rb.Error, h.log("shop", rb.ID))
	}
	ups := h.upCalls()
	if len(ups) != 2 || !strings.HasPrefix(ups[0], "up --no-start") || !strings.Contains(ups[1], "--wait") {
		t.Errorf("up calls = %v, want create then start", ups)
	}
	if got := h.calls("network", "connect"); !slices.Equal(got, []string{"network connect " + network + " c-web"}) {
		t.Errorf("connect calls = %v", got)
	}
}

// A kept release that now breaks a link rule is not started again.
func TestRollbackRefusesLinkProblems(t *testing.T) {
	h, _ := newLinkHarness(t)
	h.start()
	shop := h.createApp("shop")
	first := h.deploy("shop", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", first.Error)
	}
	h.expose(h.reload(shop.Slug), "team", "cache")

	h.runner.Reset()
	rb := h.rollback("shop", first.ID)
	if rb.Status != store.StatusFailed {
		t.Fatalf("status = %s, want failed", rb.Status)
	}
	if log := h.log("shop", rb.ID); !strings.Contains(log, "[link_service]") {
		t.Errorf("log lacks the violation:\n%s", log)
	}
	if len(h.upCalls()) != 0 {
		t.Errorf("containers were started: %v", h.upCalls())
	}
}

func TestDeployRefusesLinkProblems(t *testing.T) {
	cases := map[string]func(h *harness){
		// A link named like one of the app's own services.
		"name shadows a service": func(h *harness) {
			accounts, shop := h.createApp("accounts"), h.createApp("shop")
			h.expose(accounts, "team", "web")
			h.expose(shop, "team")
			h.link(h.reload(shop.Slug), "db", h.reload(accounts.Slug), "web", "web")
		},
		// The app exposes a service its Compose file does not have.
		"exposed service missing": func(h *harness) {
			h.expose(h.createApp("shop"), "team", "cache")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newLinkHarness(t)
			h.start()
			setup(h)
			d := h.deploy("shop", shaA)
			if d.Status != store.StatusFailed {
				t.Fatalf("status = %s, want failed", d.Status)
			}
			if log := h.log("shop", d.ID); !strings.Contains(log, "[link_service]") {
				t.Errorf("log lacks the violation:\n%s", log)
			}
			if len(h.upCalls()) != 0 {
				t.Errorf("containers were started: %v", h.upCalls())
			}
		})
	}
}

func TestStartAfterDeletingTheLink(t *testing.T) {
	h, fc := newLinkHarness(t)
	h.start()
	_, shop, l := linkedPair(h)
	if d := h.deploy("shop", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", d.Error)
	}
	shop = h.reload(shop.Slug)
	network := h.network(l)
	fc.members[network] = []string{"c-web"}

	ctx := context.Background()
	h.runner.Reset()
	if err := h.engine.DeleteLink(ctx, shop, l.ID); err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}
	if got := h.calls("network", "disconnect"); !slices.Equal(got, []string{"network disconnect --force " + network + " c-web"}) {
		t.Errorf("disconnect calls = %v", got)
	}
	if got := h.calls("network", "rm"); !slices.Equal(got, []string{"network rm " + network}) {
		t.Errorf("rm calls = %v", got)
	}

	h.runner.Reset()
	if err := h.engine.Start(ctx, shop); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ups := h.upCalls(); len(ups) != 1 || strings.Contains(ups[0], "--no-start") {
		t.Errorf("up calls after deleting the link = %v", ups)
	}
}

func TestCreateLink(t *testing.T) {
	h, fc := newLinkHarness(t)
	h.start()
	ctx := context.Background()
	accounts, shop := h.createApp("accounts"), h.createApp("shop")
	if d := h.deploy("shop", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", d.Error)
	}
	h.expose(accounts, "team", "web")
	h.expose(shop, "team")
	accounts, shop = h.reload(accounts.Slug), h.reload(shop.Slug)
	fc.set("koroku-accounts", "a-web\tkoroku-accounts-web-1\tweb\tkoroku-accounts_default")

	// The deployed consumer has a db: a link named db would shadow it.
	var linkErr *LinkServiceError
	if _, err := h.engine.CreateLink(ctx, shop, "db", accounts, "web", []string{"web"}); !errors.As(err, &linkErr) {
		t.Errorf("shadowing name: err = %v", err)
	}
	if _, err := h.engine.CreateLink(ctx, shop, "auth", accounts, "web", []string{"sidecar"}); !errors.As(err, &linkErr) {
		t.Errorf("unknown consumer service: err = %v", err)
	}
	if _, err := h.engine.CreateLink(ctx, shop, "auth", accounts, "db", []string{"web"}); !errors.Is(err, store.ErrLinkTarget) {
		t.Errorf("service not exposed: err = %v", err)
	}

	h.runner.Reset()
	l, err := h.engine.CreateLink(ctx, shop, "auth", accounts, "web", []string{"web"})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// The target is attached at once, under the link's name.
	if got := h.calls("network", "connect"); !slices.Equal(got, []string{"network connect --alias auth " + h.network(l) + " a-web"}) {
		t.Errorf("connect calls = %v", got)
	}
}

func TestSetGroupRemovesLinks(t *testing.T) {
	h, fc := newLinkHarness(t)
	h.start()
	ctx := context.Background()
	accounts, _, l := linkedPair(h)
	network := h.network(l)
	fc.members[network] = []string{"a-web", "c-web"}

	g := h.group("team")
	if err := h.engine.SetGroup(ctx, accounts, &g.ID, []string{"db"}); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if got := h.calls("network", "rm"); !slices.Equal(got, []string{"network rm " + network}) {
		t.Errorf("rm calls = %v", got)
	}
	if len(h.calls("network", "disconnect")) != 2 {
		t.Errorf("disconnect calls = %v", h.calls("network", "disconnect"))
	}
	if links, _ := h.store.ListLinks(ctx); len(links) != 0 {
		t.Errorf("links left: %+v", links)
	}

	production, _ := h.store.CreateGroup(ctx, "live", store.EnvironmentProduction, h.clock.Now())
	if err := h.engine.SetGroup(ctx, h.reload(accounts.Slug), &production.ID, nil); !errors.Is(err, store.ErrGroupEnvironment) {
		t.Errorf("production group: err = %v", err)
	}
}

func TestDeleteAppRemovesLinkNetworks(t *testing.T) {
	h, _ := newLinkHarness(t)
	h.start()
	accounts, _, l := linkedPair(h)
	if err := h.engine.Delete(context.Background(), accounts, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := h.calls("network", "rm"); !slices.Equal(got, []string{"network rm " + h.network(l)}) {
		t.Errorf("rm calls = %v", got)
	}
}

func TestReconcileLinks(t *testing.T) {
	h, fc := newLinkHarness(t)
	ctx := context.Background()
	_, _, l := linkedPair(h)
	network := h.network(l)
	installation, _ := h.store.InstallationID(ctx)
	stale := compose.LinkNetworkPrefix + installation + "_99"
	fc.missing[network] = true
	fc.networks = []string{network, stale}
	fc.set("koroku-accounts", "a-web\tkoroku-accounts-web-1\tweb\tkoroku-accounts_default,"+stale)
	fc.set("koroku-shop",
		"c-web\tkoroku-shop-web-1\tweb\tkoroku-shop_default",
		"c-db\tkoroku-shop-db-1\tdb\tkoroku-shop_default,"+network)

	if err := h.engine.ReconcileLinks(ctx); err != nil {
		t.Fatal(err)
	}
	if ls := h.calls("network", "ls"); len(ls) != 1 || !strings.Contains(ls[0], "label=io.koroku.installation="+installation) {
		t.Errorf("network ls = %v: must only list this installation's networks", ls)
	}
	if created := h.calls("network", "create"); len(created) != 1 || !strings.HasSuffix(created[0], network) {
		t.Errorf("create calls = %v", created)
	}
	if rm := h.calls("network", "rm"); !slices.Equal(rm, []string{"network rm " + stale}) {
		t.Errorf("rm calls = %v", rm)
	}
	connects := h.calls("network", "connect")
	slices.Sort(connects)
	want := []string{"network connect " + network + " c-web", "network connect --alias auth " + network + " a-web"}
	slices.Sort(want)
	if !slices.Equal(connects, want) {
		t.Errorf("connect calls = %v, want %v", connects, want)
	}
	disconnects := h.calls("network", "disconnect")
	slices.Sort(disconnects)
	wantOff := []string{"network disconnect --force " + network + " c-db", "network disconnect --force " + stale + " a-web"}
	slices.Sort(wantOff)
	if !slices.Equal(disconnects, wantOff) {
		t.Errorf("disconnect calls = %v, want %v", disconnects, wantOff)
	}
}
