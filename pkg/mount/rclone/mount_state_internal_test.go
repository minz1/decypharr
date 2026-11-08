package rclone

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestMountTransitionTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, mountSuccessors,
		[]mountState{
			mountWaiting, mountMounting, mountMounted, mountDegraded, mountFailed, mountUnmounting, mountUnmounted,
		},
		[]statetest.Edge[mountState]{
			{From: mountWaiting, To: mountMounting},
			{From: mountWaiting, To: mountUnmounting},
			{From: mountWaiting, To: mountUnmounted},
			{From: mountMounting, To: mountMounting},
			{From: mountMounting, To: mountMounted},
			{From: mountMounting, To: mountFailed},
			{From: mountMounting, To: mountWaiting},
			{From: mountMounting, To: mountUnmounting},
			{From: mountMounted, To: mountDegraded},
			{From: mountMounted, To: mountMounting},
			{From: mountMounted, To: mountWaiting},
			{From: mountMounted, To: mountUnmounting},
			{From: mountDegraded, To: mountDegraded},
			{From: mountDegraded, To: mountMounted},
			{From: mountDegraded, To: mountMounting},
			{From: mountDegraded, To: mountWaiting},
			{From: mountDegraded, To: mountUnmounting},
			{From: mountFailed, To: mountFailed},
			{From: mountFailed, To: mountMounting},
			{From: mountFailed, To: mountWaiting},
			{From: mountFailed, To: mountUnmounting},
			{From: mountUnmounting, To: mountUnmounted},
		})
}

func TestMountStateNames(t *testing.T) {
	t.Parallel()
	want := map[mountState]string{
		mountWaiting: "waiting", mountMounting: "mounting", mountMounted: "mounted", mountDegraded: "degraded",
		mountFailed: "failed", mountUnmounting: "unmounting", mountUnmounted: "unmounted", mountState(99): "mount(99)",
	}
	for s, name := range want {
		if got := s.String(); got != name {
			t.Errorf("state %d = %q, want %q", uint32(s), got, name)
		}
	}
}

func newViewManager() *Manager {
	m := &Manager{mount: config.Mount{MountPath: "/mnt/decypharr"}, webdavURL: "http://x/webdav/"}
	m.view.Store(&mountView{state: mountWaiting})
	return m
}

func TestMoveMountRefusesIllegal(t *testing.T) {
	t.Parallel()
	m := newViewManager()
	before := m.view.Load()
	if m.moveMount(mountMounted, nil) {
		t.Fatal("Waiting → Mounted was allowed")
	}
	if m.view.Load() != before {
		t.Fatal("a refused move changed the view")
	}
	if got := m.mountRefused.Load(); got != 1 {
		t.Fatalf("mountRefused = %d, want 1", got)
	}
}

func TestMoveMountWithPicksFromLoadedView(t *testing.T) {
	t.Parallel()
	m := newViewManager()
	var seen mountState
	ok := m.moveMountWith(func(cur mountView) (mountState, func(*mountView)) {
		seen = cur.state
		return mountUnmounted, nil
	})
	if !ok || seen != mountWaiting || m.view.Load().state != mountUnmounted {
		t.Fatalf("ok=%v seen=%s state=%s", ok, seen, m.view.Load().state)
	}
}

func TestMountInfoNilBeforeAttempt(t *testing.T) {
	t.Parallel()
	if info := newViewManager().mountInfo(); info != nil {
		t.Fatalf("mountInfo = %+v, want nil", info)
	}
}

func TestMountInfoKeepsErrorUntilMounted(t *testing.T) {
	t.Parallel()
	m := newViewManager()
	if !m.moveMount(mountMounting, func(v *mountView) { v.attempted = true; v.err = "x" }) {
		t.Fatal("Waiting → Mounting refused")
	}
	info := m.mountInfo()
	if info == nil || info.Error != "x" || info.Mounted || info.State != "mounting" || info.MountedAt != "" {
		t.Fatalf("mounting info = %+v", info)
	}
	if !m.moveMount(mountMounting, nil) || m.mountInfo().Error != "x" {
		t.Fatal("Mounting → Mounting blanked the error")
	}
	at := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	if !m.moveMount(mountMounted, func(v *mountView) { v.err = ""; v.mountedAt = at }) {
		t.Fatal("Mounting → Mounted refused")
	}
	info = m.mountInfo()
	if info.Error != "" || !info.Mounted || info.State != "mounted" || info.MountedAt != at.Format(time.RFC3339) ||
		info.LocalPath != "/mnt/decypharr" || info.WebDAVURL != "http://x/webdav/" || info.ConfigName != ConfigName {
		t.Fatalf("mounted info = %+v", info)
	}
}
