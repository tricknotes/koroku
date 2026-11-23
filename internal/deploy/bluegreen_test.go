package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// Blue/green deploys (ARCHITECTURE 7.2) against the fake runner. A small
// simulation of the app's containers answers `docker compose ps` and
// follows up, stop, start and rm.

const bgRepoServices = `{
	"web": {"build": {"context": "."}, "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}],
		"depends_on": {"db": {"condition": "service_healthy"}, "migrate": {"condition": "service_completed_successfully"}}},
	"migrate": {"build": {"context": "."}, "restart": "no"},
	"worker": {"build": {"context": "."}},
	"db": {"image": "postgres:17", "mem_limit": "268435456"}
}`

// bgMergedServices is the merged project of a blue/green release with the
// public service's copies on 20000 (blue) and 20001 (green).
const bgMergedServices = `{
	"web": {"build": {"context": "."}, "mem_limit": "536870912",
		"depends_on": {"db": {"condition": "service_healthy"}, "migrate": {"condition": "service_completed_successfully"}}},
	"web-blue": {"image": "koroku-blog-web:r1", "mem_limit": "536870912", "ports": [{"host_ip": "127.0.0.1", "target": 3000, "published": "20000", "protocol": "tcp"}],
		"depends_on": {"db": {"condition": "service_healthy"}, "migrate": {"condition": "service_completed_successfully"}}},
	"web-green": {"image": "koroku-blog-web:r1", "mem_limit": "536870912", "ports": [{"host_ip": "127.0.0.1", "target": 3000, "published": "20001", "protocol": "tcp"}],
		"depends_on": {"db": {"condition": "service_healthy"}, "migrate": {"condition": "service_completed_successfully"}}},
	"migrate": {"build": {"context": "."}, "restart": "no"},
	"worker": {"build": {"context": "."}},
	"db": {"image": "postgres:17", "mem_limit": "268435456"}
}`

// bgPlainMerged is the merged project of a release that runs the public
// service as itself on 20000.
const bgPlainMerged = `{
	"web": {"build": {"context": "."}, "ports": [{"host_ip": "127.0.0.1", "target": 3000, "published": "20000", "protocol": "tcp"}],
		"depends_on": {"db": {"condition": "service_healthy"}, "migrate": {"condition": "service_completed_successfully"}}},
	"migrate": {"build": {"context": "."}, "restart": "no"},
	"worker": {"build": {"context": "."}},
	"db": {"image": "postgres:17"}
}`

// containers simulates the app's containers: service -> state.
type containers struct {
	mu    sync.Mutex
	state map[string]string
	// all is what an `up` without service names starts.
	all []string
}

func (c *containers) set(states map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = states
}

func (c *containers) get() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.state {
		out[k] = v
	}
	return out
}

func (c *containers) ps() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range c.state {
			if !yield(k) {
				return
			}
		}
	}) {
		lines = append(lines, fmt.Sprintf(`{"Name": "koroku-blog-%s-1", "Service": %q, "State": %q}`, name, name, c.state[name]))
	}
	return strings.Join(lines, "\n")
}

// composeArgs splits a docker compose call into its subcommand and the
// service names given to it.
func composeArgs(args []string) (sub string, names []string) {
	i := 1
	for ; i < len(args); i++ {
		switch args[i] {
		case "-p", "-f", "--env-file", "--profile":
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return "", nil
	}
	sub = args[i]
	for j := i + 1; j < len(args); j++ {
		a := args[j]
		switch a {
		case "--pull", "--wait-timeout", "--tail":
			j++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			if sub == "exec" || sub == "run" {
				names = append(names, a)
				break
			}
			names = append(names, a)
		}
	}
	return sub, names
}

func (c *containers) follow(cmd runner.Cmd) {
	if cmd.Name != "docker" || len(cmd.Args) == 0 || cmd.Args[0] != "compose" {
		return
	}
	sub, names := composeArgs(cmd.Args)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.state = map[string]string{}
	}
	switch sub {
	case "up":
		if len(names) == 0 {
			names = c.all
		}
		for _, n := range names {
			if n == "migrate" {
				c.state[n] = "exited"
			} else {
				c.state[n] = "running"
			}
		}
	case "start", "restart":
		if len(names) == 0 {
			for n := range c.state {
				names = append(names, n)
			}
		}
		for _, n := range names {
			if _, ok := c.state[n]; ok && n != "migrate" {
				c.state[n] = "running"
			}
		}
	case "stop":
		if len(names) == 0 {
			for n := range c.state {
				names = append(names, n)
			}
		}
		for _, n := range names {
			if _, ok := c.state[n]; ok {
				c.state[n] = "exited"
			}
		}
	case "rm":
		for _, n := range names {
			delete(c.state, n)
		}
	case "down":
		c.state = map[string]string{}
	}
}

