package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
)

// WebhookPath receives the GitHub App's webhook deliveries (ARCHITECTURE 5.2).
const WebhookPath = "/webhooks/github"

const (
	// maxWebhookBodyBytes bounds a delivery. Push payloads list at most a
	// few dozen commits; GitHub itself caps payloads at 25 MB.
	maxWebhookBodyBytes = 5 << 20
	// webhookEnqueueTimeout bounds queueing the deployments of one push.
	webhookEnqueueTimeout = 30 * time.Second
	// Deliveries are remembered this long, up to this many keys, to drop
	// duplicates (ARCHITECTURE 5.2).
	deliveryTTL   = 24 * time.Hour
	deliveryLimit = 4096
)

var (
	eventNamePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
	deliveryPattern  = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
)

// PushDeployer queues deployments started by GitHub pushes (ARCHITECTURE 5.2).
type PushDeployer interface {
	EnqueuePush(ctx context.Context, app store.App, branch, sha, message, pusher string) (store.Deployment, error)
	// EnqueueWorkflowRun queues a push released by a successful run of
	// workflow, for an app that waits for one.
	EnqueueWorkflowRun(ctx context.Context, app store.App, branch, sha, message, pusher, workflow string) (store.Deployment, error)
}

type webhookResultJSON struct {
	// Result is pong, ignored or deployed.
	Result string `json:"result"`
	// Reason says why a delivery was ignored.
	Reason string `json:"reason,omitempty"`
	// Apps lists what each matching app did with a push.
	Apps []pushResultJSON `json:"apps,omitempty"`
}

type pushResultJSON struct {
	App string `json:"app"`
	// Status is queued, duplicate, stopped or failed; for a push to an
	// app that waits for a workflow, waiting_workflow; for a workflow run,
	// also skipped (the run did not succeed) or stale (the branch has a
	// newer commit).
	Status       string `json:"status"`
	DeploymentID int64  `json:"deployment_id,omitempty"`
	// Error is the code of a failure: busy, disk_full, branch_unresolved
	// or internal.
	Error string `json:"error,omitempty"`
	// Conclusion is the run's conclusion, for skipped.
	Conclusion string `json:"conclusion,omitempty"`
}

