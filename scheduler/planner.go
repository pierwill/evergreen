package scheduler

import (
	"crypto/sha1"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/model/task"
)

// UnitCache stores an unordered collection of schedulable units. The
// Unit type holds one or more tasks, but is handled by the scheduler
// as a single object. While the constituent tasks in a unit have an
// order, the unit themselves are an intermediate abstraction for the
// planner which represent task groups, tasks with their dependencies,
// or the tasks from a single version
type UnitCache map[string]*Unit

// AddWhen wraps AddNew, and is a noop if the conditional is false.
func (cache UnitCache) AddWhen(cond bool, id string, t task.Task) {
	if !cond {
		return
	}

	if existing, ok := cache[id]; ok {
		existing.Add(t)
		return
	}

	cache.Create(id, t)
}

// AddNew adds an entire unit to a cache with the specified ID. If the
// cached item exists, AddNew extends the existing unit with the tasks
// from the passed unit.
func (cache UnitCache) AddNew(id string, unit *Unit) {
	if existing, ok := cache[id]; ok {
		for _, t := range unit.tasks {
			existing.Add(t)
		}

		return
	}

	cache[id] = unit
}

func (cache UnitCache) Exists(key string) bool { _, ok := cache[key]; return ok }

// Create makes a new unit around the existing task, caching it with
// the specified key, and returning the resulting unit. If there is an
// existing cache item with the specified ID, then Create extends that
// unit with this task. In both cases, the resulting unit is returned
// to the caller.
func (cache UnitCache) Create(id string, t task.Task) *Unit {
	if unit, ok := cache[id]; ok {
		unit.Add(t)
		return unit
	}

	unit := NewUnit(t)
	cache.AddNew(id, unit)
	return unit
}

// Export returns an unordered sequence of unique Units.
func (cache UnitCache) Export() TaskPlan {
	seen := StringSet{}
	tpl := TaskPlan{}
	for id := range cache {
		if seen.Visit(cache[id].ID()) {
			continue
		}

		if cache[id].distro == nil {
			continue
		}

		tpl = append(tpl, cache[id])
	}

	return tpl
}

// Unit is a holder of a group of related tasks which should be
// scheculded together. Typically these represent task groups, tasks,
// and their dependencies, or even all tasks of a version. All tasks
// in a Unit must be unique with regards to their ID.
type Unit struct {
	tasks       map[string]task.Task
	cachedValue int64
	id          string
	distro      *distro.Distro
}

// MakeuUnit constructs a new unit, caching a reference to the distro
// in the unit. It's valid to pass a nil here.
func MakeUnit(d *distro.Distro) *Unit {
	return &Unit{
		distro: d,
		tasks:  map[string]task.Task{},
	}
}

// NewUnit constructs a new Unit container for a task.
func NewUnit(t task.Task) *Unit {
	u := MakeUnit(nil)
	u.Add(t)
	return u
}

// Export returns an unordered sequence of tasks from unit. All tasks
// are unique.
func (unit *Unit) Export() TaskList {
	out := make(TaskList, 0, len(unit.tasks))

	for _, t := range unit.tasks {
		out = append(out, t)
	}

	return out
}

// Add caches a task in the unit.
func (unit *Unit) Add(t task.Task) { unit.tasks[t.Id] = t }

// SetDistro makes it possible to change/set the cached distro
// reference in the unit; however, it is not possible to set a nil
// distro.
func (unit *Unit) SetDistro(d *distro.Distro) {
	if d == nil || unit == nil {
		return
	}

	unit.distro = d
}

// Keys returns all of the ids of tasks in the unit.
func (unit *Unit) Keys() []string {
	out := []string{}
	for k := range unit.tasks {
		out = append(out, k)
	}
	return out
}

// ID constructs a unique and hashed ID of all the tasks in the unit.
func (unit *Unit) ID() string {
	if unit.id != "" {
		return unit.id
	}

	hash := sha1.New()
	ids := make(sort.StringSlice, 0, len(unit.tasks))
	for id := range unit.tasks {
		ids = append(ids, id)
	}
	sort.Sort(ids)

	for _, id := range ids {
		_, _ = io.WriteString(hash, id)
	}

	unit.id = fmt.Sprintf("%x", hash.Sum(nil))
	return unit.id
}