// bgHarness is a harness with a blue/green app "blog" (host ports 20000
// and 20001) whose containers are simulated.
type bgHarness struct {
	*harness
	app  store.App
	sim  *containers
	fail func(sub string, names []string) error
}

func newBGHarness(t *testing.T, blueGreen bool) *bgHarness {
	t.Helper()
	h := newHarness(t, nil)
	app := h.createApp("blog")
	if _, err := h.store.AllocateAltHostPort(context.Background(), app.ID, 20000, 20999); err != nil {
		t.Fatal(err)
	}
	b := &bgHarness{harness: h, sim: &containers{all: []string{"db", "migrate", "web", "worker"}}}
	b.setBlueGreen(blueGreen)
	h.mu.Lock()
	h.repoServices = bgRepoServices
	h.servicesFor = func(cmd runner.Cmd) string {
		if _, merged := composeSubcommand(cmd.Args); !merged {
			return bgRepoServices
		}
		if slices.Contains(cmd.Args, compose.DisabledProfile) {
			return bgMergedServices
		}
		return bgPlainMerged
	}
	h.psOutput = b.sim.ps
	h.onCompose = func(ctx context.Context, sub string, cmd runner.Cmd) error {
		if b.fail != nil {
			_, names := composeArgs(cmd.Args)
			if err := b.fail(sub, names); err != nil {
				return err
			}
		}
		b.sim.follow(cmd)
		return nil
	}
	h.mu.Unlock()
	h.start()
	return b
}

func (b *bgHarness) setBlueGreen(on bool) {
	b.t.Helper()
	app := b.reload("blog")
	app.BlueGreen = on
	app.BlueGreenDrainSeconds = 0
	if err := b.store.UpdateApp(context.Background(), app, b.clock.Now()); err != nil {
		b.t.Fatal(err)
	}
	b.app = b.reload("blog")
	if on {
		b.sim.all = []string{"db", "migrate", "web-blue", "worker"}
	} else {
		b.sim.all = []string{"db", "migrate", "web", "worker"}
	}
}

func (b *bgHarness) setFail(fn func(sub string, names []string) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = fn
}

// composeCalls lists the compose calls since the last reset as
// "<sub> <names...>".
func (b *bgHarness) composeCalls() []string {
	var out []string
	for _, c := range b.runner.Calls() {
		if c.Name != "docker" || len(c.Args) == 0 || c.Args[0] != "compose" {
			continue
		}
		sub, names := composeArgs(c.Args)
		if sub == "config" || sub == "ps" || sub == "logs" || sub == "build" || sub == "pull" {
			continue
		}
		out = append(out, strings.TrimSpace(sub+" "+strings.Join(names, " ")))
	}
	return out
}

func (b *bgHarness) running() []string {
	var out []string
	for name, state := range b.sim.get() {
		if state == "running" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBlueGreenFirstDeployStartsInBlue(t *testing.T) {
	b := newBGHarness(t, true)
	d := b.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, b.log("blog", d.ID))
	}
	if d.WebColor != store.ColorBlue || d.SwitchedAt != nil {
		t.Errorf("colour %q, switched at %v", d.WebColor, d.SwitchedAt)
	}
	if app := b.reload("blog"); app.WebColor != store.ColorBlue {
		t.Errorf("app colour = %q", app.WebColor)
	}
	if got, want := b.running(), []string{"db", "web-blue", "worker"}; !slices.Equal(got, want) {
		t.Errorf("running = %v, want %v", got, want)
	}
	up := b.lastCall("up")
	if !slices.Contains(up.Args, "--remove-orphans") || slices.Contains(up.Args, "web-green") || slices.Contains(up.Args, "web") {
		t.Errorf("up args = %v", up.Args)
	}
	// Build and pull see the disabled public service.
	if build := b.lastCall("build"); !slices.Contains(build.Args, compose.DisabledProfile) {
		t.Errorf("build args = %v", build.Args)
	}
	// The commit variables live in their own file.
	env := readFile(t, b.engine.EnvFile("blog"))
	release := readFile(t, b.engine.ReleaseEnvFile("blog"))
	if strings.Contains(env, "KOROKU_COMMIT_SHA") || !strings.Contains(release, "KOROKU_COMMIT_SHA="+shaA) {
		t.Errorf("env:\n%s\nrelease env:\n%s", env, release)
	}
	if !slices.Contains(up.Args, b.engine.ReleaseEnvFile("blog")) {
		t.Errorf("interpolation does not see the release env file: %v", up.Args)
	}
	override := readFile(t, b.engine.ReleaseDir("blog", d.ID)+"/"+compose.OverrideFileName)
	if !strings.Contains(override, `"web-green":`) || !strings.Contains(override, "127.0.0.1:20001:3000") {
		t.Errorf("override:\n%s", override)
	}
	if urls := b.health.urls; len(urls) != 1 || urls[0] != "http://127.0.0.1:20000/up" {
		t.Errorf("health checks = %v", urls)
	}
	log := b.log("blog", d.ID)
	if !strings.Contains(log, "start Starting blue on port 20000") {
		t.Errorf("log:\n%s", log)
	}
}