// handleGitHubWebhook receives the GitHub App's webhook. It needs no
// session and no Origin (guard lets it through); the signature made with
// [github] webhook_secret is the authentication. Only pushes and workflow
// runs do anything: each app that deploys on push from the pushed branch
// of the repository gets a deployment of the pushed commit, or, when it
// waits for a workflow, of the commit that workflow succeeded for.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := s.cfg.GitHub.WebhookSecret
	if secret == "" || s.pushDeployer == nil {
		writeError(w, http.StatusForbidden, "webhook_disabled", "webhooks are not configured: set [github] webhook_secret")
		return
	}
	if !s.webhookLimiter.allow(s.clock.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many webhook deliveries")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "set the webhook content type to application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "the delivery is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "unreadable_body", "the delivery could not be read")
		return
	}
	if err := github.VerifySignature([]byte(secret), body, r.Header.Get(github.SignatureHeader)); err != nil {
		// Neither the signature nor the body is logged.
		s.log.Warn("webhook delivery with a bad signature refused")
		writeError(w, http.StatusUnauthorized, "bad_signature", "the signature does not match the webhook secret")
		return
	}
	event := r.Header.Get(github.EventHeader)
	delivery := r.Header.Get(github.DeliveryHeader)
	if !eventNamePattern.MatchString(event) || !deliveryPattern.MatchString(delivery) {
		writeError(w, http.StatusBadRequest, "bad_headers", "X-GitHub-Event and X-GitHub-Delivery are required")
		return
	}
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "pong"})
		return
	case "push":
	case "workflow_run":
		s.handleWorkflowRun(w, r, body, delivery)
		return
	default:
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "event " + event + " is not used"})
		return
	}
	ev, err := github.ParsePushEvent(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_payload", "the push payload could not be read")
		return
	}
	branch, isBranch := ev.Branch()
	switch {
	case !isBranch:
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "not a branch"})
		return
	case ev.RemovesRef():
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "branch deleted"})
		return
	case !gitrepo.IsFullSHA(ev.After):
		writeError(w, http.StatusBadRequest, "bad_payload", "after is not a commit SHA")
		return
	}
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	targets := pushTargets(apps, ev.RepoID, ev.RepoFullName, branch)
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "no app deploys this branch on push"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookEnqueueTimeout)
	defer cancel()
	pusher := cleanLogin(ev.Pusher)
	resp := webhookResultJSON{Result: "deployed"}
	failed := false
	for _, app := range targets {
		res := pushResultJSON{App: app.Slug}
		keys := deliveryKeys(delivery, body, app)
		switch {
		case app.AutoDeployWorkflow != "":
			// A run of the workflow deploys it (handleWorkflowRun).
			res.Status = "waiting_workflow"
		case app.DesiredState == store.DesiredStopped:
			res.Status = "stopped"
		case !s.deliveries.reserve(s.clock.Now(), keys...):
			res.Status = "duplicate"
		default:
			d, err := s.pushDeployer.EnqueuePush(ctx, app, branch, ev.After, ev.HeadMessage, pusher)
			if err != nil {
				// Let a redelivery from GitHub try again.
				s.deliveries.release(keys...)
				failed = true
				res.Status, res.Error = "failed", enqueueErrorCode(err)
				s.log.Error("queueing a push deployment failed", "app", app.Slug, "error", err)
				break
			}
			res.Status, res.DeploymentID = "queued", d.ID
			s.log.Info("push deployment queued", "app", app.Slug, "deployment", d.ID, "branch", branch, "pusher", pusher)
		}
		resp.Apps = append(resp.Apps, res)
	}
	status := http.StatusOK
	if failed {
		// GitHub shows the delivery as failed, and it can be redelivered.
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// deliveryKeys are what a delivery is remembered by for app, to drop
// duplicates. GitHub redelivers with the same delivery ID and body. A copy
// of a signed body sent with another delivery ID is caught by the body.
func deliveryKeys(delivery string, body []byte, app store.App) []string {
	bodySum := sha256.Sum256(body)
	appKey := "/" + strconv.FormatInt(app.ID, 10)
	return []string{"delivery:" + delivery + appKey, "body:" + hex.EncodeToString(bodySum[:]) + appKey}
}

// pushTargets returns the apps that deploy pushes to branch of the
// repository: auto-deploy on, fetched through the GitHub App, following
// branch. The repository matches by ID when both sides know it (it
// survives renames), by full name otherwise, ignoring case.
func pushTargets(apps []store.App, repoID int64, repoFullName, branch string) []store.App {
	var out []store.App
	for _, app := range apps {
		if !app.AutoDeploy || !app.UsesGitHubApp() || app.EffectiveAutoDeployBranch() != branch {
			continue
		}
		if app.RepoID != nil && repoID != 0 {
			if *app.RepoID != repoID {
				continue
			}
		} else if !strings.EqualFold(app.RepoFullName, repoFullName) {
			continue
		}
		out = append(out, app)
	}
	return out
}

// handleWorkflowRun deploys the apps that wait for a workflow when a run
// of it succeeded for the head of their branch (ARCHITECTURE 5.2). A run
// that did not succeed leaves an entry in each such app's activity.
func (s *Server) handleWorkflowRun(w http.ResponseWriter, r *http.Request, body []byte, delivery string) {
	ev, err := github.ParseWorkflowRunEvent(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_payload", "the workflow_run payload could not be read")
		return
	}
	switch {
	case ev.Action != "completed":
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "the run has not completed"})
		return
	case !ev.FromHeadRepository():
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "the run is for a commit from another repository"})
		return
	case ev.HeadBranch == "":
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "the run has no branch"})
		return
	case !gitrepo.IsFullSHA(ev.HeadSHA):
		writeError(w, http.StatusBadRequest, "bad_payload", "head_sha is not a commit SHA")
		return
	}
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var targets []store.App
	for _, app := range pushTargets(apps, ev.RepoID, ev.RepoFullName, ev.HeadBranch) {
		if app.AutoDeployWorkflow != "" && app.AutoDeployWorkflow == ev.WorkflowPath {
			targets = append(targets, app)
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, webhookResultJSON{Result: "ignored", Reason: "no app waits for this workflow on this branch"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookEnqueueTimeout)
	defer cancel()
	pusher := cleanLogin(ev.Actor)
	resp := webhookResultJSON{Result: "deployed"}
	failed := false
	for _, app := range targets {
		res := pushResultJSON{App: app.Slug}
		keys := deliveryKeys(delivery, body, app)
		switch {
		case app.DesiredState == store.DesiredStopped:
			res.Status = "stopped"
		case !s.deliveries.reserve(s.clock.Now(), keys...):
			res.Status = "duplicate"
		case ev.Conclusion != "success":
			res.Status, res.Conclusion = "skipped", cleanLogin(ev.Conclusion)
			s.recordActivity(r, app.ID, actAutoDeploySkipped, map[string]any{
				"workflow":   ev.WorkflowPath,
				"conclusion": res.Conclusion,
				"commit_sha": ev.HeadSHA,
				"branch":     ev.HeadBranch,
				"run_url":    runURL(ev.RunURL),
			})
			s.log.Info("workflow did not succeed; not deploying", "app", app.Slug, "workflow", ev.WorkflowPath,
				"conclusion", res.Conclusion, "commit", ev.HeadSHA)
		default:
			head, message, err := s.branchHead(ctx, app, ev.HeadBranch)
			if err != nil {
				// Let a redelivery from GitHub try again.
				s.deliveries.release(keys...)
				failed = true
				res.Status, res.Error = "failed", "branch_unresolved"
				s.log.Error("reading the branch head for a workflow run failed", "app", app.Slug, "branch", ev.HeadBranch, "error", err)
				break
			}
			if head != ev.HeadSHA {
				// A newer push has its own run, which deploys it.
				res.Status = "stale"
				s.log.Info("workflow run is for an older commit; not deploying", "app", app.Slug,
					"workflow", ev.WorkflowPath, "commit", ev.HeadSHA, "head", head)
				break
			}
			d, err := s.pushDeployer.EnqueueWorkflowRun(ctx, app, ev.HeadBranch, ev.HeadSHA, message, pusher, ev.WorkflowPath)
			if err != nil {
				s.deliveries.release(keys...)
				failed = true
				res.Status, res.Error = "failed", enqueueErrorCode(err)
				s.log.Error("queueing a workflow run deployment failed", "app", app.Slug, "error", err)
				break
			}
			res.Status, res.DeploymentID = "queued", d.ID
			s.log.Info("workflow run deployment queued", "app", app.Slug, "deployment", d.ID,
				"branch", ev.HeadBranch, "workflow", ev.WorkflowPath, "pusher", pusher)
		}
		resp.Apps = append(resp.Apps, res)
	}
	status := http.StatusOK
	if failed {
		// GitHub shows the delivery as failed, and it can be redelivered.
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// branchHead asks GitHub (or git) for the commit at the head of branch
// now, rather than trusting the pushes koroku happened to see.
func (s *Server) branchHead(ctx context.Context, app store.App, branch string) (sha, message string, err error) {
	if s.refs == nil {
		return "", "", errors.New("no ref resolver is configured")
	}
	sha, message, err = s.refs.ResolveRef(ctx, app, branch)
	if err != nil {
		return "", "", err
	}
	return strings.ToLower(sha), message, nil
}

// runURL keeps a workflow run's link only when it is a plain https URL,
// since the console shows it as a link.
func runURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 512 {
		return ""
	}
	return raw
}

// enqueueErrorCode names a failed enqueue for the webhook response.
func enqueueErrorCode(err error) string {
	var diskFull *deploy.DiskFullError
	switch {
	case errors.Is(err, deploy.ErrBusy):
		return "busy"
	case errors.As(err, &diskFull):
		return "disk_full"
	default:
		return "internal"
	}
}

// cleanLogin keeps a GitHub login printable and short before it reaches
// the database, the deploy log and the console.
func cleanLogin(login string) string {
	var b strings.Builder
	for _, r := range login {
		if b.Len() >= 64 {
			break
		}
		if r > ' ' && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// deliverySet remembers recent webhook deliveries, in bounded memory, to
// drop duplicates. It is lost on restart.
type deliverySet struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	order []deliveryEntry
	limit int
	ttl   time.Duration
}

type deliveryEntry struct {
	key string
	at  time.Time
}

func newDeliverySet(limit int, ttl time.Duration) *deliverySet {
	return &deliverySet{seen: map[string]time.Time{}, limit: limit, ttl: ttl}
}

// reserve records keys and reports true, or reports false without
// recording anything when one of them was seen within the TTL.
func (d *deliverySet) reserve(now time.Time, keys ...string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evict(now)
	for _, k := range keys {
		if _, ok := d.seen[k]; ok {
			return false
		}
	}
	for _, k := range keys {
		d.seen[k] = now
		d.order = append(d.order, deliveryEntry{key: k, at: now})
	}
	d.evict(now)
	return true
}

// release forgets keys, for a delivery that did not go through.
func (d *deliverySet) release(keys ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range keys {
		delete(d.seen, k)
	}
}

// evict drops expired entries and the oldest ones beyond the limit.
func (d *deliverySet) evict(now time.Time) {
	for len(d.order) > 0 {
		oldest := d.order[0]
		if now.Sub(oldest.at) < d.ttl && len(d.order) <= d.limit {
			return
		}
		if at, ok := d.seen[oldest.key]; ok && at.Equal(oldest.at) {
			delete(d.seen, oldest.key)
		}
		d.order = d.order[1:]
	}
}
