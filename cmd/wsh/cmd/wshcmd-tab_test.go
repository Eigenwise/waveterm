// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"github.com/wavetermdev/waveterm/pkg/wshutil"
)

type tabTestServer struct {
	workspaceId string
	tabName     string
	createCalls int
	createErr   error
	blockData   *wshrpc.CommandCreateBlockData
}

func (*tabTestServer) WshServerImpl() {}

func (s *tabTestServer) CreateTabCommand(ctx context.Context, workspaceId string, tabName string) (string, error) {
	s.workspaceId, s.tabName = workspaceId, tabName
	s.createCalls++
	return "new-tab-id", s.createErr
}

func (s *tabTestServer) CreateBlockCommand(ctx context.Context, data wshrpc.CommandCreateBlockData) (waveobj.ORef, error) {
	s.blockData = &data
	return waveobj.MakeORef(waveobj.OType_Block, "b29d96a6-2c12-4f47-9a50-9bfd70dc095b"), nil
}

func (*tabTestServer) WshActivityCommand(ctx context.Context, data map[string]int) error {
	return nil
}

func setupTabTestRpc(t *testing.T, impl *tabTestServer) {
	t.Helper()
	client := wshutil.MakeWshRpc(wshrpc.RpcContext{}, nil, "tab-test-client")
	server := wshutil.MakeWshRpc(wshrpc.RpcContext{}, impl, "tab-test-server")
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, pair := range [][2]*wshutil.WshRpc{{client, server}, {server, client}} {
		wg.Add(1)
		go func(src, dst *wshutil.WshRpc) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				case msg := <-src.OutputCh:
					if !dst.SendRpcMessage(msg, 0, "tab-test") {
						t.Error("test RPC input channel full")
					}
				}
			}
		}(pair[0], pair[1])
	}
	oldClient := RpcClient
	RpcClient = client
	t.Cleanup(func() {
		RpcClient = oldClient
		close(done)
		wg.Wait()
		close(client.InputCh)
		close(server.InputCh)
	})
}

func TestTabCreate(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		env       string
		workspace string
		tabName   string
		wantErr   string
		rpcErr    error
		wantCalls int
	}{
		{name: "current workspace", env: "current-ws", workspace: "current-ws", wantCalls: 1},
		{name: "explicit workspace and name", args: []string{"--workspace", "target-ws", "Build logs"}, env: "current-ws", workspace: "target-ws", tabName: "Build logs", wantCalls: 1},
		{name: "explicit workspace without environment", args: []string{"--workspace=target-ws"}, workspace: "target-ws", wantCalls: 1},
		{name: "missing workspace", wantErr: "no workspace id specified"},
		{name: "too many names", args: []string{"one", "two"}, env: "current-ws", wantErr: "accepts at most 1 arg"},
		{name: "missing flag value", args: []string{"--workspace"}, wantErr: "flag needs an argument"},
		{name: "server rejects malformed workspace", args: []string{"--workspace=bad-id"}, workspace: "bad-id", rpcErr: fmt.Errorf("no window found for workspace"), wantCalls: 1, wantErr: "creating tab: no window found for workspace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WAVETERM_WORKSPACEID", tt.env)
			server := &tabTestServer{createErr: tt.rpcErr}
			setupTabTestRpc(t, server)
			var out bytes.Buffer
			oldOut, oldWorkspace, oldPreRun := WrappedStdout, tabWorkspaceId, tabCreateCmd.PreRunE
			WrappedStdout, tabWorkspaceId = &out, ""
			tabCreateCmd.PreRunE = nil
			t.Cleanup(func() {
				WrappedStdout, tabWorkspaceId, tabCreateCmd.PreRunE = oldOut, oldWorkspace, oldPreRun
				tabCreateCmd.Flags().Lookup("workspace").Changed = false
				rootCmd.SetArgs(nil)
			})
			rootCmd.SetArgs(append([]string{"tab", "create"}, tt.args...))
			err := rootCmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if out.Len() != 0 {
					t.Fatalf("failed creation printed success: %q", out.String())
				}
			} else if err != nil || out.String() != "new-tab-id\n" {
				t.Fatalf("output = %q, error = %v", out.String(), err)
			}
			if server.createCalls != tt.wantCalls || server.workspaceId != tt.workspace || server.tabName != tt.tabName {
				t.Fatalf("RPC got calls=%d workspace=%q name=%q", server.createCalls, server.workspaceId, server.tabName)
			}
		})
	}
}

