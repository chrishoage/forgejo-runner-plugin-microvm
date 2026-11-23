// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// started returns the job id names, locked, with a call context that Remove
// also cancels, or FailedPrecondition before Start completed (PROTO-11):
// at once while Start is still in progress, without waiting for the job's
// lock. Its wait for the lock ends with ctx. The caller calls done.
func (s *Server) started(ctx context.Context, id string) (*job, context.Context, func(), error) {
	j, err := s.lookup(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if !j.ready.Load() {
		return nil, nil, nil, notStarted(id)
	}
	if err := s.lockJob(ctx, j); err != nil {
		return nil, nil, nil, err
	}
	if j.agent == nil {
		j.mu.Unlock()
		return nil, nil, nil, notStarted(id)
	}
	cctx, cancel := callContext(ctx, j)
	return j, cctx, func() { cancel(); j.mu.Unlock() }, nil
}

func notStarted(id string) error {
	return status.Errorf(codes.FailedPrecondition, "environment %s is not started", id)
}

// agentError is the status for an agent call that failed: the call's own
// context's code once it ended, the agent's status otherwise.
func agentError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return err
}

// Exec runs the command through the job's container's agent and relays its
// output, then its exit code or why it could not start. The env values are
// secrets: they are never logged (LOG-2).
func (s *Server) Exec(req *pluginv1alpha.ExecRequest, stream pluginv1alpha.BackendPlugin_ExecServer) error {
	j, ctx, done, err := s.started(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer done()
	out, err := j.agent.Exec(ctx, &agentv1.ExecRequest{
		Argv:       req.GetCommand(),
		Env:        req.GetEnv(),
		User:       req.GetUser(),
		WorkingDir: req.GetWorkdir(),
	})
	if err != nil {
		return agentError(ctx, err)
	}
	for {
		resp, err := out.Recv()
		if errors.Is(err, io.EOF) {
			return status.Error(codes.Unavailable, "the agent ended Exec without an exit code")
		}
		if err != nil {
			return agentError(ctx, err)
		}
		var o *pluginv1alpha.ExecOutput
		switch ev := resp.GetEvent().(type) {
		case *agentv1.ExecResponse_Stdout:
			o = execData(pluginv1alpha.DataChunk_STDOUT, ev.Stdout)
		case *agentv1.ExecResponse_Stderr:
			o = execData(pluginv1alpha.DataChunk_STDERR, ev.Stderr)
		case *agentv1.ExecResponse_ExitCode:
			return await(ctx, func() error {
				return stream.Send(&pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_ExecComplete{
					ExecComplete: &pluginv1alpha.ExecComplete{ExitCode: ev.ExitCode},
				}})
			})
		case *agentv1.ExecResponse_StartError:
			return await(ctx, func() error {
				return stream.Send(&pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_ExecFailed{
					ExecFailed: &pluginv1alpha.ExecFailed{ErrorMessage: ev.StartError},
				}})
			})
		default:
			continue
		}
		// A runner that stops reading can't keep the job held (await).
		if err := await(ctx, func() error { return stream.Send(o) }); err != nil {
			return err
		}
	}
}

func execData(s pluginv1alpha.DataChunk_Stream, b []byte) *pluginv1alpha.ExecOutput {
	return &pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_Data{Data: &pluginv1alpha.DataChunk{Stream: s, Data: b}}}
}

// CopyIn relays a tar to the agent, which extracts it at the destination
// the first chunk names.
func (s *Server) CopyIn(stream pluginv1alpha.BackendPlugin_CopyInServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.EnvironmentId == nil || first.DestPath == nil {
		return status.Error(codes.InvalidArgument, "the first chunk must set environment_id and dest_path")
	}
	j, ctx, done, err := s.started(stream.Context(), first.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer done()
	in, err := j.agent.CopyIn(ctx)
	if err != nil {
		return agentError(ctx, err)
	}
	// Send's own errors surface in CloseAndRecv.
	sendErr := in.Send(&agentv1.CopyInRequest{Destination: first.GetDestPath(), Chunk: first.GetData()})
	for sendErr == nil {
		// A runner that stops sending can't keep the job held (await).
		var chunk *pluginv1alpha.CopyInChunk
		err := await(ctx, func() error {
			var err error
			chunk, err = stream.Recv()
			return err
		})
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if chunk.EnvironmentId != nil || chunk.DestPath != nil {
			return status.Error(codes.InvalidArgument, "only the first chunk may set environment_id or dest_path")
		}
		sendErr = in.Send(&agentv1.CopyInRequest{Chunk: chunk.GetData()})
	}
	if _, err := in.CloseAndRecv(); err != nil {
		return agentError(ctx, err)
	}
	return await(ctx, func() error { return stream.SendAndClose(&pluginv1alpha.CopyInResponse{}) })
}

// CopyOut relays the agent's tar of a guest path.
func (s *Server) CopyOut(req *pluginv1alpha.CopyOutRequest, stream pluginv1alpha.BackendPlugin_CopyOutServer) error {
	j, ctx, done, err := s.started(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer done()
	out, err := j.agent.CopyOut(ctx, &agentv1.CopyOutRequest{Path: req.GetSrcPath()})
	if err != nil {
		return agentError(ctx, err)
	}
	for {
		resp, err := out.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return agentError(ctx, err)
		}
		// A runner that stops reading can't keep the job held (await).
		if err := await(ctx, func() error { return stream.Send(&pluginv1alpha.CopyOutChunk{Data: resp.GetChunk()}) }); err != nil {
			return err
		}
	}
}
