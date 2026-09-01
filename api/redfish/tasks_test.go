package redfish

// tasks_test.go covers the minimal in-memory TaskService: the task lifecycle
// (Running → Completed / Exception), the registry's eviction policy (cap of
// 16, oldest completed first, running tasks never evicted, 24h lazy purge),
// and the HTTP surface operator tooling polls after a 202.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// taskRouter mounts the TaskService read surface over a fresh registry.
func taskRouter(t *testing.T) (*gin.Engine, *handlers) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := testHandlers()
	r := gin.New()
	r.GET(taskServicePath, h.GetTaskService)
	r.GET(tasksPath, h.GetTaskCollection)
	r.GET(tasksPath+"/:id", h.GetTask)
	return r, h
}

// waitForTask polls the task at uri (via sensors_test.go's getJSON helper) until it leaves Running, returning the
// final rendered document. Shared with the SimpleUpdate and InsertMedia tests,
// which poll exactly the way operator tooling does.
func waitForTask(t *testing.T, r *gin.Engine, uri string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := getJSON(t, r, uri)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", uri, code)
		}
		if s, _ := body["TaskState"].(string); s != "" && s != "Running" {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not leave Running", uri)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// taskMessages flattens the Messages array of a rendered task document.
func taskMessages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["Messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("Messages entry is %T, want an object", m)
		}
		out = append(out, mm)
	}
	return out
}

func TestTaskLifecycleToCompleted(t *testing.T) {
	reg := newTaskRegistry()
	tk := reg.newTask("Stage capsule")

	got := tk.render()
	if got.ID != "1" {
		t.Errorf("first task Id = %q, want 1", got.ID)
	}
	if got.TaskState != schemas.RunningTaskState {
		t.Errorf("TaskState = %q, want Running", got.TaskState)
	}
	if got.StartTime == "" {
		t.Error("StartTime is empty, want RFC3339 stamp")
	}
	if got.EndTime != "" {
		t.Errorf("EndTime = %q on a running task, want empty", got.EndTime)
	}
	if got.PercentComplete != nil {
		t.Errorf("PercentComplete = %v before any progress, want unset", *got.PercentComplete)
	}
	if !got.HidePayload {
		t.Error("HidePayload = false, want true")
	}

	tk.setPercent(37)
	if p := tk.render().PercentComplete; p == nil || *p != 37 {
		t.Errorf("PercentComplete = %v, want 37", p)
	}

	tk.complete(nil)
	got = tk.render()
	if got.TaskState != schemas.CompletedTaskState {
		t.Errorf("TaskState = %q, want Completed", got.TaskState)
	}
	if got.TaskStatus != schemas.OKHealth {
		t.Errorf("TaskStatus = %q, want OK", got.TaskStatus)
	}
	if got.EndTime == "" {
		t.Error("EndTime is empty on a completed task")
	}
}

func TestTaskLifecycleToException(t *testing.T) {
	reg := newTaskRegistry()
	tk := reg.newTask("Stage capsule")
	tk.complete(errors.New("fetch failed: remote returned 500"))

	got := tk.render()
	if got.TaskState != schemas.ExceptionTaskState {
		t.Errorf("TaskState = %q, want Exception", got.TaskState)
	}
	if got.TaskStatus != schemas.CriticalHealth {
		t.Errorf("TaskStatus = %q, want Critical", got.TaskStatus)
	}
	var found bool
	for _, m := range got.Messages {
		if strings.Contains(m.Message, "remote returned 500") {
			found = true
		}
	}
	if !found {
		t.Errorf("Messages = %+v, want one carrying the error text", got.Messages)
	}
}

func TestTaskRegistryCapEvictsOldestCompletedFirst(t *testing.T) {
	reg := newTaskRegistry()

	oldest := reg.newTask("t1") // id 1
	oldest.complete(nil)
	second := reg.newTask("t2") // id 2, completed later than t1
	second.complete(nil)

	// Fill to 17 total with running tasks: the cap of 16 must evict the
	// oldest completed task and nothing else.
	for i := 3; i <= 17; i++ {
		reg.newTask(fmt.Sprintf("t%d", i))
	}

	if got := len(reg.all()); got != 16 {
		t.Fatalf("registry holds %d tasks, want 16", got)
	}
	if reg.get("1") != nil {
		t.Error("oldest completed task survived the cap, want it evicted first")
	}
	if reg.get("2") == nil {
		t.Error("newer completed task was evicted before the oldest")
	}
	if reg.get("3") == nil {
		t.Error("a running task was evicted; only completed tasks may be")
	}
}

