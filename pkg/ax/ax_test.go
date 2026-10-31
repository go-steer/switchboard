// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ax

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/go-steer/switchboard/internal/axapi"
)

// fakeAX is ax-server as far as Ensure and Delete go: immutable tasks, a
// second create of a name refused, resume and delete by name.
type fakeAX struct {
	axapi.UnimplementedAXServer
	mu       sync.Mutex
	tasks    map[string]*axapi.Task // atespace/name
	creates  []*axapi.Task
	resumes  []string
	deletes  []string
	createEr error // fails every create with this, task or not
}

func key(atespace, name string) string { return atespace + "/" + name }

func (f *fakeAX) CreateTask(_ context.Context, r *axapi.CreateTaskRequest) (*axapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, proto.Clone(r.Task).(*axapi.Task))
	if f.createEr != nil {
		return nil, f.createEr
	}
	k := key(r.Task.Metadata.Atespace, r.Task.Metadata.Name)
	if _, ok := f.tasks[k]; ok {
		return nil, status.Errorf(codes.FailedPrecondition, "task %s already exists and is immutable", k)
	}
	saved := proto.Clone(r.Task).(*axapi.Task)
	saved.Status = &axapi.TaskStatus{Phase: "Suspended"}
	f.tasks[k] = saved
	return saved, nil
}

func (f *fakeAX) GetTask(_ context.Context, r *axapi.GetTaskRequest) (*axapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[key(r.Atespace, r.Name)]; ok {
		return t, nil
	}
	return nil, status.Error(codes.NotFound, "no such task")
}

func (f *fakeAX) ResumeTask(_ context.Context, r *axapi.ResumeTaskRequest) (*axapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes = append(f.resumes, key(r.Atespace, r.Name))
	if t, ok := f.tasks[key(r.Atespace, r.Name)]; ok {
		t.Status = &axapi.TaskStatus{Phase: "Running"}
		return t, nil
	}
	return nil, status.Error(codes.NotFound, "no such task")
}

func (f *fakeAX) ListTasks(_ context.Context, r *axapi.ListTasksRequest) (*axapi.ListTasksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for k := range f.tasks {
		names = append(names, k)
	}
	sort.Strings(names)
	resp := &axapi.ListTasksResponse{}
	for i, k := range names {
		if int64(i) < r.Offset || int64(len(resp.Tasks)) >= r.Limit {
			continue
		}
		resp.Tasks = append(resp.Tasks, f.tasks[k])
	}
	return resp, nil
}

func (f *fakeAX) DeleteTask(_ context.Context, r *axapi.DeleteTaskRequest) (*axapi.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(r.Atespace, r.Name)
	if _, ok := f.tasks[k]; !ok {
		return nil, status.Error(codes.NotFound, "no such task")
	}
	delete(f.tasks, k)
	f.deletes = append(f.deletes, k)
	return &axapi.DeleteTaskResponse{}, nil
}

// fakeRouter answers /readyz for an actor: 503 notReady times, then 200.
type fakeRouter struct {
	mu       sync.Mutex
	notReady int
	actors   []string
}

