// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package wshserver

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wavetermdev/waveterm/pkg/eventbus"
	"github.com/wavetermdev/waveterm/pkg/wavebase"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wps"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"github.com/wavetermdev/waveterm/pkg/wstore"
)

type tabUpdateRecorder struct {
	updates map[waveobj.ORef]bool
}

func (r *tabUpdateRecorder) SendEvent(routeId string, event wps.WaveEvent) {
	update := event.Data.(waveobj.WaveObjUpdate)
	r.updates[waveobj.MakeORef(update.OType, update.OID)] = true
}

// captureActiveTabEvents runs fn and returns the active-tab updates it sent to
// Electron (wavesrv writes them as WAVESRV-EVENT lines on stderr).
func captureActiveTabEvents(t *testing.T, fn func()) []waveobj.ActiveTabUpdate {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	output := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		output <- data
	}()
	fn()
	os.Stderr = stderr
	w.Close()
	data := <-output
	r.Close()
	var rtn []waveobj.ActiveTabUpdate
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "WAVESRV-EVENT:") {
			continue
		}
		var event eventbus.WSEventType
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "WAVESRV-EVENT:")), &event); err != nil {
			t.Fatal(err)
		}
		if event.EventType != eventbus.WSEvent_ElectronUpdateActiveTab {
			continue
		}
		raw, _ := json.Marshal(event.Data)
		var update waveobj.ActiveTabUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			t.Fatal(err)
		}
		rtn = append(rtn, update)
	}
	return rtn
}