type unitInfo struct {
	// TaskIDs are the ids for the tasks in the unit.
	TaskIDs []string `json:"task_ids"`
	// Settings are the planner settings for the unit's distro.
	Settings distro.PlannerSettings `json:"settings"`
	// ExpectedRuntime is the sum of the durations the tasks in the unit are expected to take.
	ExpectedRuntime time.Duration `json:"expected_runtime_ns"`
	// TimeInQueue is the sum of the durations the tasks in the unit have been waiting in the queue.
	TimeInQueue time.Duration `json:"time_in_queue_ns"`
	// TotalPriority is the sum of the priority values of all the tasks in the unit.
	TotalPriority int64 `json:"total_priority"`
	// NumDeps is the total number of tasks depending on tasks in the unit.
	NumDeps int64 `json:"num_deps"`
	// ContainsInCommitQueue indicates if the unit contains any tasks that are part of a commit queue version.
	ContainsInCommitQueue bool `json:"contains_in_commit_queue"`
	// ContainsInPatch indicates if the unit contains any tasks that are part of a patch.
	ContainsInPatch bool `json:"contains_in_patch"`
	// ContainsNonGroupTasks indicates if the unit contains any tasks that are not part of a task group.
	ContainsNonGroupTasks bool `json:"contains_non_group_tasks"`
	// ContainsGenerateTask indicates if the unit contains generator task.
	ContainsGenerateTask bool `json:"contains_generate_task"`
	// ContainsStepbackTask indicates if the unit contains task activated by stepback.
	ContainsStepbackTask bool `json:"contains_stepback_task"`
}

func (u *unitInfo) value() int64 {
	var value int64

	length := int64(len(u.TaskIDs))
	priority := 1 + (u.TotalPriority / length)

	if !u.ContainsNonGroupTasks {
		// if all tasks in the unit are in a task group then
		// we should give it a little bump, so that task
		// groups tasks are sorted together even when they
		// would also be scheduled in a version.
		priority += length
	}
	if u.ContainsGenerateTask {
		// give generators a boost so people don't have to wait twice.
		priority = priority * u.Settings.GetGenerateTaskFactor()
	}

	if u.ContainsInPatch {
		// give patches a bump, over non-patches.
		value += priority * u.Settings.GetPatchFactor()
		// patches that have spent more time in the queue
		// should get worked on first (because people are
		// waiting on the results), and because FIFO feels
		// fair in this context.
		value += priority * u.Settings.GetPatchTimeInQueueFactor() * int64(math.Floor(u.TimeInQueue.Minutes()/float64(length)))
	} else if u.ContainsInCommitQueue {
		// give commit queue patches a boost over everything else
		priority += 200
		value += priority * u.Settings.GetCommitQueueFactor()
	} else {
		// for mainline builds that are more recent, give them a bit
		// of a bump, to avoid running older builds first.
		avgLifeTime := u.TimeInQueue / time.Duration(length)

		var mainlinePriority int64
		if avgLifeTime < time.Duration(7*24)*time.Hour {
			mainlinePriority += u.Settings.GetMainlineTimeInQueueFactor() * int64((7*24*time.Hour - avgLifeTime).Hours())
		}
		if u.ContainsStepbackTask {
			mainlinePriority += u.Settings.GetStepbackTaskFactor()
		}

		value += priority * mainlinePriority
	}

	// Start with the number of tasks so that units with more
	// tasks get sorted above one-offs, and then add the priority
	// setting as a base.
	value += length
	value += priority

	// The remaining values are normalized per tasks, to avoid
	// situations where larger units are always prioritized above
	// smaller groups.
	//
	// Additionally, all these values are multiplied by the
	// priority, to avoid situations where the impact of changing
	// priority is obviated by other factors.

	// Increase the value for the number of dependencies, so that
	// tasks (and units) which block other tasks run before tasks
	// that don't block other tasks.
	value += priority * (u.NumDeps / length)

	// Increase the value for tasks with longer runtimes, given
	// that most of our workloads have different runtimes, and we
	// don't want to have longer makespans if longer running tasks
	// have to execute after shorter running tasks.
	value += priority * u.Settings.GetExpectedRuntimeFactor() * int64(math.Floor(u.ExpectedRuntime.Minutes()/float64(length)))

	return value
}

func (unit *Unit) info() unitInfo {
	info := unitInfo{
		Settings: unit.distro.PlannerSettings,
	}

	for _, t := range unit.tasks {
		if evergreen.IsCommitQueueRequester(t.Requester) || evergreen.IsGithubMergeQueueRequester(t.Requester) {
			info.ContainsInCommitQueue = true
		} else if evergreen.IsPatchRequester(t.Requester) {
			info.ContainsInPatch = true
		}

		info.ContainsNonGroupTasks = info.ContainsNonGroupTasks || t.TaskGroup == ""
		info.ContainsGenerateTask = info.ContainsGenerateTask || t.GenerateTask
		info.ContainsStepbackTask = info.ContainsStepbackTask || t.ActivatedBy == evergreen.StepbackTaskActivator

		if !t.ActivatedTime.IsZero() {
			info.TimeInQueue += time.Since(t.ActivatedTime)
		} else if !t.IngestTime.IsZero() {
			info.TimeInQueue += time.Since(t.IngestTime)
		}

		info.TotalPriority += t.Priority
		info.ExpectedRuntime += t.FetchExpectedDuration().Average
		info.NumDeps += int64(t.NumDependents)
		info.TaskIDs = append(info.TaskIDs, t.Id)
	}

	return info
}