// deployBoth deploys shaA (blue) and then shaB side by side (green).
func (b *bgHarness) deployBoth() (first, second store.Deployment) {
	b.t.Helper()
	first = b.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		b.t.Fatalf("first deployment: %+v\n%s", first, b.log("blog", first.ID))
	}
	b.runner.Reset()
	b.health.mu.Lock()
	b.health.urls = nil
	b.health.mu.Unlock()
	return first, b.deploy("blog", shaB)
}

func TestBlueGreenDeploySideBySide(t *testing.T) {
	b := newBGHarness(t, true)
	var colorAtSync []string
	b.router.mu.Lock()
	b.router.onSync = func() {
		app, _ := b.store.GetApp(context.Background(), "blog")
		colorAtSync = append(colorAtSync, app.WebColor)
	}
	b.router.mu.Unlock()
	_, d := b.deployBoth()
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, b.log("blog", d.ID))
	}
	want := []string{
		"up migrate web-green", // start: the one-shot and the new colour, --no-deps
		"up db worker",         // services, after the switch
		"stop web-blue",        // drain
		"rm web-blue",          //
		"up web-green",         // removing the containers of removed services
	}
	if got := b.composeCalls(); !slices.Equal(got, want) {
		t.Errorf("compose calls = %q\nwant %q", got, want)
	}
	for _, c := range b.runner.Calls() {
		if sub, names := composeArgs(c.Args); sub == "up" && len(names) > 0 && names[0] == "migrate" {
			if !slices.Contains(c.Args, "--no-deps") || slices.Contains(c.Args, "--remove-orphans") {
				t.Errorf("start up = %v", c.Args)
			}
		}
	}
	if urls := b.health.urls; len(urls) != 1 || urls[0] != "http://127.0.0.1:20001/up" {
		t.Errorf("health checks = %v", urls)
	}
	app := b.reload("blog")
	if app.WebColor != store.ColorGreen || app.LivePort() != 20001 {
		t.Errorf("app colour %q", app.WebColor)
	}
	if d.WebColor != store.ColorGreen || d.SwitchedAt == nil {
		t.Errorf("deployment colour %q, switched %v", d.WebColor, d.SwitchedAt)
	}
	// The proxy was loaded with green at the switch.
	if !slices.Contains(colorAtSync, store.ColorGreen) {
		t.Errorf("colours seen by the router: %v", colorAtSync)
	}
	if got, want := b.running(), []string{"db", "web-green", "worker"}; !slices.Equal(got, want) {
		t.Errorf("running = %v, want %v", got, want)
	}
	log := b.log("blog", d.ID)
	for _, line := range []string{
		"start Starting green on port 20001",
		"check Checking http://127.0.0.1:20001/up",
		"switch Switching traffic from blue to green",
		"services Recreating the other services",
		"drain Stopping blue",
		"done Deployment succeeded",
	} {
		if !strings.Contains(log, line) {
			t.Errorf("log lacks %q:\n%s", line, log)
		}
	}
	// The variables did not change: nothing warns of a recreated database.
	if strings.Contains(log, "because the app's env file changed") {
		t.Errorf("log:\n%s", log)
	}

	// The next one goes back to blue.
	b.runner.Reset()
	third := b.deploy("blog", shaC)
	if third.Status != store.StatusSucceeded || third.WebColor != store.ColorBlue || b.reload("blog").WebColor != store.ColorBlue {
		t.Fatalf("third = %+v\n%s", third, b.log("blog", third.ID))
	}
	if got := b.composeCalls(); got[0] != "up migrate web-blue" {
		t.Errorf("third calls = %q", got)
	}
}

