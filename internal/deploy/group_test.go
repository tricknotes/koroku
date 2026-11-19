package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
)

// containers fakes `docker ps` per Compose project: lines of
// "<id>\t<name>\t<service>\t<networks>".
type fakeContainers struct {
	mu       sync.Mutex
	projects map[string][]string
	networks []string // what `docker network ls` lists
	missing  map[string]bool
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
		if f.missing[args[len(args)-1]] {
			return "", true, errors.New("no such network")
		}
	}
	return "", false, nil
}

func newGroupHarness(t *testing.T) (*harness, *fakeContainers) {
	h := newHarness(t, nil)
	fc := &fakeContainers{}
	h.mu.Lock()
	h.dockerOutput = fc.answer
	h.mu.Unlock()
	return h, fc
}

func (h *harness) joinGroup(app store.App, name string, services ...string) store.Group {
	h.t.Helper()
	ctx := context.Background()
	groups, _ := h.store.ListGroups(ctx)
	var group store.Group
	for _, g := range groups {
		if g.Name == name {
			group = g
		}
	}
	if group.ID == 0 {
		var err error
		if group, err = h.store.CreateGroup(ctx, name, store.EnvironmentStaging, h.clock.Now()); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.store.SetAppGroup(ctx, app.ID, &group.ID, services, h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	return group
}

func (h *harness) calls(prefix ...string) [][]string {
	var out [][]string
	for _, c := range h.runner.Calls() {
		if c.Name == "docker" && len(c.Args) >= len(prefix) && slices.Equal(c.Args[:len(prefix)], prefix) {
			out = append(out, c.Args)
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

func TestDeployAttachesTheGroupServices(t *testing.T) {
	h, fc := newGroupHarness(t)
	h.start()
	app := h.createApp("shop-api")
	group := h.joinGroup(app, "shop", "web")
	fc.missing = map[string]bool{group.Network(): true}
	fc.set("koroku-shop-api",
		"c-web\tkoroku-shop-api-web-1\tweb\tkoroku-shop-api_default",
		"c-db\tkoroku-shop-api-db-1\tdb\tkoroku-shop-api_default,koroku_group_1")

	d := h.deploy("shop-api", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("status = %s: %s\n%s", d.Status, d.Error, h.log("shop-api", d.ID))
	}
	ups := h.upCalls()
	if len(ups) != 2 || !strings.HasPrefix(ups[0], "up --no-start") || !strings.Contains(ups[1], "--wait") {
		t.Errorf("up calls = %v, want create then start", ups)
	}
	connects := h.calls("network", "connect")
	if len(connects) != 1 || strings.Join(connects[0], " ") != "network connect --alias web.shop-api.koroku.internal koroku_group_1 c-web" {
		t.Errorf("connect calls = %v", connects)
	}
	// db was not chosen: it comes off the network.
	if got := h.calls("network", "disconnect"); len(got) != 1 || got[0][len(got[0])-1] != "c-db" {
		t.Errorf("disconnect calls = %v", got)
	}
	if created := h.calls("network", "create"); len(created) != 1 || !slices.Contains(created[0], "--internal") {
		t.Errorf("network create calls = %v", created)
	}
	// Connecting happens between creating and starting.
	var order []string
	for _, c := range h.runner.Calls() {
		if c.Name != "docker" {
			continue
		}
		if sub, _ := composeSubcommand(c.Args); sub == "up" {
			order = append(order, "up")
		} else if len(c.Args) > 1 && c.Args[1] == "connect" {
			order = append(order, "connect")
		}
	}
	if strings.Join(order, ",") != "up,connect,up" {
		t.Errorf("order = %v", order)
	}
	if !strings.Contains(h.log("shop-api", d.ID), "Joining the group network: [web]") {
		t.Errorf("the deploy log does not mention the group")
	}
}

func TestDeployRefusesServicesThatCannotJoin(t *testing.T) {
	h, _ := newGroupHarness(t)
	h.start()
	app := h.createApp("shop-api")
	h.joinGroup(app, "shop", "cache")

	d := h.deploy("shop-api", shaA)
	if d.Status != store.StatusFailed {
		t.Fatalf("status = %s, want failed", d.Status)
	}
	if log := h.log("shop-api", d.ID); !strings.Contains(log, `[group_service] service "cache"`) {
		t.Errorf("log lacks the violation:\n%s", log)
	}
	if len(h.upCalls()) != 0 {
		t.Errorf("containers were started: %v", h.upCalls())
	}
}

func TestStartAfterLeavingDoesNotAttach(t *testing.T) {
	h, _ := newGroupHarness(t)
	h.start()
	app := h.createApp("shop-api")
	h.joinGroup(app, "shop", "web")
	if d := h.deploy("shop-api", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", d.Error)
	}

	ctx := context.Background()
	if err := h.store.SetAppGroup(ctx, app.ID, nil, nil, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	app, _ = h.store.GetApp(ctx, "shop-api")
	h.runner.Reset()
	if err := h.engine.Start(ctx, app); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ups := h.upCalls(); len(ups) != 1 || strings.Contains(ups[0], "--no-start") {
		t.Errorf("up calls after leaving = %v", ups)
	}
	if got := h.calls("network", "connect"); len(got) != 0 {
		t.Errorf("connect calls after leaving = %v", got)
	}
}

func TestSetGroup(t *testing.T) {
	h, fc := newGroupHarness(t)
	h.start()
	ctx := context.Background()
	app := h.createApp("shop-api")
	if d := h.deploy("shop-api", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", d.Error)
	}
	app, _ = h.store.GetApp(ctx, "shop-api")
	shop, err := h.engine.CreateGroup(ctx, "shop", store.EnvironmentStaging)
	if err != nil {
		t.Fatal(err)
	}
	live, err := h.engine.CreateGroup(ctx, "live", store.EnvironmentProduction)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.engine.SetGroup(ctx, app, &live.ID, nil); !errors.Is(err, store.ErrGroupEnvironment) {
		t.Errorf("production group: err = %v", err)
	}
	var cannot *GroupServiceError
	if err := h.engine.SetGroup(ctx, app, &shop.ID, []string{"web", "cache"}); !errors.As(err, &cannot) || len(cannot.Violations) != 1 || cannot.Violations[0].Service != "cache" {
		t.Errorf("unknown service: err = %v", err)
	}
	if err := h.engine.SetGroup(ctx, app, &shop.ID, []string{"web", "db"}); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	app, _ = h.store.GetApp(ctx, "shop-api")

	// Dropping "db" takes its container off the network right away.
	fc.set("koroku-shop-api",
		"c-web\tkoroku-shop-api-web-1\tweb\tkoroku-shop-api_default,koroku_group_1",
		"c-db\tkoroku-shop-api-db-1\tdb\tkoroku-shop-api_default,koroku_group_1")
	h.runner.Reset()
	if err := h.engine.SetGroup(ctx, app, &shop.ID, []string{"web"}); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	got := h.calls("network", "disconnect")
	if len(got) != 1 || got[0][len(got[0])-1] != "c-db" {
		t.Errorf("disconnect calls = %v, want only c-db", got)
	}

	// Leaving takes everything off.
	app, _ = h.store.GetApp(ctx, "shop-api")
	h.runner.Reset()
	if err := h.engine.SetGroup(ctx, app, nil, nil); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if got := h.calls("network", "disconnect"); len(got) != 2 {
		t.Errorf("disconnect calls after leaving = %v", got)
	}

	// A group with members cannot be deleted; an empty one can.
	app, _ = h.store.GetApp(ctx, "shop-api")
	if err := h.engine.SetGroup(ctx, app, &shop.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.DeleteGroup(ctx, shop.ID); !errors.Is(err, store.ErrGroupNotEmpty) {
		t.Errorf("DeleteGroup with a member: err = %v", err)
	}
	h.runner.Reset()
	if err := h.engine.DeleteGroup(ctx, live.ID); err != nil {
		t.Errorf("DeleteGroup: %v", err)
	}
	if rm := h.calls("network", "rm"); len(rm) != 1 || rm[0][2] != live.Network() {
		t.Errorf("network rm calls = %v", rm)
	}
}

func TestReconcileGroups(t *testing.T) {
	h, fc := newGroupHarness(t)
	ctx := context.Background()
	api := h.createApp("shop-api")
	group := h.joinGroup(api, "shop", "web")
	fc.missing = map[string]bool{group.Network(): true}
	fc.networks = []string{"koroku_group_1", "koroku_group_7", "not_koroku_group_9"}
	fc.set("koroku-shop-api",
		"c-web\tkoroku-shop-api-web-1\tweb\tkoroku-shop-api_default,koroku_group_1",
		"c-db\tkoroku-shop-api-db-1\tdb\tkoroku-shop-api_default,koroku_group_1,koroku_group_7")

	if err := h.engine.ReconcileGroups(ctx); err != nil {
		t.Fatal(err)
	}
	if created := h.calls("network", "create"); len(created) != 1 {
		t.Errorf("create calls = %v", created)
	}
	if rm := h.calls("network", "rm"); len(rm) != 1 || rm[0][2] != "koroku_group_7" {
		t.Errorf("rm calls = %v", rm)
	}
	var disconnected []string
	for _, args := range h.calls("network", "disconnect") {
		disconnected = append(disconnected, args[3]+" "+args[4])
	}
	slices.Sort(disconnected)
	want := []string{"koroku_group_1 c-db", "koroku_group_7 c-db"}
	if !slices.Equal(disconnected, want) {
		t.Errorf("disconnected = %v, want %v", disconnected, want)
	}
}
