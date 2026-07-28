package pluginloader

import (
	"testing"
	"time"

	"cipherlake/internal/scheduler"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testScheduler(t *testing.T, l *Loader) *scheduler.Scheduler {
	t.Helper()
	sched := scheduler.NewScheduler()
	l.SetScheduler(sched)
	require.NoError(t, sched.Start())
	t.Cleanup(func() { sched.Stop() })
	return sched
}

func minimalTaskManifest(name string) *Manifest {
	return &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               name,
		Version:            "0.1.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"task:schedule"},
	}
}

func TestLoader_TaskSchedule_Manifest(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	sched := testScheduler(t, l)

	manifest := minimalTaskManifest("task-schedule-test")
	manifest.TaskSchedules = []ManifestTaskSchedule{
		{Schedule: "*/1 * * * * *", Payload: "ping"},
	}
	require.NoError(t, manifest.Validate())

	ctx := testCtx(t)
	err := l.InstallPlugin(ctx, manifest, minimalPluginBytes, nil, "")
	require.NoError(t, err)

	// Wait up to 3 seconds for at least one trigger.
	var taskType scheduler.TaskType
	require.Eventually(t, func() bool {
		l.tasks.mu.RLock()
		defer l.tasks.mu.RUnlock()
		for _, s := range l.tasks.schedules {
			taskType = s.taskType
			return true
		}
		return false
	}, 2*time.Second, 50*time.Millisecond, "task schedule not registered")

	require.Eventually(t, func() bool {
		hist := sched.GetTaskHistory(taskType, 1)
		return len(hist) > 0 && hist[0].Success
	}, 3*time.Second, 100*time.Millisecond, "task did not run successfully")
}

func TestHostImports_TaskScheduleAndRunNow(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	sched := testScheduler(t, l)

	manifest := minimalTaskManifest("task-host-test")
	ctx := testCtx(t)
	err := l.InstallPlugin(ctx, manifest, minimalPluginBytes, nil, "")
	require.NoError(t, err)

	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("task-host-test", 0, []string{"task:schedule"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	cronExpr := []byte("*/1 * * * * *")
	payload := []byte(`{"count":1}`)
	writeTestMemory(t, mem, base, cronExpr)
	writeTestMemory(t, mem, base+128, payload)

	// task.schedule(token, cron_ptr, cron_len, payload_ptr, payload_len, out_handle, out_status)
	h.taskSchedule(ctx, mod, uint64(tok),
		base, uint32(len(cronExpr)),
		base+128, uint32(len(payload)),
		base+256,
		base+264,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+264), "task.schedule status")
	handle := readTestUint64(t, mem, base+256)
	require.NotZero(t, handle)

	sub, ok := l.tasks.lookup(handle)
	require.True(t, ok)

	// Trigger immediately via RunTaskNow.
	err = sched.RunTaskNow(ctx, sub.taskType)
	require.NoError(t, err)

	hist := sched.GetTaskHistory(sub.taskType, 1)
	require.Len(t, hist, 1)
	require.True(t, hist[0].Success)
}

func TestHostImports_TaskUnschedule(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	sched := testScheduler(t, l)

	manifest := minimalTaskManifest("task-unschedule-test")
	ctx := testCtx(t)
	err := l.InstallPlugin(ctx, manifest, minimalPluginBytes, nil, "")
	require.NoError(t, err)

	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("task-unschedule-test", 0, []string{"task:schedule"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	cronExpr := []byte("*/1 * * * * *")
	writeTestMemory(t, mem, base, cronExpr)

	h.taskSchedule(ctx, mod, uint64(tok),
		base, uint32(len(cronExpr)),
		base+128, 0,
		base+256,
		base+264,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+264))
	handle := readTestUint64(t, mem, base+256)

	// task.unschedule(token, handle, out_status)
	h.taskUnschedule(ctx, mod, uint64(tok), handle, base+300)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+300))

	sub, ok := l.tasks.lookup(handle)
	require.False(t, ok, "schedule should be removed")

	// RunTaskNow should fail because the task type is gone.
	if ok {
		err = sched.RunTaskNow(ctx, sub.taskType)
		require.Error(t, err)
	}
}

func TestHostImports_TaskRead(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	payload := []byte(`{"task":"demo"}`)
	inv := &taskInvocation{pluginName: "demo", payload: payload}
	handle := l.allocateTaskContext(inv)
	defer l.releaseTaskContext(handle)

	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	ctx := testCtx(t)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("demo", 0, []string{"task:schedule"}, neverExpires())
	require.NoError(t, err)

	const base uint32 = 64
	h.taskRead(ctx, mod, uint64(tok), handle,
		base, 256,
		base+256,
		base+260,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+260), "task.read status")
	require.Equal(t, uint32(len(payload)), readTestUint32(t, mem, base+256), "task.read len")
	got, _ := mem.Read(base, uint32(len(payload)))
	require.Equal(t, payload, got)
}

func TestManifest_TaskSchedules_Validation(t *testing.T) {
	valid := minimalTaskManifest("valid")
	valid.TaskSchedules = []ManifestTaskSchedule{{Schedule: "0 0 * * *"}}
	require.NoError(t, valid.Validate())

	missingCap := minimalTaskManifest("missing-cap")
	missingCap.Capabilities = nil
	missingCap.TaskSchedules = []ManifestTaskSchedule{{Schedule: "0 0 * * *"}}
	require.Error(t, missingCap.Validate())

	invalidCron := minimalTaskManifest("invalid-cron")
	invalidCron.TaskSchedules = []ManifestTaskSchedule{{Schedule: "not-a-cron"}}
	require.Error(t, invalidCron.Validate())
}

func TestLoader_TaskSchedules_ClearedOnUninstall(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	testScheduler(t, l)

	manifest := minimalTaskManifest("task-uninstall-test")
	manifest.TaskSchedules = []ManifestTaskSchedule{{Schedule: "0 0 * * *"}}
	ctx := testCtx(t)
	err := l.InstallPlugin(ctx, manifest, minimalPluginBytes, nil, "")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		l.tasks.mu.RLock()
		defer l.tasks.mu.RUnlock()
		return len(l.tasks.byPlugin["task-uninstall-test"]) == 1
	}, 2*time.Second, 50*time.Millisecond, "schedule not registered")

	l.unloadPlugin("task-uninstall-test")

	l.tasks.mu.RLock()
	defer l.tasks.mu.RUnlock()
	assert.Empty(t, l.tasks.byPlugin["task-uninstall-test"])
	assert.Empty(t, l.tasks.schedules)
}

func TestLoader_SetScheduler_WiresExistingPlugins(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := minimalTaskManifest("task-existing-test")
	manifest.TaskSchedules = []ManifestTaskSchedule{{Schedule: "0 0 * * *"}}
	ctx := testCtx(t)
	err := l.InstallPlugin(ctx, manifest, minimalPluginBytes, nil, "")
	require.NoError(t, err)

	// No schedules should be registered until a scheduler is wired.
	l.tasks.mu.RLock()
	empty := len(l.tasks.schedules) == 0
	l.tasks.mu.RUnlock()
	require.True(t, empty, "schedules should not be registered without a scheduler")

	sched := scheduler.NewScheduler()
	defer sched.Stop()
	l.SetScheduler(sched)

	require.Eventually(t, func() bool {
		l.tasks.mu.RLock()
		defer l.tasks.mu.RUnlock()
		return len(l.tasks.schedules) == 1
	}, 2*time.Second, 50*time.Millisecond, "existing plugin schedules should be registered when scheduler is wired")
}
