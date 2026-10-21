// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"github.com/wavetermdev/waveterm/pkg/wshutil"
)

const (
	restartTestCurrentBlock = "6f1f6a2e-0d0c-4c3a-9f0e-0a6b0a1d8a11"
	restartTestCurrentTab   = "b0f1a4c2-7f5e-4d5a-8c0d-2e6b7a9c1d22"
	restartTestOtherBlock   = "62289c70-c9fa-4f90-bd20-de67bf13c493"
)

// blocksTestServer records the block RPCs issued by the CLI.
type blocksTestServer struct {
	resync *wshrpc.CommandControllerResyncData
	err    error
}

func (*blocksTestServer) WshServerImpl() {}

func (s *blocksTestServer) ResolveIdsCommand(ctx context.Context, data wshrpc.CommandResolveIdsData) (wshrpc.CommandResolveIdsRtnData, error) {
	rtn := wshrpc.CommandResolveIdsRtnData{ResolvedIds: map[string]waveobj.ORef{}}
	for _, id := range data.Ids {
		switch id {
		case "this":
			rtn.ResolvedIds[id] = waveobj.MakeORef(waveobj.OType_Block, data.BlockId)
		case restartTestOtherBlock, restartTestCurrentBlock:
			rtn.ResolvedIds[id] = waveobj.MakeORef(waveobj.OType_Block, id)
		}
	}
	return rtn, nil
}

func (s *blocksTestServer) ControllerResyncCommand(ctx context.Context, data wshrpc.CommandControllerResyncData) error {
	s.resync = &data
	return s.err
}

func (*blocksTestServer) WshActivityCommand(ctx context.Context, data map[string]int) error {
	return nil
}

func setupBlocksTest(t *testing.T, impl *blocksTestServer) *bytes.Buffer {
	t.Helper()
	client := wshutil.MakeWshRpc(wshrpc.RpcContext{}, nil, "blocks-test-client")
	server := wshutil.MakeWshRpc(wshrpc.RpcContext{}, impl, "blocks-test-server")
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
					if !dst.SendRpcMessage(msg, 0, "blocks-test") {
						t.Error("test RPC input channel full")
					}
				}
			}
		}(pair[0], pair[1])
	}
	var out bytes.Buffer
	oldClient, oldOut, oldPreRun := RpcClient, WrappedStdout, blocksRestartCmd.PreRunE
	RpcClient, WrappedStdout, blocksRestartCmd.PreRunE = client, &out, nil
	blockArg, blocksRestartTabId = "", ""
	if rootCmd.PersistentFlags().Lookup("block") == nil {
		// Execute() registers the global -b flag; tests drive rootCmd directly.
		rootCmd.PersistentFlags().StringVarP(&blockArg, "block", "b", "", "for commands which require a block id")
	}
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	blocksRestartCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	t.Cleanup(func() {
		blocksRestartCmd.PreRunE = oldPreRun
		rootCmd.SetArgs(nil)
		RpcClient, WrappedStdout = oldClient, oldOut
		close(done)
		wg.Wait()
		close(client.InputCh)
		close(server.InputCh)
	})
	return &out
}

func TestBlocksRestart(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		want    *wshrpc.CommandControllerResyncData
		rpcErr  error
		wantErr string
	}{
		{name: "current block uses the caller's tab", want: &wshrpc.CommandControllerResyncData{TabId: restartTestCurrentTab, BlockId: restartTestCurrentBlock, ForceRestart: true}},
		{name: "other block lets Wave resolve the tab", args: []string{"-b", restartTestOtherBlock},
			want: &wshrpc.CommandControllerResyncData{BlockId: restartTestOtherBlock, ForceRestart: true}},
		{name: "explicit tab", args: []string{"-b", restartTestOtherBlock, "--tab", "tab-x"},
			want: &wshrpc.CommandControllerResyncData{TabId: "tab-x", BlockId: restartTestOtherBlock, ForceRestart: true}},
		{name: "unknown block", args: []string{"-b", "99"}, wantErr: "id not found"},
		{name: "server failure", args: []string{"-b", restartTestOtherBlock}, rpcErr: fmt.Errorf("error getting block"), wantErr: "restarting block: error getting block",
			want: &wshrpc.CommandControllerResyncData{BlockId: restartTestOtherBlock, ForceRestart: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WAVETERM_BLOCKID", restartTestCurrentBlock)
			t.Setenv("WAVETERM_TABID", restartTestCurrentTab)
			server := &blocksTestServer{err: tt.rpcErr}
			out := setupBlocksTest(t, server)
			rootCmd.SetArgs(append([]string{"blocks", "restart"}, tt.args...))
			err := rootCmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if out.Len() != 0 {
					t.Fatalf("failed restart printed output: %q", out.String())
				}
			} else if err != nil || !strings.Contains(out.String(), "restarted block "+tt.want.BlockId) {
				t.Fatalf("output = %q, error = %v", out.String(), err)
			}
			if tt.want == nil {
				if server.resync != nil {
					t.Fatalf("unexpected resync RPC: %+v", *server.resync)
				}
				return
			}
			if server.resync == nil || *server.resync != *tt.want {
				t.Fatalf("resync RPC = %+v, want %+v", server.resync, *tt.want)
			}
		})
	}
}
