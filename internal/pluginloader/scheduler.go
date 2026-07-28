package pluginloader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"cipherlake/internal/logger"
	syssched "cipherlake/internal/scheduler"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// pluginTaskSchedule is a runtime task schedule registered for a plugin.
type pluginTaskSchedule struct {
	handle     uint64
	pluginName string
	taskType   syssched.TaskType
	schedule   string
	payload    []byte
	handler    string
}

// taskRegistry holds runtime task schedules for all plugins.
type taskRegistry struct {
	mu         sync.RWMutex
	schedules  map[uint64]*pluginTaskSchedule
	byPlugin   map[string]map[uint64]*pluginTaskSchedule
	byTaskType map[syssched.TaskType]*pluginTaskSchedule
	nextHandle uint64
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{
		schedules:  make(map[uint64]*pluginTaskSchedule),
		byPlugin:   make(map[string]map[uint64]*pluginTaskSchedule),
		byTaskType: make(map[syssched.TaskType]*pluginTaskSchedule),
	}
}

func (r *taskRegistry) add(pluginName, schedule string, payload []byte, handler string) (uint64, syssched.TaskType) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextHandle++
	if r.nextHandle == 0 {
		r.nextHandle++
	}
	handle := r.nextHandle
	taskType := syssched.TaskType(fmt.Sprintf("plugin:%s:%d", pluginName, handle))

	s := &pluginTaskSchedule{
		handle:     handle,
		pluginName: pluginName,
		taskType:   taskType,
		schedule:   schedule,
		payload:    payload,
		handler:    handler,
	}
	r.schedules[handle] = s
	if r.byPlugin[pluginName] == nil {
		r.byPlugin[pluginName] = make(map[uint64]*pluginTaskSchedule)
	}
	r.byPlugin[pluginName][handle] = s
	r.byTaskType[taskType] = s
	return handle, taskType
}

func (r *taskRegistry) remove(handle uint64) (syssched.TaskType, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.schedules[handle]
	if !ok {
		return "", false
	}
	delete(r.schedules, handle)
	delete(r.byTaskType, s.taskType)
	if r.byPlugin[s.pluginName] != nil {
		delete(r.byPlugin[s.pluginName], handle)
		if len(r.byPlugin[s.pluginName]) == 0 {
			delete(r.byPlugin, s.pluginName)
		}
	}
	return s.taskType, true
}

func (r *taskRegistry) clear(pluginName string) []syssched.TaskType {
	r.mu.Lock()
	defer r.mu.Unlock()

	var types []syssched.TaskType
	for handle, s := range r.byPlugin[pluginName] {
		types = append(types, s.taskType)
		delete(r.schedules, handle)
		delete(r.byTaskType, s.taskType)
	}
	delete(r.byPlugin, pluginName)
	return types
}

func (r *taskRegistry) lookup(handle uint64) (*pluginTaskSchedule, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.schedules[handle]
	return s, ok
}

// taskInvocation is the per-on_task call context.
type taskInvocation struct {
	pluginName string
	taskType   syssched.TaskType
	payload    []byte
	startedAt  time.Time
}

// SetScheduler wires the loader to the system scheduler. If plugins are
// already installed, their manifest-declared schedules are registered
// immediately. Pass nil to detach.
func (l *Loader) SetScheduler(sched *syssched.Scheduler) {
	l.mu.Lock()
	l.scheduler = sched
	l.mu.Unlock()

	if sched == nil {
		return
	}

	// Register schedules for plugins that were installed before the
	// scheduler was wired.
	l.mu.RLock()
	holders := make([]*pluginHolder, 0, len(l.plugins))
	for _, h := range l.plugins {
		holders = append(holders, h)
	}
	l.mu.RUnlock()

	for _, h := range holders {
		l.registerTaskSchedulesFromManifest(h.manifest)
	}
}

// SchedulePluginTask registers a dynamic cron task for a plugin. The loader
// invokes the plugin's on_task entry point (or handler) on each trigger.
func (l *Loader) SchedulePluginTask(pluginName, schedule string, payload []byte, handler string) (uint64, error) {
	if schedule == "" {
		return 0, errors.New("schedule is required")
	}
	if handler == "" {
		handler = "on_task"
	}
	if _, err := parseCron(schedule); err != nil {
		return 0, err
	}

	l.mu.RLock()
	sched := l.scheduler
	l.mu.RUnlock()
	if sched == nil {
		return 0, errors.New("scheduler not configured")
	}

	// Ensure a unique task type for this schedule.
	handle, taskType := l.tasks.add(pluginName, schedule, payload, handler)

	err := func() error {
		_, err := sched.RegisterTask(&syssched.TaskConfig{
			Type:        taskType,
			Schedule:    schedule,
			Concurrency: 1,
			Handler: func(ctx context.Context) error {
				return l.invokePluginTask(ctx, handle)
			},
		})
		return err
	}()
	if err != nil {
		l.tasks.remove(handle)
		return 0, fmt.Errorf("failed to register task: %w", err)
	}

	zap.L().Debug("plugin scheduled task",
		zap.String("plugin", pluginName),
		zap.String("schedule", schedule),
		zap.Uint64("handle", handle))
	return handle, nil
}

