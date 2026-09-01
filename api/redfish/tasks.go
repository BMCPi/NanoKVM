package redfish

// tasks.go implements a minimal, in-memory Redfish TaskService: just enough
// for the two long-running operations this BMC starts on a client's behalf
// (SimpleUpdate's capsule fetch, InsertMedia's URL fetch) to hand back a 202
// whose Location an operator tool can poll. No EventService, no persistence,
// no POST/DELETE on tasks.
//
// The registry is in-memory ONLY — a BMC restart forgets every task. That is
// deliberate: both operations a task tracks are themselves forgotten by a
// restart (the staging sentinel clears, the fetch dies with the process), so
// a persisted task could only ever mislead.
//
// Monitor semantics: the 202 responses point Location at the Task RESOURCE
// URI and return the Task resource as the 202 body. DSP0266 describes a
// separate task-monitor URI, but what Ansible's redfish_command, gofish and
// redfishtool actually do is read Location and poll TaskState on what they
// find there — so this BMC deviates on purpose and does not grow a
// /TaskMonitors/ tree.

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// maxTasks caps the registry. When a new task would push past it, the oldest
// COMPLETED task is evicted (CompletedTaskOverWritePolicy: "Oldest"); running
// tasks are never evicted, so the registry can briefly run over if somehow
// more than maxTasks operations are in flight at once.
const maxTasks = 16

// completedTaskTTL is how long a finished task stays pollable. Purged lazily
// on registry access — there is no background goroutine to leak.
const completedTaskTTL = 24 * time.Hour

// task is one tracked operation. Its fields are guarded by mu; every render
// copies out under the lock because this package's handlers run concurrently
// with the goroutine driving the task.
type task struct {
	mu       sync.Mutex
	id       string
	name     string
	state    schemas.TaskState
	start    time.Time
	end      time.Time
	percent  *int
	messages []Message
}

// setPercent records download progress, clamped to 0–100.
func (t *task) setPercent(p int) {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.percent = &p
}

// addMessage appends a Message to the task (e.g. the acceptance message the
// old bare-202 body used to carry).
func (t *task) addMessage(m Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messages = append(t.messages, m)
}

// complete finishes the task: Completed on a nil err, Exception otherwise
// (with a Message carrying the error text — the whole point of the task: a
// staging failure used to be visible only in the BMC log).
func (t *task) complete(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.end = time.Now()
	if err != nil {
		t.state = schemas.ExceptionTaskState
		t.messages = append(t.messages, Message{
			ODataType: "#Message.v1_1_0.Message",
			MessageID: "Base.1.0.GeneralError",
			Message:   err.Error(),
			Severity:  "Critical",
		})
		return
	}
	t.state = schemas.CompletedTaskState
}

// isDone reports whether the task has left Running. Used by the registry's
// purge under its own lock; lock order is always registry.mu then task.mu.
func (t *task) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state != schemas.RunningTaskState
}

// endedBefore reports whether the task finished before cutoff.
func (t *task) endedBefore(cutoff time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state != schemas.RunningTaskState && t.end.Before(cutoff)
}

// uri is the task's resource URI — what a 202's Location points at.
func (t *task) uri() string { return tasksPath + "/" + t.id }

// render copies the task out under its lock as the wire resource.
func (t *task) render() TaskResource {
	t.mu.Lock()
	defer t.mu.Unlock()

	res := TaskResource{
		Resource: Resource{
			ODataType:    odataTypeTask,
			ODataID:      tasksPath + "/" + t.id,
			ODataContext: odataContext("Task.Task"),
			ID:           t.id,
			Name:         t.name,
		},
		TaskState:   t.state,
		TaskStatus:  schemas.OKHealth,
		StartTime:   t.start.UTC().Format(time.RFC3339),
		Messages:    append([]Message{}, t.messages...),
		HidePayload: true,
	}
	if t.state == schemas.ExceptionTaskState {
		res.TaskStatus = schemas.CriticalHealth
	}
	if !t.end.IsZero() {
		res.EndTime = t.end.UTC().Format(time.RFC3339)
	}
	if t.percent != nil {
		p := *t.percent
		res.PercentComplete = &p
	}
	return res
}

// taskRegistry owns every live task. One is created per Register call and
// hangs off the handlers struct — never a package-level singleton — which is
// also what keeps tests isolated from each other.
type taskRegistry struct {
	mu     sync.Mutex
	nextID int
	tasks  map[string]*task
	order  []string // insertion order, for "Oldest" eviction and stable listings

	// mediaBusy is the single-in-flight guard for InsertMedia URL fetches,
	// the media path's analogue of firmware.IsStaging.
	mediaBusy bool
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{tasks: make(map[string]*task)}
}

// newTask registers a Running task and enforces the eviction policy.
func (r *taskRegistry) newTask(name string) *task {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	t := &task{
		id:    strconv.Itoa(r.nextID),
		name:  name,
		state: schemas.RunningTaskState,
		start: time.Now(),
	}
	r.tasks[t.id] = t
	r.order = append(r.order, t.id)
	r.purgeLocked()
	return t
}

