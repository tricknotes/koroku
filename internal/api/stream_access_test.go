package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// fastStreams makes log streams poll, send heartbeats and ask about their
// user's access every few milliseconds.
func fastStreams(t *testing.T) {
	t.Helper()
	poll, beat := logPollInterval, logHeartbeatInterval
	logPollInterval, logHeartbeatInterval = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { logPollInterval, logHeartbeatInterval = poll, beat })
}

// openStream is the client side of a log stream that is being read.
type openStream struct {
	t      *testing.T
	what   string
	beats  chan struct{} // one per heartbeat comment
	closed chan struct{} // closed when the server ends the stream
	sawEnd atomic.Bool   // an "end" event was sent
}

// stream opens an SSE endpoint with the given request headers.
func stream(t *testing.T, ts *httptest.Server, what, path string, header http.Header) *openStream {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%s: opening %s: %d %s", what, path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &openStream{t: t, what: what, beats: make(chan struct{}, 1), closed: make(chan struct{})}
	go func() {
		defer close(s.closed)
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			switch {
			case strings.HasPrefix(lines.Text(), ": keep-alive"):
				select {
				case s.beats <- struct{}{}:
				default:
				}
			case lines.Text() == "event: end":
				s.sawEnd.Store(true)
			}
		}
	}()
	return s
}

// staysOpen waits for heartbeats that start after the call, each of which
// asked whether the user may still read the stream.
func (s *openStream) staysOpen() {
	s.t.Helper()
	select {
	case <-s.beats:
	default:
	}
	for range 3 {
		select {
		case <-s.beats:
		case <-s.closed:
			s.t.Fatalf("%s: the stream ended", s.what)
		case <-time.After(5 * time.Second):
			s.t.Fatalf("%s: no heartbeat", s.what)
		}
	}
}

func (s *openStream) ends() {
	s.t.Helper()
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		s.t.Errorf("%s: the stream stayed open", s.what)
	}
	if s.sawEnd.Load() {
		s.t.Errorf("%s: the stream reported the end of the job", s.what)
	}
}