func TestTaskRegistryNeverEvictsRunningTasks(t *testing.T) {
	reg := newTaskRegistry()
	for i := 1; i <= 20; i++ {
		reg.newTask(fmt.Sprintf("t%d", i))
	}
	// All 20 are running: the cap cannot be enforced without killing a live
	// task, so the registry runs over instead.
	if got := len(reg.all()); got != 20 {
		t.Fatalf("registry holds %d tasks, want all 20 running tasks kept", got)
	}
}

func TestTaskRegistryPurgesStaleCompletedTasks(t *testing.T) {
	reg := newTaskRegistry()

	stale := reg.newTask("old")
	stale.complete(nil)
	stale.mu.Lock()
	stale.end = time.Now().Add(-25 * time.Hour)
	stale.mu.Unlock()

	fresh := reg.newTask("new")
	fresh.complete(nil)

	if reg.get("1") != nil {
		t.Error("completed task older than 24h survived a registry access")
	}
	if reg.get("2") == nil {
		t.Error("freshly completed task was purged, want it kept")
	}
}

func TestGetTaskServiceDocument(t *testing.T) {
	r, _ := taskRouter(t)

	code, body := getJSON(t, r, taskServicePath)
	if code != http.StatusOK {
		t.Fatalf("GET TaskService = %d, want 200", code)
	}
	if got, _ := body["@odata.type"].(string); got != odataTypeTaskService {
		t.Errorf("@odata.type = %q, want %q", got, odataTypeTaskService)
	}
	if v, _ := body["ServiceEnabled"].(bool); !v {
		t.Error("ServiceEnabled = false, want true")
	}
	if got, _ := body["CompletedTaskOverWritePolicy"].(string); got != "Oldest" {
		t.Errorf("CompletedTaskOverWritePolicy = %q, want Oldest", got)
	}
	if v, ok := body["LifeCycleEventOnTaskStateChange"].(bool); !ok || v {
		t.Errorf("LifeCycleEventOnTaskStateChange = %v, want false", body["LifeCycleEventOnTaskStateChange"])
	}
	tasks, _ := body["Tasks"].(map[string]any)
	if got, _ := tasks["@odata.id"].(string); got != tasksPath {
		t.Errorf("Tasks link = %q, want %q", got, tasksPath)
	}
}

func TestTaskCollectionAndMemberRender(t *testing.T) {
	r, h := taskRouter(t)
	tk := h.tasks.newTask("Stage capsule")

	code, body := getJSON(t, r, tasksPath)
	if code != http.StatusOK {
		t.Fatalf("GET Tasks = %d, want 200", code)
	}
	if got, _ := body["Members@odata.count"].(float64); got != 1 {
		t.Errorf("Members@odata.count = %v, want 1", body["Members@odata.count"])
	}

	memberURI := tasksPath + "/1"
	code, body = getJSON(t, r, memberURI)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", memberURI, code)
	}
	if got, _ := body["@odata.type"].(string); got != odataTypeTask {
		t.Errorf("@odata.type = %q, want %q", got, odataTypeTask)
	}
	if got, _ := body["Id"].(string); got != "1" {
		t.Errorf("Id = %q, want 1", got)
	}
	if got, _ := body["TaskState"].(string); got != "Running" {
		t.Errorf("TaskState = %q, want Running", got)
	}
	if v, _ := body["HidePayload"].(bool); !v {
		t.Error("HidePayload = false, want true")
	}

	tk.complete(nil)
	_, body = getJSON(t, r, memberURI)
	if got, _ := body["TaskState"].(string); got != "Completed" {
		t.Errorf("TaskState after complete = %q, want Completed", got)
	}
	if got, _ := body["TaskStatus"].(string); got != "OK" {
		t.Errorf("TaskStatus after complete = %q, want OK", got)
	}
}

func TestTaskUnknownIDReturns404(t *testing.T) {
	r, _ := taskRouter(t)

	code, body := getJSON(t, r, tasksPath+"/999")
	if code != http.StatusNotFound {
		t.Fatalf("GET unknown task = %d, want 404", code)
	}
	// The package's standard error shape (errors.go), not a bare body.
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["code"].(string); got != "Base.1.0.GeneralError" {
		t.Errorf("error.code = %q, want Base.1.0.GeneralError", got)
	}
}

func TestServiceRootExposesTasks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()
	r.GET(ServiceRootPath, svc.GetServiceRoot)

	code, body := getJSON(t, r, ServiceRootPath)
	if code != http.StatusOK {
		t.Fatalf("GET service root = %d, want 200", code)
	}
	tasks, _ := body["Tasks"].(map[string]any)
	if got, _ := tasks["@odata.id"].(string); got != taskServicePath {
		t.Errorf("service root Tasks = %q, want %q", got, taskServicePath)
	}
}