func TestBlueGreenWarnsWhenTheEnvironmentChanged(t *testing.T) {
	b := newBGHarness(t, true)
	b.deploy("blog", shaA)
	b.setEnv(b.app, map[string]string{"GREETING": "hello"})
	d := b.deploy("blog", shaB)
	log := b.log("blog", d.ID)
	if d.Status != store.StatusSucceeded ||
		!strings.Contains(log, "Recreating db because the app's env file changed (GREETING); requests may fail while it restarts.") ||
		strings.Contains(log, "Recreating worker because") {
		t.Errorf("deployment %s, log:\n%s", d.Status, log)
	}
}

func TestBlueGreenNewColourFailsCheck(t *testing.T) {
	b := newBGHarness(t, true)
	first := b.deploy("blog", shaA)
	b.health.mu.Lock()
	b.health.err = errors.New("the app did not answer")
	b.health.mu.Unlock()
	b.runner.Reset()
	d := b.deploy("blog", shaB)
	if d.Status != store.StatusFailed || strings.Contains(d.Error, "restored") {
		t.Fatalf("deployment = %+v", d)
	}
	want := []string{"up migrate web-green", "rm web-green"}
	if got := b.composeCalls(); !slices.Equal(got, want) {
		t.Errorf("compose calls = %q, want %q", got, want)
	}
	app := b.reload("blog")
	if app.WebColor != store.ColorBlue || *app.CurrentDeploymentID != first.ID {
		t.Errorf("app = colour %q, current %d", app.WebColor, *app.CurrentDeploymentID)
	}
	if got, want := b.running(), []string{"db", "web-blue", "worker"}; !slices.Equal(got, want) {
		t.Errorf("running = %v, want %v", got, want)
	}
	// The env files are the running release's again.
	if release := readFile(t, b.engine.ReleaseEnvFile("blog")); !strings.Contains(release, shaA) {
		t.Errorf("release env = %s", release)
	}
	log := b.log("blog", d.ID)
	if strings.Contains(log, "rollback") || !strings.Contains(log, "Removing web-green; the previous release keeps serving.") {
		t.Errorf("log:\n%s", log)
	}
}

func TestBlueGreenFailureAfterTheSwitchSwitchesBack(t *testing.T) {
	b := newBGHarness(t, true)
	first := b.deploy("blog", shaA)
	b.runner.Reset()
	upCalls := 0
	b.setFail(func(sub string, names []string) error {
		if sub == "up" {
			upCalls++
			if upCalls == 2 { // services
				return errors.New("worker did not start")
			}
		}
		return nil
	})
	var colorAtSync []string
	b.router.mu.Lock()
	b.router.onSync = func() {
		app, _ := b.store.GetApp(context.Background(), "blog")
		colorAtSync = append(colorAtSync, app.WebColor)
	}
	b.router.mu.Unlock()
	d := b.deploy("blog", shaB)
	if d.Status != store.StatusFailed || d.SwitchedAt != nil {
		t.Fatalf("deployment = %+v", d)
	}
	want := []string{"up migrate web-green", "up db worker", "up db worker", "rm web-green"}
	if got := b.composeCalls(); !slices.Equal(got, want) {
		t.Errorf("compose calls = %q, want %q", got, want)
	}
	// The restore used the running release's files.
	var restore runner.Cmd
	for _, c := range b.runner.Calls() {
		if sub, names := composeArgs(c.Args); sub == "up" && slices.Equal(names, []string{"db", "worker"}) {
			restore = c
		}
	}
	if !strings.Contains(strings.Join(restore.Args, " "), fmt.Sprintf("/releases/%d/", first.ID)) {
		t.Errorf("restore = %v", restore.Args)
	}
	if !slices.Equal(colorAtSync[len(colorAtSync)-2:], []string{store.ColorGreen, store.ColorBlue}) {
		t.Errorf("colours seen by the router: %v", colorAtSync)
	}
	if app := b.reload("blog"); app.WebColor != store.ColorBlue {
		t.Errorf("colour = %q", app.WebColor)
	}
	if log := b.log("blog", d.ID); !strings.Contains(log, "Restored db, worker of deployment") {
		t.Errorf("log:\n%s", log)
	}
}