func cookiesOf(b *browser) http.Header {
	h := http.Header{}
	for _, c := range b.cookies {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	return h
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

type streamFixture struct {
	*roleFixture
	ts *httptest.Server
}

// newStreamFixture is the role fixture with log streams that check often.
// The viewer also has a role on shop, so that losing blog leaves its
// session and tokens valid.
func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	fastStreams(t)
	f := newRoleFixture(t, func(o *Options) { o.Logs = &fakeLogs{} })
	if err := f.srv.store.SetAppRole(context.Background(), f.app(t, "shop").ID, viewerID, store.RoleView); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	return &streamFixture{roleFixture: f, ts: ts}
}

func (f *streamFixture) ownerDoes(t *testing.T, method, path, body string, want int) {
	t.Helper()
	if rec := f.owner.do(method, path, body, originHeader); rec.Code != want {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
	}
}

func (f *streamFixture) token(t *testing.T, b *browser) createdToken {
	t.Helper()
	rec := b.do(http.MethodPost, "/api/tokens", `{"name":"laptop","expires_in_days":7}`, originHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	return decode[createdToken](t, rec)
}

const liveLogsPath = "/api/apps/blog/logs"

// A live log stream is authorized when it is opened and then for as long as
// it lasts: it ends at the next heartbeat after the role, the session or
// the token it was opened with is gone.
func TestLiveLogStreamEndsWithTheRole(t *testing.T) {
	f := newStreamFixture(t)
	viewer := stream(t, f.ts, "viewer", liveLogsPath, cookiesOf(f.as[viewerID]))
	viewerToken := stream(t, f.ts, "viewer's token", liveLogsPath, bearer(f.token(t, f.as[viewerID]).Value))
	manager := stream(t, f.ts, "manager", liveLogsPath, cookiesOf(f.as[managerID]))
	deployer := stream(t, f.ts, "deployer", liveLogsPath, cookiesOf(f.as[deployerID]))
	owner := stream(t, f.ts, "owner", liveLogsPath, cookiesOf(f.owner))
	for _, s := range []*openStream{viewer, viewerToken, manager, deployer, owner} {
		s.staysOpen()
	}

	// The viewer loses blog and keeps shop: its session and token stay
	// valid, the streams of blog end.
	f.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/api/members/%d/roles/blog", viewerID), "", http.StatusOK)
	viewer.ends()
	viewerToken.ends()
	if rec := f.as[viewerID].do(http.MethodGet, "/api/apps/shop", "", nil); rec.Code != http.StatusOK {
		t.Errorf("the viewer's session must still work for shop: %d %s", rec.Code, rec.Body)
	}

	// A lower role that still allows reading keeps the stream.
	f.ownerDoes(t, http.MethodPut, fmt.Sprintf("/api/members/%d/roles/blog", managerID), `{"role":"view"}`, http.StatusOK)
	manager.staysOpen()

	// A removed member loses everything.
	f.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/api/members/%d", deployerID), "", http.StatusNoContent)
	deployer.ends()

	manager.staysOpen()
	owner.staysOpen()
}

func TestLiveLogStreamEndsWithTheSession(t *testing.T) {
	f := newStreamFixture(t)
	manager := f.as[managerID]
	revoked := stream(t, f.ts, "manager", liveLogsPath, cookiesOf(manager))
	owner := stream(t, f.ts, "owner", liveLogsPath, cookiesOf(f.owner))
	revoked.staysOpen()
	owner.staysOpen()

	// The owner ends the manager's session.
	var list struct {
		Sessions []sessionJSON `json:"sessions"`
	}
	if err := json.Unmarshal(manager.do(http.MethodGet, "/api/sessions", "", nil).Body.Bytes(), &list); err != nil || len(list.Sessions) != 1 {
		t.Fatalf("sessions: %+v, %v", list, err)
	}
	f.ownerDoes(t, http.MethodDelete, "/api/sessions/"+list.Sessions[0].ID, "", http.StatusNoContent)
	revoked.ends()
	owner.staysOpen()

	// Sessions expire; the stream of an expired one ends, the owner's too.
	f.clk.Advance(SessionLifetime)
	owner.ends()
}

func TestLiveLogStreamEndsWithTheToken(t *testing.T) {
	f := newStreamFixture(t)
	revokedToken, expiringToken := f.token(t, f.owner), f.token(t, f.owner)
	revoked := stream(t, f.ts, "revoked token", liveLogsPath, bearer(revokedToken.Value))
	expiring := stream(t, f.ts, "expiring token", liveLogsPath, bearer(expiringToken.Value))
	revoked.staysOpen()
	expiring.staysOpen()

	f.ownerDoes(t, http.MethodDelete, "/api/tokens/"+revokedToken.Token.ID, "", http.StatusNoContent)
	revoked.ends()
	expiring.staysOpen()

	f.clk.Advance(7*24*time.Hour + time.Second)
	expiring.ends()
}

// The logs of a deployment and of a command follow a job that can run for a
// long time, so they are bounded the same way.
func TestJobLogStreamsEndWithTheRole(t *testing.T) {
	f := newStreamFixture(t)
	ctx := context.Background()
	blog := f.app(t, "blog")
	d, _, err := f.srv.store.EnqueueDeployment(ctx, blog.ID, "main", testSHA, "first", f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.srv.store.StartTaskRun(ctx, store.TaskRun{AppID: blog.ID, Service: "web", Command: "sleep 3600"}, f.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"deployment log": fmt.Sprintf("/api/deployments/%d/logs", d.ID),
		"task run log":   fmt.Sprintf("/api/task-runs/%d/logs", run.ID),
	}
	var viewers, owners []*openStream
	for what, path := range paths {
		viewers = append(viewers, stream(t, f.ts, "viewer's "+what, path, cookiesOf(f.as[viewerID])))
		owners = append(owners, stream(t, f.ts, "owner's "+what, path, cookiesOf(f.owner)))
	}
	for _, s := range append(viewers, owners...) {
		s.staysOpen()
	}

	f.ownerDoes(t, http.MethodDelete, fmt.Sprintf("/api/members/%d/roles/blog", viewerID), "", http.StatusOK)
	for _, s := range viewers {
		s.ends()
	}
	for _, s := range owners {
		s.staysOpen()
	}
}

// Without authentication there is no access to lose.
func TestLogStreamsStayOpenInNoAuthMode(t *testing.T) {
	fastStreams(t)
	srv := newLogsServer(t, &fakeLogs{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	stream(t, ts, "no-auth", liveLogsPath, http.Header{}).staysOpen()
}