func TestTabCreateHelpWithoutWave(t *testing.T) {
	t.Setenv(wshutil.WaveJwtTokenVarName, "")
	var out bytes.Buffer
	oldOut := rootCmd.OutOrStdout()
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"tab", "create", "--help"})
	t.Cleanup(func() {
		rootCmd.SetOut(oldOut)
		rootCmd.SetArgs(nil)
		tabCreateCmd.Flags().Set("help", "false")
		tabCreateCmd.Flags().Lookup("help").Changed = false
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"create [name]", "--workspace", "WAVETERM_WORKSPACEID", "Does not create a window or request OS window focus"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("help missing %q: %s", text, out.String())
		}
	}
}

func TestTabCreateRequiresWaveAuthentication(t *testing.T) {
	t.Setenv(wshutil.WaveJwtTokenVarName, "")
	if err := tabCreateCmd.PreRunE(tabCreateCmd, nil); err == nil || !strings.Contains(err.Error(), "WAVETERM_JWT not found") {
		t.Fatalf("expected missing authentication error, got %v", err)
	}
}

func TestTermStillCreatesBlockInCurrentTab(t *testing.T) {
	t.Setenv("WAVETERM_TABID", "existing-tab")
	server := &tabTestServer{}
	setupTabTestRpc(t, server)
	oldOut, oldContext := WrappedStdout, RpcContext
	WrappedStdout, RpcContext = &bytes.Buffer{}, wshrpc.RpcContext{Conn: "test-connection"}
	t.Cleanup(func() { WrappedStdout, RpcContext = oldOut, oldContext })
	if err := termRun(termCmd, nil); err != nil {
		t.Fatal(err)
	}
	if server.createCalls != 0 || server.blockData == nil || server.blockData.TabId != "existing-tab" {
		t.Fatalf("term must create a block in the existing tab, got %+v", server)
	}
	meta := server.blockData.BlockDef.Meta
	if meta.GetString(waveobj.MetaKey_View, "") != "term" || meta.GetString(waveobj.MetaKey_Controller, "") != "shell" || meta.GetString(waveobj.MetaKey_Connection, "") != "test-connection" {
		t.Fatalf("terminal metadata changed: %+v", meta)
	}
}

func TestReorderTabIds(t *testing.T) {
	tests := []struct {
		name      string
		tabIds    []string
		oldIdx    int
		targetIdx int
		tabId     string
		expected  []string
	}{
		{
			name:      "move first to last",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    0,
			targetIdx: 4,
			tabId:     "a",
			expected:  []string{"b", "c", "d", "e", "a"},
		},
		{
			name:      "move last to first",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    4,
			targetIdx: 0,
			tabId:     "e",
			expected:  []string{"e", "a", "b", "c", "d"},
		},
		{
			name:      "move second to fourth (right)",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    1,
			targetIdx: 3,
			tabId:     "b",
			expected:  []string{"a", "c", "d", "b", "e"},
		},
		{
			name:      "move fourth to second (left)",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    3,
			targetIdx: 1,
			tabId:     "d",
			expected:  []string{"a", "d", "b", "c", "e"},
		},
		{
			name:      "move second to third (right by one)",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    1,
			targetIdx: 2,
			tabId:     "b",
			expected:  []string{"a", "c", "b", "d", "e"},
		},
		{
			name:      "move third to second (left by one)",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    2,
			targetIdx: 1,
			tabId:     "c",
			expected:  []string{"a", "c", "b", "d", "e"},
		},
		{
			name:      "two elements swap",
			tabIds:    []string{"x", "y"},
			oldIdx:    0,
			targetIdx: 1,
			tabId:     "x",
			expected:  []string{"y", "x"},
		},
		{
			name:      "move middle to end",
			tabIds:    []string{"a", "b", "c", "d", "e"},
			oldIdx:    2,
			targetIdx: 4,
			tabId:     "c",
			expected:  []string{"a", "b", "d", "e", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make a copy to avoid mutating the test input
			input := make([]string, len(tt.tabIds))
			copy(input, tt.tabIds)
			result := reorderTabIds(input, tt.oldIdx, tt.targetIdx, tt.tabId)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("reorderTabIds(%v, %d, %d, %q) = %v, want %v",
					tt.tabIds, tt.oldIdx, tt.targetIdx, tt.tabId, result, tt.expected)
			}
			if len(result) != len(tt.tabIds) {
				t.Errorf("length changed: got %d, want %d", len(result), len(tt.tabIds))
			}
		})
	}
}