// UnschedulePluginTask removes a dynamic task schedule for a plugin.
func (l *Loader) UnschedulePluginTask(pluginName string, handle uint64) error {
	sub, ok := l.tasks.lookup(handle)
	if !ok || sub.pluginName != pluginName {
		return fmt.Errorf("task %d not found for plugin %q", handle, pluginName)
	}

	l.mu.RLock()
	sched := l.scheduler
	l.mu.RUnlock()
	if sched == nil {
		return errors.New("scheduler not configured")
	}

	if err := sched.UnregisterTask(sub.taskType); err != nil {
		return err
	}
	l.tasks.remove(handle)
	return nil
}

// invokePluginTask borrows an instance and calls the plugin's task handler.
func (l *Loader) invokePluginTask(ctx context.Context, handle uint64) error {
	sub, ok := l.tasks.lookup(handle)
	if !ok {
		return fmt.Errorf("task schedule %d not found", handle)
	}

	l.mu.RLock()
	holder, ok := l.plugins[sub.pluginName]
	l.mu.RUnlock()
	if !ok || holder == nil || holder.pool == nil {
		return fmt.Errorf("plugin %q not installed", sub.pluginName)
	}

	inv := &taskInvocation{
		pluginName: sub.pluginName,
		taskType:   sub.taskType,
		payload:    sub.payload,
		startedAt:  time.Now(),
	}
	taskHandle := l.allocateTaskContext(inv)
	defer l.releaseTaskContext(taskHandle)

	inst, err := holder.pool.Borrow(ctx)
	if err != nil {
		return fmt.Errorf("failed to borrow instance: %w", err)
	}
	defer holder.pool.Return(inst)

	handler := sub.handler
	if handler == "" {
		handler = "on_task"
	}

	logger.AuditLogger("plugin.task.dispatch", sub.pluginName, "", "allowed", map[string]interface{}{
		"task_type":  string(sub.taskType),
		"schedule":   sub.schedule,
		"handler":    handler,
		"payload_bytes": len(sub.payload),
	})

	if _, err := inst.CallEntry(ctx, handler, taskHandle); err != nil {
		return fmt.Errorf("plugin %s handler %s failed: %w", sub.pluginName, handler, err)
	}
	return nil
}

// registerTaskSchedulesFromManifest registers schedules declared in the
// plugin manifest.
func (l *Loader) registerTaskSchedulesFromManifest(manifest *Manifest) {
	l.mu.RLock()
	sched := l.scheduler
	l.mu.RUnlock()
	if sched == nil {
		return
	}

	for _, ts := range manifest.TaskSchedules {
		handler := ts.Handler
		if handler == "" {
			handler = "on_task"
		}
		payload := []byte(ts.Payload)
		if _, err := l.SchedulePluginTask(manifest.Name, ts.Schedule, payload, handler); err != nil {
			zap.L().Warn("failed to register manifest task schedule",
				zap.String("plugin", manifest.Name),
				zap.String("schedule", ts.Schedule),
				zap.Error(err))
		}
	}
}

// unregisterPluginTaskSchedules removes all task schedules for a plugin.
// Caller must hold l.mu (write lock); unloadPlugin is the only caller.
func (l *Loader) unregisterPluginTaskSchedules(pluginName string) {
	sched := l.scheduler
	for _, taskType := range l.tasks.clear(pluginName) {
		if sched != nil {
			_ = sched.UnregisterTask(taskType)
		}
	}
}

// stopAllPluginTasks unregisters every plugin-owned task schedule. Called
// during shutdown before the WASM runtime is closed.
func (l *Loader) stopAllPluginTasks() {
	l.mu.RLock()
	sched := l.scheduler
	l.mu.RUnlock()

	l.tasks.mu.RLock()
	schedules := make([]*pluginTaskSchedule, 0, len(l.tasks.schedules))
	for _, s := range l.tasks.schedules {
		schedules = append(schedules, s)
	}
	l.tasks.mu.RUnlock()

	for _, s := range schedules {
		if sched != nil {
			_ = sched.UnregisterTask(s.taskType)
		}
		l.tasks.remove(s.handle)
	}
}

// --- task handle management ---

func (l *Loader) allocateTaskContext(inv *taskInvocation) uint64 {
	l.taskMu.Lock()
	defer l.taskMu.Unlock()
	l.nextTaskHandle++
	if l.nextTaskHandle == 0 {
		l.nextTaskHandle++
	}
	h := l.nextTaskHandle
	l.taskContexts[h] = inv
	return h
}

// GetTaskContext returns the task invocation for the given handle.
func (l *Loader) GetTaskContext(handle uint64) *taskInvocation {
	l.taskMu.Lock()
	defer l.taskMu.Unlock()
	return l.taskContexts[handle]
}

func (l *Loader) releaseTaskContext(handle uint64) {
	l.taskMu.Lock()
	defer l.taskMu.Unlock()
	delete(l.taskContexts, handle)
}

// parseCron validates a cron expression using the same parser as the
// system scheduler, accepting both 5-field and 6-field (with seconds) cron.
func parseCron(schedule string) (cron.Schedule, error) {
	return cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(schedule)
}
