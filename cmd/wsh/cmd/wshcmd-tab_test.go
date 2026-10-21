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

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"github.com/wavetermdev/waveterm/pkg/wshutil"
)

// tabTestServer records the tab RPCs issued by the CLI.
type tabTestServer struct {
	createData  *wshrpc.CommandCreateTabData
	renameTabId string
	renameName  string
	focusTabId  string
	err         error
}

func (*tabTestServer) WshServerImpl() {}

func (s *tabTestServer) CreateTabCommand(ctx context.Context, data wshrpc.CommandCreateTabData) (string, error) {
	s.createData = &data
	return "new-tab-id", s.err
}

func (s *tabTestServer) UpdateTabNameCommand(ctx context.Context, tabId string, newName string) error {
	s.renameTabId, s.renameName = tabId, newName
	return s.err
}

func (s *tabTestServer) FocusTabCommand(ctx context.Context, tabId string) error {
	s.focusTabId = tabId
	return s.err
}

func (*tabTestServer) WshActivityCommand(ctx context.Context, data map[string]int) error {
	return nil
}

// setupTabTest wires the CLI's RpcClient to an in-memory server and resets the
// tab command state so table-driven cases do not leak flags into each other.
func setupTabTest(t *testing.T, impl *tabTestServer) *bytes.Buffer {
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
	var out bytes.Buffer
	oldClient, oldOut := RpcClient, WrappedStdout
	RpcClient, WrappedStdout = client, &out
	tabWorkspaceId, tabCreateName, tabCreateNoActivate, tabCreateMeta, tabRenameTabId = "", "", false, nil, ""
	commands := []*cobra.Command{tabCreateCmd, tabRenameCmd, tabFocusCmd}
	oldPreRuns := map[string]func(*cobra.Command, []string) error{}
	for _, c := range commands {
		oldPreRuns[c.Name()] = c.PreRunE
		c.PreRunE = nil
		c.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	}
	t.Cleanup(func() {
		for _, c := range commands {
			c.PreRunE = oldPreRuns[c.Name()]
		}
		rootCmd.SetArgs(nil)
		RpcClient, WrappedStdout = oldClient, oldOut
		close(done)
		wg.Wait()
		close(client.InputCh)
		close(server.InputCh)
	})
	return &out
}

func TestTabCreate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		env     string
		want    *wshrpc.CommandCreateTabData
		rpcErr  error
		wantErr string
	}{
		{name: "current workspace", env: "current-ws", want: &wshrpc.CommandCreateTabData{WorkspaceId: "current-ws", ActivateTab: true}},
		{name: "explicit workspace, name and background", args: []string{"-w", "target-ws", "--name", "Build logs", "--no-activate"}, env: "current-ws",
			want: &wshrpc.CommandCreateTabData{WorkspaceId: "target-ws", TabName: "Build logs"}},
		{name: "server resolves caller workspace", want: &wshrpc.CommandCreateTabData{ActivateTab: true}},
		{name: "typed meta", args: []string{"--meta", "bg:color=#223344", "--meta", "bg:opacity=0.5"}, env: "current-ws",
			want: &wshrpc.CommandCreateTabData{WorkspaceId: "current-ws", ActivateTab: true, Meta: waveobj.MetaMapType{"bg:color": "#223344", "bg:opacity": 0.5}}},
		{name: "invalid meta", args: []string{"--meta", "novalue"}, env: "current-ws", wantErr: "invalid meta set"},
		{name: "positional name rejected", args: []string{"Build logs"}, env: "current-ws", wantErr: "unknown command"},
		{name: "server failure", env: "current-ws", rpcErr: fmt.Errorf("workspace not found"), wantErr: "creating tab: workspace not found",
			want: &wshrpc.CommandCreateTabData{WorkspaceId: "current-ws", ActivateTab: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WAVETERM_WORKSPACEID", tt.env)
			server := &tabTestServer{err: tt.rpcErr}
			out := setupTabTest(t, server)
			rootCmd.SetArgs(append([]string{"tab", "create"}, tt.args...))
			err := rootCmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if out.Len() != 0 {
					t.Fatalf("failed creation printed output: %q", out.String())
				}
			} else if err != nil || out.String() != "new-tab-id\n" {
				t.Fatalf("output = %q, error = %v", out.String(), err)
			}
			if tt.want == nil {
				if server.createData != nil {
					t.Fatalf("unexpected RPC: %+v", *server.createData)
				}
				return
			}
			if server.createData == nil || !reflect.DeepEqual(*server.createData, *tt.want) {
				t.Fatalf("RPC data = %+v, want %+v", server.createData, *tt.want)
			}
		})
	}
}