func TestBlueGreenSwitchFailureKeepsTheOldColour(t *testing.T) {
	b := newBGHarness(t, true)
	b.deploy("blog", shaA)
	b.router.mu.Lock()
	b.router.err = errors.New("caddy is down")
	b.router.mu.Unlock()
	b.runner.Reset()
	d := b.deploy("blog", shaB)
	if d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}
	if got, want := b.composeCalls(), []string{"up migrate web-green", "rm web-green"}; !slices.Equal(got, want) {
		t.Errorf("compose calls = %q, want %q", got, want)
	}
	if app := b.reload("blog"); app.WebColor != store.ColorBlue {
		t.Errorf("colour = %q", app.WebColor)
	}
}

func TestBlueGreenNeedsMemoryForTwoCopies(t *testing.T) {
	b := newBGHarness(t, true)
	b.deploy("blog", shaA)
	free := uint64(300 << 20)
	b.prober.mu.Lock()
	b.prober.free = &free
	b.prober.mu.Unlock()
	b.runner.Reset()
	d := b.deploy("blog", shaB)
	want := "Not enough memory to run two web containers (300 MiB available, 512 MiB needed)."
	if d.Status != store.StatusFailed || d.Error != want {
		t.Fatalf("deployment = %+v", d)
	}
	if got := b.composeCalls(); len(got) != 0 {
		t.Errorf("compose calls = %q", got)
	}
	if replaced := d.ContainersReplaced; replaced == nil || *replaced {
		t.Errorf("containers replaced = %v", replaced)
	}
	if log := b.log("blog", d.ID); !strings.Contains(log, want) {
		t.Errorf("log:\n%s", log)
	}
}