// get returns the task with the given id, or nil.
func (r *taskRegistry) get(id string) *task {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked()
	return r.tasks[id]
}

// all returns the live tasks in insertion order.
func (r *taskRegistry) all() []*task {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked()
	out := make([]*task, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.tasks[id])
	}
	return out
}

// beginMediaInsert claims the single InsertMedia fetch slot; false means one
// is already in flight and the caller must 409.
func (r *taskRegistry) beginMediaInsert() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mediaBusy {
		return false
	}
	r.mediaBusy = true
	return true
}

// endMediaInsert releases the slot claimed by beginMediaInsert.
func (r *taskRegistry) endMediaInsert() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mediaBusy = false
}

// purgeLocked applies both retention rules under r.mu: completed tasks older
// than completedTaskTTL go first, then — while over maxTasks — the oldest
// completed tasks. A running task is never evicted, whatever the count.
func (r *taskRegistry) purgeLocked() {
	cutoff := time.Now().Add(-completedTaskTTL)
	kept := r.order[:0]
	for _, id := range r.order {
		if r.tasks[id].endedBefore(cutoff) {
			delete(r.tasks, id)
			continue
		}
		kept = append(kept, id)
	}
	r.order = kept

	for len(r.order) > maxTasks {
		evicted := false
		for i, id := range r.order {
			if r.tasks[id].isDone() {
				delete(r.tasks, id)
				r.order = append(r.order[:i], r.order[i+1:]...)
				evicted = true
				break
			}
		}
		if !evicted {
			return // everything left is running; run over rather than kill one
		}
	}
}

// acceptedTask writes the 202 every task-backed action returns: Location
// pointing at the task resource, with the rendered task as the body. See the
// file header for why Location is the resource URI rather than a DSP0266
// task-monitor URI.
func acceptedTask(c *gin.Context, t *task) {
	c.Header("Location", t.uri())
	c.JSON(http.StatusAccepted, t.render())
}

// ---------------------------------------------------------------------------
// Wire resources

// TaskServiceResource is the TaskService root document.
type TaskServiceResource struct {
	Resource
	ServiceEnabled                  bool                               `json:"ServiceEnabled"`
	CompletedTaskOverWritePolicy    schemas.TaskServiceOverWritePolicy `json:"CompletedTaskOverWritePolicy"`
	LifeCycleEventOnTaskStateChange bool                               `json:"LifeCycleEventOnTaskStateChange"`
	Status                          *Status                            `json:"Status,omitempty"`
	Tasks                           Link                               `json:"Tasks"`
}

// TaskResource is one Task document. Messages is always present (an empty
// array beats an absent property for pollers) and HidePayload is always true:
// this service never records the originating request payload.
type TaskResource struct {
	Resource
	TaskState       schemas.TaskState `json:"TaskState"`
	TaskStatus      schemas.Health    `json:"TaskStatus"`
	StartTime       string            `json:"StartTime,omitempty"`
	EndTime         string            `json:"EndTime,omitempty"`
	PercentComplete *int              `json:"PercentComplete,omitempty"`
	Messages        []Message         `json:"Messages"`
	HidePayload     bool              `json:"HidePayload"`
}

// ---------------------------------------------------------------------------
// Handlers

// GetTaskService returns the TaskService root.
func (h *handlers) GetTaskService(c *gin.Context) {
	c.JSON(http.StatusOK, TaskServiceResource{
		Resource: Resource{
			ODataType:    odataTypeTaskService,
			ODataID:      taskServicePath,
			ODataContext: odataContext("TaskService.TaskService"),
			ID:           "TaskService",
			Name:         "Task Service",
			Description:  "In-memory tasks for BMC-side downloads; forgotten at restart",
		},
		ServiceEnabled:                  true,
		CompletedTaskOverWritePolicy:    schemas.OldestTaskServiceOverWritePolicy,
		LifeCycleEventOnTaskStateChange: false,
		Status:                          &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		Tasks:                           Link(tasksPath),
	})
}

// GetTaskCollection returns the Task collection.
func (h *handlers) GetTaskCollection(c *gin.Context) {
	tasks := h.tasks.all()
	links := make(Links, 0, len(tasks))
	for _, t := range tasks {
		links = append(links, Link(t.uri()))
	}
	c.JSON(http.StatusOK, newCollection(
		"TaskCollection", "Task Collection", tasksPath,
		links...,
	))
}

// GetTask returns one Task, or the package's standard 404 shape.
func (h *handlers) GetTask(c *gin.Context) {
	t := h.tasks.get(c.Param("id"))
	if t == nil {
		redfishErrorResponse(c, http.StatusNotFound, "no such task")
		return
	}
	c.JSON(http.StatusOK, t.render())
}