// RankValue returns a point value for the tasks in the unit that can
// be used to compare units with each other.
//
// Generally, higher point values are given to larger units and for
// units that have been in the queue for longer, with longer expected
// runtimes. The tasks' priority acts as a multiplying factor.
func (unit *Unit) RankValue() int64 {
	if unit.cachedValue > 0 {
		return unit.cachedValue
	}

	info := unit.info()
	unit.cachedValue = info.value()

	return unit.cachedValue
}

// StringSet provides simple tools for managing sets of strings.
type StringSet map[string]struct{}

// Add places the string in the set.
func (s StringSet) Add(id string) { s[id] = struct{}{} }

// Check returns true if the string is a member of the set.
func (s StringSet) Check(id string) bool { _, ok := s[id]; return ok }

// Visit returns true if the string is already a member of the
// set. Otherwise it adds it to the set and returns false.
func (s StringSet) Visit(id string) bool {
	if s.Check(id) {
		return true
	}

	s.Add(id)
	return false
}

// TaskList implements sort.Interface on top of a slice of tasks. The
// provided sorting, orders members of task groups, and then
// prioritizes tasks by the number of dependencies, priority, and
// expected duration. This sorting is used for ordering tasks within a
// unit.
type TaskList []task.Task

func (tl TaskList) Len() int      { return len(tl) }
func (tl TaskList) Swap(i, j int) { tl[i], tl[j] = tl[j], tl[i] }
func (tl TaskList) Less(i, j int) bool {
	t1 := tl[i]
	t2 := tl[j]

	// TODO note about impact of this with versions.
	if t1.TaskGroupOrder != t2.TaskGroupOrder {
		return t1.TaskGroupOrder < t2.TaskGroupOrder
	}

	if t1.NumDependents != t2.NumDependents {
		return t1.NumDependents > t2.NumDependents
	}

	if t1.Priority != t2.Priority {
		return t1.Priority > t2.Priority
	}

	return t1.FetchExpectedDuration().Average > t2.FetchExpectedDuration().Average
}

// TaskPlan provides a sortable interface on top of a slice of
// schedulable units, with ordering of units provided by the
// implementation of RankValue.
type TaskPlan []*Unit

func (tpl TaskPlan) Len() int           { return len(tpl) }
func (tpl TaskPlan) Less(i, j int) bool { return tpl[i].RankValue() > tpl[j].RankValue() }
func (tpl TaskPlan) Swap(i, j int)      { tpl[i], tpl[j] = tpl[j], tpl[i] }

func (tpl TaskPlan) Keys() []string {
	out := []string{}
	for _, unit := range tpl {
		out = append(out, unit.Keys()...)
	}
	return out
}

// PrepareTasksForPlanning takes a list of tasks for a distro and
// returns a TaskPlan, grouping tasks into the appropriate units.
func PrepareTasksForPlanning(distro *distro.Distro, tasks []task.Task) TaskPlan {
	cache := UnitCache{}

	for _, t := range tasks {
		var unit *Unit
		if t.TaskGroup != "" {
			unit = cache.Create(t.GetTaskGroupString(), t)
			cache.AddNew(t.Id, unit)
			cache.AddWhen(distro.PlannerSettings.ShouldGroupVersions(), t.Version, t)
		} else if distro.PlannerSettings.ShouldGroupVersions() {
			unit = cache.Create(t.Version, t)
			cache.AddNew(t.Id, unit)
		} else {
			unit = cache.Create(t.Id, t)
		}
		unit.SetDistro(distro)
	}

	for _, t := range tasks {
		// if it has dependencies:
		if len(t.DependsOn) > 0 {
			for _, dep := range t.DependsOn {
				cache.AddWhen(cache.Exists(dep.TaskId), dep.TaskId, t)
			}
		}
	}

	return cache.Export()
}

// Export sorts the TaskPlan returning a unique list of tasks.
func (tpl TaskPlan) Export() []task.Task {
	sort.Sort(tpl)

	output := []task.Task{}
	seen := StringSet{}
	for _, unit := range tpl {
		tasks := unit.Export()
		sort.Sort(tasks)
		for _, t := range tasks {
			if seen.Visit(t.Id) {
				continue
			}

			output = append(output, t)
		}
	}

	return output
}
