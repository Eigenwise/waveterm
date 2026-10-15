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
	"github.com/wavetermdev/waveterm/pkg/wstore"
)

type tabUpdateRecorder struct {
	updates map[waveobj.ORef]bool
}

func (r *tabUpdateRecorder) SendEvent(routeId string, event wps.WaveEvent) {
	update := event.Data.(waveobj.WaveObjUpdate)
	r.updates[waveobj.MakeORef(update.OType, update.OID)] = true
}

func TestCreateTabCommand(t *testing.T) {
	root := os.Getenv("WAVETERM_TEST_TAB_CREATE_DIR")
	if root == "" {
		// wstore has no close operation; process exit releases SQLite handles before Windows removes the fixture.
		child := exec.Command(os.Args[0], "-test.run=^TestCreateTabCommand$", "-test.v")
		child.Env = append(os.Environ(), "WAVETERM_TEST_TAB_CREATE_DIR="+t.TempDir())
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
	closed := &waveobj.Workspace{OID: uuid.NewString(), TabIds: []string{}}
	window := &waveobj.Window{OID: uuid.NewString(), WorkspaceId: target.OID}
	otherWindow := &waveobj.Window{OID: uuid.NewString(), WorkspaceId: other.OID}
	for _, obj := range []waveobj.WaveObj{target, other, closed, window, otherWindow} {
		if err := wstore.DBInsert(ctx, obj); err != nil {
			t.Fatal(err)
		}
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
		t.Run(tt.wantName, func(t *testing.T) {
			before, err := wstore.DBMustGet[*waveobj.Workspace](ctx, target.OID)
			if err != nil {
				t.Fatal(err)
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr := os.Stderr
			os.Stderr = w
			defer func() { os.Stderr = stderr }()
			output := make(chan []byte, 1)
			go func() {
				data, _ := io.ReadAll(r)
				output <- data
			}()
			updateCtx := waveobj.ContextWithUpdates(ctx)
			tabId, createErr := server.CreateTabCommand(updateCtx, target.OID, tt.name)
			os.Stderr = stderr
			w.Close()
			eventOutput := <-output
			r.Close()
			if createErr != nil {
				t.Fatal(createErr)
			}
			if err := uuid.Validate(tabId); err != nil {
				t.Fatalf("invalid returned tab id %q: %v", tabId, err)
			}
			ws, err := wstore.DBMustGet[*waveobj.Workspace](ctx, target.OID)
			if err != nil {
				t.Fatal(err)
			}
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
				oref := *waveobj.ORefFromWaveObj(obj)
				if waveobj.ContextGetUpdate(updateCtx, oref) == nil || !recorder.updates[oref] {
					t.Fatalf("missing object update/publication for %v", oref)
				}
			}
			var activeUpdate waveobj.ActiveTabUpdate
			foundEvent := false
			for _, line := range strings.Split(string(eventOutput), "\n") {
				if !strings.HasPrefix(line, "WAVESRV-EVENT:") {
					continue
				}
				var event eventbus.WSEventType
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "WAVESRV-EVENT:")), &event); err != nil {
					t.Fatal(err)
				}
				if event.EventType == eventbus.WSEvent_ElectronUpdateActiveTab {
					data, _ := json.Marshal(event.Data)
					if err := json.Unmarshal(data, &activeUpdate); err != nil {
						t.Fatal(err)
					}
					foundEvent = true
				}
			}
			if !foundEvent || activeUpdate.WorkspaceId != target.OID || activeUpdate.NewActiveTabId != tabId {
				t.Fatalf("missing/misdirected Electron activation event: %s", eventOutput)
			}
		})
	}

	for _, workspaceId := range []string{"", "malformed", uuid.NewString(), closed.OID} {
		t.Run("reject-"+workspaceId, func(t *testing.T) {
			before, err := wstore.DBGetCount[*waveobj.Tab](ctx)
			if err != nil {
				t.Fatal(err)
			}
			if id, err := server.CreateTabCommand(ctx, workspaceId, ""); err == nil || id != "" {
				t.Fatalf("expected error and no id, got %q, %v", id, err)
			}
			after, err := wstore.DBGetCount[*waveobj.Tab](ctx)
			if err != nil || after != before {
				t.Fatalf("failed creation changed tab count: before=%d after=%d err=%v", before, after, err)
			}
		})
	}
	unchanged, err := wstore.DBMustGet[*waveobj.Workspace](ctx, other.OID)
	if err != nil || !reflect.DeepEqual(unchanged.TabIds, other.TabIds) || unchanged.ActiveTabId != other.ActiveTabId {
		t.Fatalf("unrelated workspace changed: %+v, %v", unchanged, err)
	}
	windows, err := wstore.DBGetAllObjsByType[*waveobj.Window](ctx, waveobj.OType_Window)
	if err != nil || len(windows) != 2 {
		t.Fatalf("creation must not create a new window: %+v, %v", windows, err)
	}
}