func TestTabCommands(t *testing.T) {
	root := os.Getenv("WAVETERM_TEST_TAB_DIR")
	if root == "" {
		// wstore has no close operation; process exit releases the SQLite handles before Windows removes the fixture.
		child := exec.Command(os.Args[0], "-test.run=^TestTabCommands$", "-test.v")
		child.Env = append(os.Environ(), "WAVETERM_TEST_TAB_DIR="+t.TempDir())
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("isolated tab lifecycle test: %v\n%s", err, output)
		}
		return
	}
	dataDir := filepath.Join(root, "data")
	wavebase.DataHome_VarCache = dataDir
	wavebase.ConfigHome_VarCache = filepath.Join(root, "config")
	if err := os.MkdirAll(wavebase.ConfigHome_VarCache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, wavebase.WaveDBDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := wstore.InitWStore(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := &waveobj.Workspace{OID: uuid.NewString(), TabIds: []string{}}
	other := &waveobj.Workspace{OID: uuid.NewString(), TabIds: []string{"untouched-tab"}, ActiveTabId: "untouched-tab"}
	window := &waveobj.Window{OID: uuid.NewString(), WorkspaceId: target.OID}
	otherWindow := &waveobj.Window{OID: uuid.NewString(), WorkspaceId: other.OID}
	for _, obj := range []waveobj.WaveObj{target, other, window, otherWindow} {
		if err := wstore.DBInsert(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	getWorkspace := func(t *testing.T) *waveobj.Workspace {
		t.Helper()
		ws, err := wstore.DBMustGet[*waveobj.Workspace](ctx, target.OID)
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}

	server := &WshServer{}
	recorder := &tabUpdateRecorder{updates: make(map[waveobj.ORef]bool)}
	oldClient := wps.Broker.GetClient()
	wps.Broker.SetClient(recorder)
	wps.Broker.Subscribe("tab-test", wps.SubscriptionRequest{Event: wps.Event_WaveObjUpdate, AllScopes: true})
	defer func() {
		wps.Broker.Unsubscribe("tab-test", wps.Event_WaveObjUpdate)
		wps.Broker.SetClient(oldClient)
	}()

	for _, tt := range []struct{ name, wantName string }{{"", "T1"}, {"Build logs", "Build logs"}, {"", "T2"}} {
		t.Run("create-"+tt.wantName, func(t *testing.T) {
			before := getWorkspace(t)
			var tabId string
			var createErr error
			events := captureActiveTabEvents(t, func() {
				tabId, createErr = server.CreateTabCommand(ctx, wshrpc.CommandCreateTabData{WorkspaceId: target.OID, TabName: tt.name, ActivateTab: true})
			})
			if createErr != nil {
				t.Fatal(createErr)
			}
			if err := uuid.Validate(tabId); err != nil {
				t.Fatalf("invalid returned tab id %q: %v", tabId, err)
			}
			ws := getWorkspace(t)
			wantIds := append(before.TabIds, tabId)
			if !reflect.DeepEqual(ws.TabIds, wantIds) || ws.ActiveTabId != tabId {
				t.Fatalf("workspace membership/activation = %+v, want tabs %v active %s", ws, wantIds, tabId)
			}
			tab, err := wstore.DBMustGet[*waveobj.Tab](ctx, tabId)
			if err != nil {
				t.Fatal(err)
			}
			if tab.Name != tt.wantName || len(tab.BlockIds) != 1 {
				t.Fatalf("expected named tab with one default terminal: %+v", tab)
			}
			block, err := wstore.DBMustGet[*waveobj.Block](ctx, tab.BlockIds[0])
			if err != nil {
				t.Fatal(err)
			}
			if block.ParentORef != waveobj.MakeORef(waveobj.OType_Tab, tabId).String() || block.Meta.GetString(waveobj.MetaKey_View, "") != "term" || block.Meta.GetString(waveobj.MetaKey_Controller, "") != "shell" {
				t.Fatalf("default terminal not attached to new tab: %+v", block)
			}
			layout, err := wstore.DBMustGet[*waveobj.LayoutState](ctx, tab.LayoutState)
			if err != nil {
				t.Fatal(err)
			}
			if layout.PendingBackendActions == nil || len(*layout.PendingBackendActions) != 2 || (*layout.PendingBackendActions)[0].ActionType != "clear" || (*layout.PendingBackendActions)[1].BlockId != block.OID || !(*layout.PendingBackendActions)[1].Focused {
				t.Fatalf("normal new-tab layout not queued: %+v", layout)
			}
			for _, obj := range []waveobj.WaveObj{ws, tab, block, layout} {
				if oref := *waveobj.ORefFromWaveObj(obj); !recorder.updates[oref] {
					t.Fatalf("missing object update publication for %v", oref)
				}
			}
			if len(events) != 1 || events[0].WorkspaceId != target.OID || events[0].NewActiveTabId != tabId {
				t.Fatalf("missing/misdirected Electron activation event: %+v", events)
			}
		})
	}

	t.Run("create-background-with-meta", func(t *testing.T) {
		before := getWorkspace(t)
		var tabId string
		var createErr error
		events := captureActiveTabEvents(t, func() {
			tabId, createErr = server.CreateTabCommand(ctx, wshrpc.CommandCreateTabData{
				WorkspaceId: target.OID, TabName: "logs", Meta: waveobj.MetaMapType{"bg:color": "#223344"},
			})
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		ws := getWorkspace(t)
		if ws.ActiveTabId != before.ActiveTabId || ws.TabIds[len(ws.TabIds)-1] != tabId {
			t.Fatalf("background creation must add the tab without activating it: %+v", ws)
		}
		if len(events) != 0 {
			t.Fatalf("background creation must not send an activation event: %+v", events)
		}
		tab, err := wstore.DBMustGet[*waveobj.Tab](ctx, tabId)
		if err != nil {
			t.Fatal(err)
		}
		if tab.Name != "logs" || tab.Meta.GetString("bg:color", "") != "#223344" {
			t.Fatalf("meta not applied to new tab: %+v", tab)
		}
	})

	for _, workspaceId := range []string{"", "malformed", uuid.NewString()} {
		t.Run("create-rejects-"+workspaceId, func(t *testing.T) {
			before, err := wstore.DBGetCount[*waveobj.Tab](ctx)
			if err != nil {
				t.Fatal(err)
			}
			if id, err := server.CreateTabCommand(ctx, wshrpc.CommandCreateTabData{WorkspaceId: workspaceId, ActivateTab: true}); err == nil || id != "" {
				t.Fatalf("expected error and no id, got %q, %v", id, err)
			}
			after, err := wstore.DBGetCount[*waveobj.Tab](ctx)
			if err != nil || after != before {
				t.Fatalf("failed creation changed tab count: before=%d after=%d err=%v", before, after, err)
			}
		})
	}

	t.Run("focus", func(t *testing.T) {
		ws := getWorkspace(t)
		first := ws.TabIds[0]
		if ws.ActiveTabId == first {
			t.Fatalf("fixture expects a non-active first tab: %+v", ws)
		}
		var focusErr error
		events := captureActiveTabEvents(t, func() {
			focusErr = server.FocusTabCommand(ctx, first)
		})
		if focusErr != nil {
			t.Fatal(focusErr)
		}
		if ws = getWorkspace(t); ws.ActiveTabId != first {
			t.Fatalf("focus did not activate tab %s: %+v", first, ws)
		}
		if !recorder.updates[waveobj.MakeORef(waveobj.OType_Workspace, target.OID)] {
			t.Fatal("focus did not publish the workspace update")
		}
		if len(events) != 1 || events[0].WorkspaceId != target.OID || events[0].NewActiveTabId != first {
			t.Fatalf("missing/misdirected Electron activation event: %+v", events)
		}
	})

	for _, tabId := range []string{"", "malformed", uuid.NewString()} {
		t.Run("focus-rejects-"+tabId, func(t *testing.T) {
			before := getWorkspace(t)
			var focusErr error
			events := captureActiveTabEvents(t, func() {
				focusErr = server.FocusTabCommand(ctx, tabId)
			})
			if focusErr == nil {
				t.Fatal("expected error")
			}
			if ws := getWorkspace(t); ws.ActiveTabId != before.ActiveTabId || len(events) != 0 {
				t.Fatalf("failed focus changed activation: %+v events=%+v", ws, events)
			}
		})
	}

	unchanged, err := wstore.DBMustGet[*waveobj.Workspace](ctx, other.OID)
	if err != nil || !reflect.DeepEqual(unchanged.TabIds, other.TabIds) || unchanged.ActiveTabId != other.ActiveTabId {
		t.Fatalf("unrelated workspace changed: %+v, %v", unchanged, err)
	}
	windows, err := wstore.DBGetAllObjsByType[*waveobj.Window](ctx, waveobj.OType_Window)
	if err != nil || len(windows) != 2 {
		t.Fatalf("tab commands must not create windows: %+v, %v", windows, err)
	}
}