func TestTabRename(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		env       string
		wantTabId string
		rpcErr    error
		wantErr   string
	}{
		{name: "current tab", args: []string{"Home"}, env: "env-tab", wantTabId: "env-tab"},
		{name: "explicit tab", args: []string{"--tab", "other-tab", "Build logs"}, env: "env-tab", wantTabId: "other-tab"},
		{name: "no tab", args: []string{"Home"}, wantErr: "no tab id specified"},
		{name: "empty name", args: []string{""}, env: "env-tab", wantErr: "must not be empty"},
		{name: "server failure", args: []string{"Home"}, env: "env-tab", wantTabId: "env-tab", rpcErr: fmt.Errorf("tab not found"), wantErr: "renaming tab: tab not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WAVETERM_TABID", tt.env)
			server := &tabTestServer{err: tt.rpcErr}
			out := setupTabTest(t, server)
			rootCmd.SetArgs(append([]string{"tab", "rename"}, tt.args...))
			err := rootCmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if out.Len() != 0 {
					t.Fatalf("failed rename printed output: %q", out.String())
				}
				if server.renameTabId != tt.wantTabId {
					t.Fatalf("rename RPC tab = %q, want %q", server.renameTabId, tt.wantTabId)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if server.renameTabId != tt.wantTabId || server.renameName != tt.args[len(tt.args)-1] {
				t.Fatalf("RPC got tab=%q name=%q", server.renameTabId, server.renameName)
			}
			if !strings.Contains(out.String(), "renamed tab "+tt.wantTabId) {
				t.Fatalf("unexpected output %q", out.String())
			}
		})
	}
}

func TestTabFocus(t *testing.T) {
	server := &tabTestServer{}
	out := setupTabTest(t, server)
	rootCmd.SetArgs([]string{"tab", "focus", "target-tab"})
	if err := rootCmd.Execute(); err != nil || server.focusTabId != "target-tab" || out.Len() != 0 {
		t.Fatalf("focus: err=%v tab=%q out=%q", err, server.focusTabId, out.String())
	}
	server.err = fmt.Errorf("tab not found")
	rootCmd.SetArgs([]string{"tab", "focus", "missing"})
	if err := rootCmd.Execute(); err == nil || !strings.Contains(err.Error(), "focusing tab: tab not found") {
		t.Fatalf("expected wrapped server error, got %v", err)
	}
}

func TestTabHelpWithoutWave(t *testing.T) {
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
	for _, text := range []string{"--no-activate", "--meta", "WAVETERM_WORKSPACEID", "no OS window focus is requested"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("help missing %q: %s", text, out.String())
		}
	}
}

func TestTabCommandsRequireWaveAuthentication(t *testing.T) {
	t.Setenv(wshutil.WaveJwtTokenVarName, "")
	for _, c := range []*cobra.Command{tabCreateCmd, tabRenameCmd, tabFocusCmd} {
		if err := c.PreRunE(c, nil); err == nil || !strings.Contains(err.Error(), "WAVETERM_JWT not found") {
			t.Fatalf("%s: expected missing authentication error, got %v", c.Name(), err)
		}
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