func TestBlueGreenCancelBeforeTheSwitch(t *testing.T) {
	b := newBGHarness(t, true)
	b.deploy("blog", shaA)
	b.runner.Reset()
	started := make(chan struct{})
	var once sync.Once
	b.setFail(func(sub string, names []string) error {
		if sub == "up" && slices.Contains(names, "web-green") {
			once.Do(func() { close(started) })
			time.Sleep(50 * time.Millisecond)
		}
		return nil
	})
	b.mu.Lock()
	inner := b.onCompose
	b.onCompose = func(ctx context.Context, sub string, cmd runner.Cmd) error {
		if err := inner(ctx, sub, cmd); err != nil {
			return err
		}
		if _, names := composeArgs(cmd.Args); sub == "up" && slices.Contains(names, "web-green") && len(names) > 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	b.mu.Unlock()
	d := b.enqueue("blog", shaB)
	<-started
	if err := b.engine.Cancel(context.Background(), d.ID); err != nil {
		t.Fatal(err)
	}
	got := b.wait(d.ID)
	if got.Status != store.StatusCanceled {
		t.Fatalf("deployment = %+v", got)
	}
	if calls := b.composeCalls(); calls[len(calls)-1] != "rm web-green" {
		t.Errorf("compose calls = %q", calls)
	}
	if app := b.reload("blog"); app.WebColor != store.ColorBlue {
		t.Errorf("colour = %q", app.WebColor)
	}
}

func TestBlueGreenCleanupAtStart(t *testing.T) {
	b := newBGHarness(t, true)
	b.deploy("blog", shaA)
	// koroku stopped after a switch to green: blue is left.
	if err := b.store.SetAppWebColor(context.Background(), b.app.ID, store.ColorGreen); err != nil {
		t.Fatal(err)
	}
	b.sim.set(map[string]string{"db": "running", "web-blue": "running", "web-green": "running", "worker": "running"})
	b.runner.Reset()
	b.engine.CleanUpColors(context.Background())
	if got := b.composeCalls(); !slices.Equal(got, []string{"rm web-blue"}) {
		t.Errorf("compose calls = %q", got)
	}
	if got, want := b.running(), []string{"db", "web-green", "worker"}; !slices.Equal(got, want) {
		t.Errorf("running = %v", got)
	}
}

func TestBlueGreenEnableMovesFromThePublicService(t *testing.T) {
	b := newBGHarness(t, false)
	first := b.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded || first.WebColor != "" {
		t.Fatalf("first = %+v", first)
	}
	if got, want := b.running(), []string{"db", "web", "worker"}; !slices.Equal(got, want) {
		t.Fatalf("running = %v", got)
	}
	b.setBlueGreen(true)
	b.runner.Reset()
	d := b.deploy("blog", shaB)
	if d.Status != store.StatusSucceeded || d.WebColor != store.ColorGreen {
		t.Fatalf("deployment = %+v\n%s", d, b.log("blog", d.ID))
	}
	want := []string{"up migrate web-green", "up db worker", "stop web", "rm web", "up web-green"}
	if got := b.composeCalls(); !slices.Equal(got, want) {
		t.Errorf("compose calls = %q\nwant %q", got, want)
	}
	if log := b.log("blog", d.ID); !strings.Contains(log, "switch Switching traffic from web to green") {
		t.Errorf("log:\n%s", log)
	}
	if got, want := b.running(), []string{"db", "web-green", "worker"}; !slices.Equal(got, want) {
		t.Errorf("running = %v", got)
	}

	// Turning it off: the next deploy recreates the public service in
	// place of its colour and forgets the colour.
	b.setBlueGreen(false)
	b.runner.Reset()
	third := b.deploy("blog", shaC)
	if third.Status != store.StatusSucceeded || third.WebColor != "" {
		t.Fatalf("third = %+v\n%s", third, b.log("blog", third.ID))
	}
	if got := b.composeCalls(); !slices.Equal(got, []string{"rm web-green", "up"}) {
		t.Errorf("compose calls = %q", got)
	}
	app := b.reload("blog")
	if app.WebColor != "" || app.LivePort() != 20000 {
		t.Errorf("app colour %q", app.WebColor)
	}
	if got, want := b.running(), []string{"db", "web", "worker"}; !slices.Equal(got, want) {
		t.Errorf("running = %v", got)
	}
}

func TestBlueGreenRollback(t *testing.T) {
	b := newBGHarness(t, true)
	first, second := b.deployBoth()
	if second.Status != store.StatusSucceeded {
		t.Fatal(second.Status)
	}
	b.runner.Reset()
	back := b.rollback("blog", first.ID)
	if back.Status != store.StatusSucceeded || back.WebColor != store.ColorBlue || back.SwitchedAt == nil {
		t.Fatalf("rollback = %+v\n%s", back, b.log("blog", back.ID))
	}
	want := []string{"up migrate web-blue", "up db worker", "stop web-green", "rm web-green", "up web-blue"}
	if got := b.composeCalls(); !slices.Equal(got, want) {
		t.Errorf("compose calls = %q\nwant %q", got, want)
	}
	for _, c := range b.runner.Calls() {
		if c.Name == "docker" && slices.Contains(c.Args, "build") {
			t.Errorf("a rollback built: %v", c.Args)
		}
	}
	// The kept release's commit variables are back.
	if release := readFile(t, b.engine.ReleaseEnvFile("blog")); !strings.Contains(release, shaA) {
		t.Errorf("release env = %s", release)
	}
	app := b.reload("blog")
	if app.WebColor != store.ColorBlue || *app.CurrentDeploymentID != first.ID {
		t.Errorf("app colour %q, current %d", app.WebColor, *app.CurrentDeploymentID)
	}
}

func TestBlueGreenRollbackToAReleaseBeforeIt(t *testing.T) {
	b := newBGHarness(t, false)
	first := b.deploy("blog", shaA)
	b.setBlueGreen(true)
	b.deploy("blog", shaB)
	b.runner.Reset()
	back := b.rollback("blog", first.ID)
	if back.Status != store.StatusSucceeded || back.WebColor != "" {
		t.Fatalf("rollback = %+v\n%s", back, b.log("blog", back.ID))
	}
	if got := b.composeCalls(); !slices.Equal(got, []string{"rm web-green", "up"}) {
		t.Errorf("compose calls = %q", got)
	}
	if app := b.reload("blog"); app.WebColor != "" {
		t.Errorf("colour = %q", app.WebColor)
	}
}

func TestBlueGreenStoppedAppStartsAlone(t *testing.T) {
	b := newBGHarness(t, true)
	b.deployBoth() // green
	if err := b.engine.Stop(context.Background(), b.reload("blog")); err != nil {
		t.Fatal(err)
	}
	b.runner.Reset()
	d := b.deploy("blog", shaC)
	if d.Status != store.StatusSucceeded || d.WebColor != store.ColorGreen || d.SwitchedAt != nil {
		t.Fatalf("deployment = %+v\n%s", d, b.log("blog", d.ID))
	}
	if got := b.composeCalls(); !slices.Equal(got, []string{"rm web", "up db migrate web-green worker"}) {
		// rm only names what has a container: nothing for web.
		if !slices.Equal(got, []string{"up db migrate web-green worker"}) {
			t.Errorf("compose calls = %q", got)
		}
	}
}

func TestBlueGreenOperationsUseTheLiveColour(t *testing.T) {
	b := newBGHarness(t, true)
	b.deployBoth() // green
	ctx := context.Background()

	b.runner.Reset()
	if err := b.engine.Restart(ctx, b.reload("blog")); err != nil {
		t.Fatal(err)
	}
	if err := b.engine.Stop(ctx, b.reload("blog")); err != nil {
		t.Fatal(err)
	}
	if err := b.engine.Start(ctx, b.reload("blog")); err != nil {
		t.Fatal(err)
	}
	want := []string{"restart db migrate web-green worker", "stop", "up db migrate web-green worker"}
	if got := b.composeCalls(); !slices.Equal(got, want) {
		t.Errorf("compose calls = %q\nwant %q", got, want)
	}
	if b.reload("blog").WebColor != store.ColorGreen {
		t.Error("start changed the colour")
	}

	// Waking a sleeping app names the live colour too.
	app := b.reload("blog")
	app.IdleSleep = true
	if err := b.store.UpdateApp(ctx, app, b.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.engine.Sleep(ctx, b.reload("blog")); err != nil {
		t.Fatal(err)
	}
	b.runner.Reset()
	if err := b.engine.Wake(ctx, b.reload("blog")); err != nil {
		t.Fatal(err)
	}
	if got := b.composeCalls(); !slices.Equal(got, []string{"start db migrate web-green worker"}) {
		t.Errorf("wake calls = %q", got)
	}
	if urls := b.health.urls; urls[len(urls)-1] != "http://127.0.0.1:20001/up" {
		t.Errorf("wake checked %v", urls)
	}

	// Hooks run in the live colour; the colours are no task services.
	if args := hookArgs(b.reload("blog"), "web", "true"); !slices.Contains(args, "web-green") {
		t.Errorf("hook args = %v", args)
	}
	var unknown *UnknownServiceError
	if _, err := b.engine.RunTask(ctx, b.reload("blog"), TaskSpec{Service: "web-green", Command: "true"}); !errors.As(err, &unknown) ||
		slices.Contains(unknown.Services, "web-green") || !slices.Contains(unknown.Services, "web") {
		t.Errorf("task in a colour: %v", err)
	}
}

func TestBlueGreenPolicyRejectsAContainerName(t *testing.T) {
	b := newBGHarness(t, true)
	b.mu.Lock()
	b.servicesFor = func(runner.Cmd) string {
		return strings.Replace(bgRepoServices, `"web": {"build"`, `"web": {"container_name": "blog-web", "build"`, 1)
	}
	b.mu.Unlock()
	d := b.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "[blue_green]") {
		t.Fatalf("deployment = %+v", d)
	}
	b.app = b.reload("blog")
	b.app.RelaxPolicy = true
	if err := b.store.UpdateApp(context.Background(), b.app, b.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if d := b.deploy("blog", shaB); d.Status != store.StatusFailed || !strings.Contains(d.Error, "[blue_green]") {
		t.Errorf("relaxed: %+v", d)
	}
}

func TestBlueGreenIncludedPublicService(t *testing.T) {
	b := newBGHarness(t, true)
	b.setFail(func(sub string, names []string) error { return nil })
	b.mu.Lock()
	inner := b.onCompose
	b.onCompose = func(ctx context.Context, sub string, cmd runner.Cmd) error {
		if _, merged := composeSubcommand(cmd.Args); sub == "config" && merged {
			_, _ = cmd.Stderr.Write([]byte(`cannot extend service "web-blue" in /x/koroku.override.yaml: service "web" not found in compose.yaml` + "\n"))
			return errors.New("exit status 15")
		}
		return inner(ctx, sub, cmd)
	}
	b.mu.Unlock()
	d := b.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "must be defined in compose.yaml itself") {
		t.Fatalf("deployment = %+v", d)
	}
}
