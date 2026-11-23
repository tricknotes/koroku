//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeployOnlyTakesCommitsTheRefReaches deploys commits by SHA against a
// repository that, like a hosting service, also hands out the head of a
// pull request from a fork (refs/pull/1/head): a deployment takes a commit
// only from a branch or tag that reaches it (ARCHITECTURE 7).
func TestDeployOnlyTakesCommitsTheRefReaches(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	source := newRepo(t, "image-app")
	older := source.commit("Version 1")
	source.write("VERSION", "v2\n")
	tip := source.commit("Version 2")

	// A commit that only another branch reaches.
	source.git("checkout", "--quiet", "-b", "release")
	source.write("VERSION", "release\n")
	onRelease := source.commit("Release")
	source.git("checkout", "--quiet", "main")

	// A commit that only the pull request's ref reaches. Its Compose file
	// would start a service of its own.
	source.git("checkout", "--quiet", "-b", "from-fork")
	compose, err := os.ReadFile(filepath.Join(source.dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	source.write("compose.yaml", string(compose)+"  intruder:\n    image: busybox:1.37\n    command: [\"sleep\", \"600\"]\n")
	foreign := source.commit("From a fork")
	source.git("update-ref", "refs/pull/1/head", foreign)
	source.git("checkout", "--quiet", "main")
	source.git("branch", "--quiet", "-D", "from-fork")

	slug := e.slug("reach")
	project := "koroku-" + slug
	app := e.createApp(slug, source.url())

	// refused checks that a deployment failed in the fetch step and left
	// nothing of the commit behind.
	refused := func(name string, body map[string]string, want string) {
		t.Helper()
		d := e.deploy(slug, body)
		log := e.logText(d.ID)
		if d.Status != "failed" || !strings.Contains(d.Error, want) ||
			!strings.Contains(d.Error, "deploy from a branch or tag that contains the commit") {
			t.Errorf("%s: deployment %s: %q\n%s", name, d.Status, d.Error, log)
		}
		var steps []string
		for _, line := range strings.Split(log, "\n") {
			if fields := strings.Fields(line); len(fields) > 2 && fields[0] == "==>" {
				steps = append(steps, fields[2])
			}
		}
		if strings.Join(steps, " ") != "fetch failed" {
			t.Errorf("%s: steps = %v, want the deployment to end in the fetch step:\n%s", name, steps, log)
		}
		entries, _ := os.ReadDir(e.engine.ReleaseDir(slug, d.ID))
		for _, entry := range entries {
			if entry.Name() != ".git" {
				t.Errorf("%s: %s was checked out", name, entry.Name())
			}
		}
		if _, ok := containerStates(t, project)["intruder"]; ok {
			t.Errorf("%s: a service of the refused commit runs", name)
		}
	}

	// Before the app runs anything: the commit from the fork is refused,
	// from the branch the app follows and from any other ref.
	short := foreign[:8]
	refused("only the SHA", map[string]string{"sha": foreign}, `commit `+short+` is not reachable from "main"`)
	refused("from another branch", map[string]string{"ref": "release", "sha": foreign}, `commit `+short+` is not reachable from "release"`)
	refused("from the pull request's ref", map[string]string{"ref": "pull/1/head", "sha": foreign}, `no branch or tag named "pull/1/head"`)
	refused("from the full name of the ref", map[string]string{"ref": "refs/pull/1/head", "sha": foreign}, `no branch or tag named "refs/pull/1/head"`)
	if states := containerStates(t, project); len(states) != 0 {
		t.Fatalf("containers after refused deployments: %v", states)
	}

	// A commit behind the tip of the default branch deploys by its SHA,
	// and the record names the branch it was checked against.
	first := e.deploy(slug, map[string]string{"sha": older})
	if first.Status != "succeeded" || first.Ref != "main" {
		t.Fatalf("deployment of an older commit: %s from %q: %s\n%s", first.Status, first.Ref, first.Error, e.logText(first.ID))
	}
	if d := e.deploy(slug, map[string]string{"sha": tip}); d.Status != "succeeded" {
		t.Fatalf("deployment of the tip: %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}

	// A commit of another branch deploys only from that branch. The
	// refused attempts leave the running release alone.
	refused("another branch's commit by its SHA", map[string]string{"sha": onRelease}, `is not reachable from "main"`)
	refused("the fork's commit over a running release", map[string]string{"sha": foreign}, `is not reachable from "main"`)
	if got := page(t, app.HostPort)[""]; !strings.HasPrefix(got, "image-app") {
		t.Errorf("the app stopped answering after refused deployments: %q", got)
	}
	if states := containerStates(t, project); len(states) != 1 || states["web"] != "running" {
		t.Errorf("containers = %v, want only web running", states)
	}
	fromRelease := e.deploy(slug, map[string]string{"ref": "release", "sha": onRelease})
	if fromRelease.Status != "succeeded" || fromRelease.Ref != "release" {
		t.Fatalf("deployment from release: %s from %q: %s\n%s", fromRelease.Status, fromRelease.Ref, fromRelease.Error, e.logText(fromRelease.ID))
	}
	version, err := os.ReadFile(filepath.Join(e.engine.ReleaseDir(slug, fromRelease.ID), "VERSION"))
	if err != nil || string(version) != "release\n" {
		t.Errorf("the release of the other branch: VERSION = %q, %v", version, err)
	}
}