func (r *fakeRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actors = append(r.actors, req.Header.Get(ActorHeader))
	if req.URL.Path != "/readyz" {
		http.NotFound(w, req)
		return
	}
	if r.notReady > 0 {
		r.notReady--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

const testTemplate = `apiVersion: ax.io/v1alpha1
kind: Task
spec:
  image: agent-mast
  command: ["mast", "--attach-listen=127.0.0.1:8484"]
  env:
    - name: MAST_ATTACH_TOKEN
      value: t0ken
  http:
    port: 8484
  idle:
    suspendAfter: 10m
`

func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "task.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fixture wires a Client to a fake ax-server over bufconn and a fake router.
func fixture(t *testing.T) (*Client, *fakeAX, *fakeRouter) {
	t.Helper()
	ax, conn := fakeServer(t)

	router := &fakeRouter{}
	rs := httptest.NewServer(router)
	t.Cleanup(rs.Close)

	tmpl, err := LoadTemplate(writeTemplate(t, testTemplate))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Router: rs.URL, Atespace: "agents", Template: tmpl, Conn: conn, ReadyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fastPoll(t)
	return c, ax, router
}

// fastPoll shortens the readiness poll for the test.
func fastPoll(t *testing.T) {
	old := readyPoll
	readyPoll = 10 * time.Millisecond
	t.Cleanup(func() { readyPoll = old })
}

// fakeServer is a fake ax-server and a connection to it over bufconn.
func fakeServer(t *testing.T) (*fakeAX, *grpc.ClientConn) {
	t.Helper()
	ax := &fakeAX{tasks: map[string]*axapi.Task{}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	axapi.RegisterAXServer(srv, ax)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ax, conn
}

// Ensure creates the task from the template — only its name and atespace
// filled in — resumes it, and waits until the router says it is ready.
func TestEnsureCreatesResumesAndWaits(t *testing.T) {
	c, ax, router := fixture(t)
	router.notReady = 3

	if _, err := c.Ensure(context.Background(), "sb-triage-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(ax.creates) != 1 {
		t.Fatalf("creates = %d", len(ax.creates))
	}
	got := ax.creates[0]
	if got.Metadata.Name != "sb-triage-0123456789abcdef" || got.Metadata.Atespace != "agents" ||
		got.Spec.Image != "agent-mast" || got.Spec.Http.Port != 8484 || got.Kind != "Task" {
		t.Errorf("created %v", got)
	}
	if len(ax.resumes) != 1 || ax.resumes[0] != "agents/sb-triage-0123456789abcdef" {
		t.Errorf("resumes = %v", ax.resumes)
	}
	if len(router.actors) != 4 || router.actors[3] != "agents/sb-triage-0123456789abcdef" {
		t.Errorf("router saw %v, want 4 readiness polls for the task's actor", router.actors)
	}
}

// A task that already exists is reused: ax refuses the create (tasks are
// immutable), the client asks, and carries on.
func TestEnsureReusesAnExistingTask(t *testing.T) {
	c, ax, _ := fixture(t)
	for range 2 {
		if _, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
	}
	if len(ax.creates) != 2 || len(ax.resumes) != 1 || len(ax.tasks) != 1 {
		t.Errorf("creates %d, resumes %d, tasks %d; want the second create refused, the running task reused and not resumed again",
			len(ax.creates), len(ax.resumes), len(ax.tasks))
	}
}

// A create that fails for any other reason is the error, not a reuse.
func TestACreateFailureIsReported(t *testing.T) {
	c, ax, _ := fixture(t)
	ax.createEr = status.Error(codes.Unavailable, "redis is down")
	_, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef")
	if err == nil || !strings.Contains(err.Error(), "redis is down") {
		t.Errorf("err = %v", err)
	}
}

// A task that never answers runs out the budget, saying what it last heard.
func TestATaskThatNeverAnswersTimesOut(t *testing.T) {
	c, _, router := fixture(t)
	router.notReady = 1 << 30
	// Room for many 10ms polls: at 100ms a slow CI runner's first request
	// was still open at the deadline, so the last answer was its timeout,
	// not the 503 (seen on #176).
	c.cfg.ReadyTimeout = 750 * time.Millisecond
	_, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the deadline and the last status", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	c, ax, _ := fixture(t)
	if _, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.Delete(context.Background(), "sb-x-0123456789abcdef"); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if len(ax.deletes) != 1 || len(ax.tasks) != 0 {
		t.Errorf("deletes %v, tasks %d", ax.deletes, len(ax.tasks))
	}
}

// Every request through a task's client carries the actor header, and the
// caller's request is left untouched.
func TestTheTaskClientCarriesTheActor(t *testing.T) {
	c, _, router := fixture(t)
	req, _ := http.NewRequest(http.MethodGet, c.Router()+"/readyz", nil)
	resp, err := c.HTTPClient("sb-y-0123456789abcdef").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if router.actors[0] != "agents/sb-y-0123456789abcdef" {
		t.Errorf("actor = %q", router.actors[0])
	}
	if req.Header.Get(ActorHeader) != "" {
		t.Error("the caller's request was modified")
	}
}

func TestTemplatesAreValidated(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no http port":  {"kind: Task\nspec:\n  image: x\n", "spec.http.port is required"},
		"no spec":       {"kind: Task\n", "spec is required"},
		"another kind":  {"kind: Workspace\nspec:\n  image: x\n", "want Task"},
		"unknown field": {"kind: Task\nspec:\n  image: x\n  http:\n    port: 1\n  imagee: y\n", "imagee"},
	} {
		if _, err := LoadTemplate(writeTemplate(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	tmpl, err := LoadTemplate(writeTemplate(t, "kind: Task\nspec:\n  http:\n    port: 1\n"))
	if err != nil || tmpl.Idle {
		t.Errorf("a template without idle: %+v, %v; want loaded, Idle false", tmpl, err)
	}
}

func TestConfigIsValidated(t *testing.T) {
	tmpl, _ := LoadTemplate(writeTemplate(t, testTemplate))
	for name, cfg := range map[string]Config{
		"no template":      {Server: "ax:8080", Router: "http://r"},
		"no server":        {Router: "http://r", Template: tmpl},
		"a router path":    {Server: "ax:8080", Router: "http://r/x", Template: tmpl},
		"a router slash":   {Server: "ax:8080", Router: "http://r/", Template: tmpl},
		"no router scheme": {Server: "ax:8080", Router: "atenet-router:80", Template: tmpl},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Names are deterministic, valid, and carry no conversation or caller.
func TestTaskNames(t *testing.T) {
	a := TaskName("sb-Triage", "slack", "C0123:1700000000.000100")
	if a != TaskName("sb-Triage", "slack", "C0123:1700000000.000100") {
		t.Error("not deterministic")
	}
	if a == TaskName("sb-Triage", "slackC0123", ":1700000000.000100") {
		t.Error("part boundaries do not count")
	}
	if !strings.HasPrefix(a, "sb-triage-") || ValidName(a) != nil || strings.Contains(a, "c0123") {
		t.Errorf("name %q", a)
	}
	long := TaskName(strings.Repeat("agent-", 20), "x")
	if ValidName(long) != nil || len(long) > 63 {
		t.Errorf("long name %q (%d)", long, len(long))
	}
	if n := TaskName("!!!", "x"); ValidName(n) != nil {
		t.Errorf("an unsluggable prefix gave %q", n)
	}
	if err := ValidName("Has_Upper"); err == nil {
		t.Error("an invalid name passed")
	}
}

// A task created from an older template is reused, and says so: tasks are
// immutable, so a rotated token never reaches it (caught in review).
func TestEnsureReportsATemplateThatChanged(t *testing.T) {
	c, ax, _ := fixture(t)
	ax.tasks["agents/sb-x-0123456789abcdef"] = &axapi.Task{
		Metadata: &axapi.ObjectMeta{Name: "sb-x-0123456789abcdef", Atespace: "agents"},
		Spec:     &axapi.TaskSpec{Image: "an-older-image", Http: &axapi.TaskHTTP{Port: 8484}},
		Status:   &axapi.TaskStatus{Phase: "Suspended"},
	}
	got, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if got.Created || !got.Drifted {
		t.Errorf("Ensured = %+v, want reused and drifted", got)
	}
	again, _ := c.Ensure(context.Background(), "sb-y-0123456789abcdef")
	if !again.Created || again.Drifted {
		t.Errorf("a new task: %+v", again)
	}
}

// A task being deleted is not resurrected (caught in review).
func TestEnsureLeavesATerminatingTaskAlone(t *testing.T) {
	c, ax, _ := fixture(t)
	ax.tasks["agents/sb-x-0123456789abcdef"] = &axapi.Task{
		Metadata: &axapi.ObjectMeta{Name: "sb-x-0123456789abcdef"},
		Status:   &axapi.TaskStatus{Phase: "Terminating"},
	}
	if _, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("err = %v", err)
	}
	if len(ax.resumes) != 0 {
		t.Errorf("resumed a terminating task: %v", ax.resumes)
	}
}

// Only the template's spec goes into a task: a status or creation time in it
// would give every task one identity (caught in review).
func TestTheTemplatesStatusIsNotCopied(t *testing.T) {
	c, ax, _ := fixture(t)
	tmpl, err := LoadTemplate(writeTemplate(t, testTemplate+"status:\n  id: shared\n  phase: Running\nmetadata:\n  name: fixed\n  createTime: \"2026-01-01T00:00:00Z\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.Template = tmpl
	if _, err := c.Ensure(context.Background(), "sb-x-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	got := ax.creates[0]
	if got.GetStatus() != nil || got.GetMetadata().GetCreateTime() != nil || got.GetMetadata().GetName() != "sb-x-0123456789abcdef" {
		t.Errorf("created %v", got)
	}
}

// Strict means strict: what ax would refuse at every thread fails here, at
// startup (caught in review).
func TestTemplatesAreStrict(t *testing.T) {
	ok := "kind: Task\nspec:\n  http:\n    port: 8484\n"
	for name, tc := range map[string]struct{ body, want string }{
		"a duplicate key":      {"kind: Task\nspec:\n  image: a\n  image: b\n  http:\n    port: 8484\n", "image"},
		"a second document":    {ok + "---\nkind: Task\nspec:\n  image: other\n", "one document"},
		"the runner's port":    {"kind: Task\nspec:\n  http:\n    port: 80\n", "runner's own ports"},
		"the metadata port":    {"kind: Task\nspec:\n  http:\n    port: 8099\n", "runner's own ports"},
		"an impossible port":   {"kind: Task\nspec:\n  http:\n    port: 70000\n", "not a valid port"},
		"a short idle":         {ok + "  idle:\n    suspendAfter: 1s\n", "minimum"},
		"a bad idle":           {ok + "  idle:\n    suspendAfter: banana\n", "suspendAfter"},
		"idle with no timeout": {ok + "  idle:\n    busyPath: /busy\n", "required"},
		"a relative busy path": {ok + "  idle:\n    suspendAfter: 30s\n    busyPath: busy\n", "must start with /"},
	} {
		if _, err := LoadTemplate(writeTemplate(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if _, err := LoadTemplate(writeTemplate(t, "---\n# a leading separator is one document\n"+ok)); err != nil {
		t.Errorf("a leading separator: %v", err)
	}
}

// Two prefixes that make the same name-safe slug still name two tasks
// (caught in review: "sb-triage-" and "sb-triage" shared one).
func TestPrefixesThatSlugAlikeDoNotCollide(t *testing.T) {
	for _, pair := range [][2]string{{"sb-triage-", "sb-triage"}, {"sb-sre--triage", "sb-sre-triage"}} {
		if TaskName(pair[0], "slack", "C1:1") == TaskName(pair[1], "slack", "C1:1") {
			t.Errorf("%q and %q name one task", pair[0], pair[1])
		}
	}
}

// Requests to a task follow no redirect, whatever answers: the bearer and the
// asserted caller would go along (caught in review).
func TestTheTaskClientFollowsNoRedirect(t *testing.T) {
	var followed int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed++ }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	tmpl, _ := LoadTemplate(writeTemplate(t, testTemplate))
	c, err := New(Config{Server: "ax:8080", Router: redirector.URL, Template: tmpl, HTTPClient: nil})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.HTTPClient("sb-x-0123456789abcdef").Get(redirector.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if followed != 0 || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("followed %d, status %d; want the 307 itself", followed, resp.StatusCode)
	}
	slow := &http.Client{Timeout: time.Second}
	c2, _ := New(Config{Server: "ax:8080", Router: redirector.URL, Template: tmpl, HTTPClient: slow})
	if c2.HTTPClient("t").Timeout != 0 {
		t.Error("a client timeout reached the SSE clients")
	}
}

func TestMoreConfigIsValidated(t *testing.T) {
	tmpl, _ := LoadTemplate(writeTemplate(t, testTemplate))
	for name, cfg := range map[string]Config{
		"an empty template": {Server: "ax:8080", Router: "http://r", Template: &Template{}},
		"a bad atespace":    {Server: "ax:8080", Router: "http://r", Atespace: "Not_Valid", Template: tmpl},
		"a router query":    {Server: "ax:8080", Router: "http://r?", Template: tmpl},
		"a router fragment": {Server: "ax:8080", Router: "http://r#", Template: tmpl},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// List pages through ax's tasks and keeps the ones that match.
func TestListPagesAndFilters(t *testing.T) {
	c, ax, _ := fixture(t)
	for i := range 250 {
		name := fmt.Sprintf("other-%03d", i)
		if i%50 == 0 {
			name = TaskName("sb-triage", fmt.Sprint(i))
		}
		ax.tasks["agents/"+name] = &axapi.Task{Metadata: &axapi.ObjectMeta{Name: name}, Status: &axapi.TaskStatus{Phase: "Suspended"}}
	}
	got, err := c.List(context.Background(), NameMatcher("sb-triage"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0].Phase != "Suspended" {
		t.Errorf("got %d tasks (%+v), want the 5 of ours across pages", len(got), got)
	}
}

// NameMatcher recognizes exactly the names TaskName gives for a prefix.
func TestNameMatcher(t *testing.T) {
	m := NameMatcher("sb-triage")
	for name, want := range map[string]bool{
		TaskName("sb-triage", "slack", "C1:1"):    true,
		TaskName("sb-triage-u", "owner", "alice"): false, // another prefix
		"sb-triage-0123456789abcdeg":              false, // not hex
		"sb-triage-0123456789abcde":               false, // too short
		"lookout-0123456789abcdef":                false,
	} {
		if m(name) != want {
			t.Errorf("%s: %v, want %v", name, !want, want)
		}
	}
	if !NameMatcher("sb-triage-")(TaskName("sb-triage-", "x")) {
		t.Error("a prefix that slugs differently does not match its own names")
	}
}
