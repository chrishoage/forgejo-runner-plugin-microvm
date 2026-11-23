// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"
	"path"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// started returns the job id names, locked, with a call context that Remove
// and the death of the job's VM also cancel (callContext), or
// FailedPrecondition before Start completed (PROTO-11), at once while Start
// is still in progress, without waiting for the job's lock, and once its VM
// has died. Its wait for the lock ends with ctx. The caller calls done.
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
	if j.gone.Err() != nil {
		j.mu.Unlock()
		return nil, nil, nil, context.Cause(j.gone)
	}
	cctx, cancel := callContext(ctx, j)
	return j, cctx, func() { cancel(); j.mu.Unlock() }, nil
}

func notStarted(id string) error {
	return status.Errorf(codes.FailedPrecondition, "environment %s is not started", id)
}

// agentError is the status for an agent call on j that failed with err,
// with ctx its call context. Any failure but the call's own end may be how
// the call first sees its VM die, moments before the job process reports
// it, so the call then waits for that report, at most the lost agent grace,
// and the handler's callError returns the VM's status. A failure other than
// a broken connection (Unavailable) waits only once a Ping shows the agent
// gone: an agent that answers is alive, and the call returns its own error
// at once. A VM not reported exited by the end of the grace fails the call
// with FailedPrecondition, naming the VM: never Unavailable, which the
// runner reads as the plugin being gone.
func (s *Server) agentError(ctx context.Context, j *job, err error) error {
	if ctx.Err() != nil {
		return err
	}
	graceCtx, cancel := context.WithTimeout(ctx, s.cfg.LostAgentGrace)
	defer cancel()
	if status.Code(err) != codes.Unavailable && j.agent.Ping(graceCtx) == nil {
		return err
	}
	<-graceCtx.Done()
	if ctx.Err() != nil {
		return err
	}
	return status.Errorf(codes.FailedPrecondition, "VM %s: the connection to its agent broke, and the VM has not exited: %s",
		j.hostname, status.Convert(err).Message())
}

// workingDir is the working directory Exec asks the agent for: the
// request's, with a relative one taken from the job's workspace, the
// request's GITHUB_WORKSPACE, when that is absolute. The runner sends a
// step's relative working-directory unchanged; its own container backend
// takes it from the container's working directory, which is the path it
// gives steps as GITHUB_WORKSPACE, and the plugin protocol passes the
// plugin no other base. Otherwise it is the request's, unchanged, and the
// agent refuses one that is relative.
func workingDir(req *pluginv1alpha.ExecRequest) string {
	dir := req.GetWorkdir()
	workspace := req.GetEnv()["GITHUB_WORKSPACE"]
	if dir == "" || path.IsAbs(dir) || !path.IsAbs(workspace) {
		return dir
	}
	return path.Join(workspace, dir)
}

// Exec runs the command through the job's container's agent and relays its
// output, then its exit code or why it could not start. The env values are
// secrets: they are never logged (LOG-2).
func (s *Server) Exec(req *pluginv1alpha.ExecRequest, stream pluginv1alpha.BackendPlugin_ExecServer) (err error) {
	j, ctx, done, err := s.started(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer done()
	defer func() { err = callError(ctx, j, err) }()
	out, err := j.agent.Exec(ctx, &agentv1.ExecRequest{
		Argv:       req.GetCommand(),
		Env:        req.GetEnv(),
		User:       req.GetUser(),
		WorkingDir: workingDir(req),
	})
	if err != nil {
		return s.agentError(ctx, j, err)
	}
	for {
		resp, err := out.Recv()
		if errors.Is(err, io.EOF) {
			// The agent ships in the plugin: its bug is the plugin's own.
			err = status.Errorf(codes.Internal, "VM %s: its agent ended Exec without an exit code", j.hostname)
		}
		if err != nil {
			return s.agentError(ctx, j, err)
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
// the first chunk names, re-chunked to at most agentChunk.
func (s *Server) CopyIn(stream pluginv1alpha.BackendPlugin_CopyInServer) (err error) {
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
	defer func() { err = callError(ctx, j, err) }()
	in, err := j.agent.CopyIn(ctx)
	if err != nil {
		return s.agentError(ctx, j, err)
	}
	// Send's own errors surface in CloseAndRecv.
	sendErr := sendChunks(in, first.GetDestPath(), first.GetData())
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
		sendErr = sendChunks(in, "", chunk.GetData())
	}
	if _, err := in.CloseAndRecv(); err != nil {
		return s.agentError(ctx, j, err)
	}
	return await(ctx, func() error { return stream.SendAndClose(&pluginv1alpha.CopyInResponse{}) })
}

// agentChunk is the most of a copy the plugin sends the agent in one
// message. The agent keeps gRPC's 4 MiB default limit, while the runner may
// send chunks of up to 32 MiB, the plugin's own limit.
const agentChunk = 256 << 10

// sendChunks sends data to the agent's CopyIn in messages of at most
// agentChunk, at least one, the first naming dest.
func sendChunks(in agentv1.AgentService_CopyInClient, dest string, data []byte) error {
	for first := true; first || len(data) > 0; first = false {
		n := min(len(data), agentChunk)
		if err := in.Send(&agentv1.CopyInRequest{Destination: dest, Chunk: data[:n]}); err != nil {
			return err
		}
		dest, data = "", data[n:]
	}
	return nil
}

// CopyOut relays the agent's tar of a guest path.
func (s *Server) CopyOut(req *pluginv1alpha.CopyOutRequest, stream pluginv1alpha.BackendPlugin_CopyOutServer) (err error) {
	j, ctx, done, err := s.started(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer done()
	defer func() { err = callError(ctx, j, err) }()
	out, err := j.agent.CopyOut(ctx, &agentv1.CopyOutRequest{Path: req.GetSrcPath()})
	if err != nil {
		return s.agentError(ctx, j, err)
	}
	for {
		resp, err := out.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return s.agentError(ctx, j, err)
		}
		// A runner that stops reading can't keep the job held (await).
		if err := await(ctx, func() error { return stream.Send(&pluginv1alpha.CopyOutChunk{Data: resp.GetChunk()}) }); err != nil {
			return err
		}
	}
}
