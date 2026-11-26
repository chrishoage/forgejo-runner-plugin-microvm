// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agentclient"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
)

// The job spec's and boot config's fixed values, as the plugin sets them
// (internal/plugin's start.go and network.go). The plugin can't write a
// spec of two VMs yet, so this test writes one itself, and its test binary
// can't link internal/plugin, whose copy of the runner's plugin protocol
// act/runner registers too.
const (
	vmsAgentPort     = 1024
	vmsLayerTag      = "layers"
	vmsGuestLayerDir = "/dev/.runner-microvm/layers"
	vmsScratchDevice = "/dev/vda"
	vmsAgentDir      = ".runner-microvm"
	vmsDefaultPath   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// vmsEventBound is how long the test waits for each of the job process's
// events: a boot takes about 2 s, and a kill's VMExited comes at once.
const vmsEventBound = 60 * time.Second

// severalVMs is a job of several VMs the test runs on the built job binary
// through the job launcher, as the plugin does.
type severalVMs struct {
	spec   *jobv1.Spec
	proc   *joblaunch.Process
	agents []*agentclient.Client
	pids   []int
	jobLog string
}

// vmsSpec writes the files of a job of one VM per name, each booted from
// the image e2e/image pins with 1 CPU and 512 MiB, and returns its spec.
// Every VM's hosts list names every VM, as the plugin's will.
func vmsSpec(t *testing.T, names ...string) *jobv1.Spec {
	t.Helper()
	jobBin, err := goBuild(module + "/cmd/runner-microvm-job")
	if err != nil {
		t.Fatal(err)
	}
	agentBin, err := goBuild(module + "/cmd/runner-microvm-agent")
	if err != nil {
		t.Fatal(err)
	}
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		t.Fatalf("the dev shell carries mkfs.ext4: %v", err)
	}
	base := t.TempDir()
	// The agent socket's path must fit connect's 107 bytes.
	runtime, err := os.MkdirTemp("", "vms-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	cache, err := imagecache.New(slog.New(slog.DiscardHandler), filepath.Join(base, "cache"), imagecache.WithMaxSize(4<<30))
	if err != nil {
		t.Fatal(err)
	}
	var hosts []*agentv1.Host
	for i, name := range names {
		hosts = append(hosts, &agentv1.Host{Name: name, Address: vmsAddress(i)})
	}
	spec := &jobv1.Spec{
		Job:       "several-vms",
		JobBinary: jobBin,
		LibDir:    devLibDir(t),
		MkfsExt4:  mkfs,
		LogLevel:  "debug",
		Network:   &jobv1.Network{Subnet: "198.18.0.0/24", GatewayAddress: "198.18.0.1", GatewayMac: "02:00:c6:12:00:01", Mtu: 1500},
		// libkrun's level, for the VM log a failure shows.
		LibkrunLogLevel: "info",
	}
	ref := readImage(t)
	for i, name := range names {
		img, err := cache.Acquire(t.Context(), imagecache.Request{Job: spec.GetJob(), VM: i, Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(base, strconv.Itoa(i))
		sock := filepath.Join(runtime, strconv.Itoa(i), "agent.sock")
		for _, d := range []string{state, filepath.Dir(sock)} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		boot := &agentv1.BootConfig{
			LayerDir:      vmsGuestLayerDir,
			LayerTag:      vmsLayerTag,
			Layers:        img.Layers,
			ScratchDevice: vmsScratchDevice,
			Network:       &agentv1.Network{Address: vmsAddress(i) + "/24", Gateway: "198.18.0.1", Mtu: 1500},
			Hostname:      name,
			Image:         &agentv1.Image{Env: img.Env, User: img.User, WorkingDir: img.WorkingDir, DefaultPath: vmsDefaultPath},
			Hosts:         hosts,
			VsockPort:     vmsAgentPort,
			LogLevel:      "info",
		}
		root := filepath.Join(state, "root")
		writeBootRoot(t, root, agentBin, boot)
		spec.Vms = append(spec.Vms, &jobv1.VM{
			Name: name, Cpus: 1, MemoryMib: 512,
			ScratchDisk: filepath.Join(state, "scratch.ext4"), ScratchSizeBytes: 1 << 30,
			VmLog: filepath.Join(state, "vm.log"), Console: filepath.Join(state, "console"),
			AgentSocket: sock, AgentPort: vmsAgentPort,
			RootDir: root, LayerDir: img.LinkDir, LayerTag: vmsLayerTag,
			GuestAddress: vmsAddress(i), GuestMac: fmt.Sprintf("02:00:c6:12:00:%02x", 2+i),
			Boot: boot,
		})
	}
	return spec
}

// vmsAddress is the job-network address of the VM at index i.
func vmsAddress(i int) string { return "198.18.0." + strconv.Itoa(2+i) }

// writeBootRoot makes a VM's boot root at root, as the plugin does: the
// mount points, and the agent and its boot config in .runner-microvm/.
func writeBootRoot(t *testing.T, root, agent string, boot *agentv1.BootConfig) {
	t.Helper()
	for _, d := range []string{"", "proc", "sys", "dev", vmsAgentDir} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, vmsAgentDir, "agent"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := proto.Marshal(boot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, vmsAgentDir, "boot-config.binpb"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
}

// launchVMs runs spec's job on the built job binary through a job launcher
// of the test's own, waits for NetworkUp and every VM's VMStarted and for
// every agent to answer, and kills the job on cleanup.
func launchVMs(t *testing.T, spec *jobv1.Spec) *severalVMs {
	t.Helper()
	l, err := joblaunch.New(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	j := &severalVMs{spec: spec, jobLog: filepath.Join(filepath.Dir(filepath.Dir(spec.GetVms()[0].GetRootDir())), "job.log")}
	j.proc, err = l.Launch(t.Context(), spec, j.jobLog)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := l.Kill(ctx, j.proc); err != nil {
			t.Errorf("kill the job process: %v", err)
		}
		l.Close()
		j.logIfFailed(t)
	})
	if ev := j.next(t); ev.GetNetworkUp() == nil {
		t.Fatalf("first event = %v, want NetworkUp", ev)
	}
	j.pids = make([]int, len(spec.GetVms()))
	for i := range spec.GetVms() {
		s := j.next(t).GetVmStarted()
		if s == nil || int(s.GetVm()) != i || s.GetPid() <= 0 {
			t.Fatalf("event = %v, want VM %d's VMStarted", s, i)
		}
		j.pids[i] = int(s.GetPid())
	}
	for i, v := range spec.GetVms() {
		a, err := agentclient.New(v.GetAgentSocket())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), vmsEventBound)
		err = a.WaitReady(ctx)
		cancel()
		if err != nil {
			t.Fatalf("VM %d's agent: %v", i, err)
		}
		j.agents = append(j.agents, a)
	}
	return j
}

// next returns the job process's next event, failing t unless one comes
// within vmsEventBound.
func (j *severalVMs) next(t *testing.T) *jobv1.Event {
	t.Helper()
	select {
	case ev, ok := <-j.proc.Events:
		if !ok {
			t.Fatal("the job process's events ended")
		}
		return ev
	case <-time.After(vmsEventBound):
		t.Fatalf("no event within %v", vmsEventBound)
		return nil
	}
}

// exec runs argv in VM i with Exec and returns its exit code and output,
// stdout then stderr.
func (j *severalVMs) exec(ctx context.Context, i int, argv ...string) (int32, string, error) {
	stream, err := j.agents[i].Exec(ctx, &agentv1.ExecRequest{Argv: argv})
	if err != nil {
		return 0, "", err
	}
	var stdout, stderr bytes.Buffer
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return 0, "", fmt.Errorf("Exec %v: no exit code", argv)
		}
		if err != nil {
			return 0, "", fmt.Errorf("Exec %v: %w", argv, err)
		}
		switch o := out.GetEvent().(type) {
		case *agentv1.ExecResponse_Stdout:
			stdout.Write(o.Stdout)
		case *agentv1.ExecResponse_Stderr:
			stderr.Write(o.Stderr)
		case *agentv1.ExecResponse_ExitCode:
			return o.ExitCode, stdout.String() + stderr.String(), nil
		case *agentv1.ExecResponse_StartError:
			return 0, "", fmt.Errorf("Exec %v: %s", argv, o.StartError)
		}
	}
}

// stop kills VM i's VM child, as a VM that stops on its own ends, and
// waits for its VMExited.
func (j *severalVMs) stop(t *testing.T, i int) {
	t.Helper()
	if err := syscall.Kill(j.pids[i], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	x := j.next(t).GetVmExited()
	if x == nil || int(x.GetVm()) != i || x.GetExitCode() != 128+9 {
		t.Fatalf("event = %v, want VM %d's VMExited with 137", x, i)
	}
}

// logIfFailed logs the job process log and each VM's VM log and console
// if t has failed.
func (j *severalVMs) logIfFailed(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	files := []string{j.jobLog}
	for _, v := range j.spec.GetVms() {
		files = append(files, v.GetVmLog(), v.GetConsole())
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		t.Logf("%s:\n%s", f, b)
	}
}

// connect has VM from open a TCP connection to VM to by its name, and fails
// t unless the line it sends arrives: VM to listens with busybox's nc, and
// VM from retries until the listener is up.
func (j *severalVMs) connect(t *testing.T, from, to int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), vmsEventBound)
	defer cancel()
	type result struct {
		code int32
		out  string
		err  error
	}
	listened := make(chan result, 1)
	go func() {
		code, out, err := j.exec(ctx, to, "nc", "-l", "-p", "7000")
		listened <- result{code, out, err}
	}()
	line := "hello from " + j.spec.GetVms()[from].GetName()
	name := j.spec.GetVms()[to].GetName()
	script := `for i in $(seq 100); do echo "$1" | nc -w 5 "$2" 7000 && exit 0; sleep 0.1; done; exit 1`
	code, out, err := j.exec(ctx, from, "sh", "-c", script, "sh", line, name)
	if err != nil || code != 0 {
		t.Fatalf("VM %d to %s: exit code %d, %v:\n%s", from, name, code, err, out)
	}
	r := <-listened
	if r.err != nil || r.code != 0 || strings.TrimSpace(r.out) != line {
		t.Fatalf("VM %s's listener: exit code %d, %v, output %q; want %q", name, r.code, r.err, r.out, line)
	}
	code, out, err = j.exec(ctx, from, "getent", "hosts", name)
	if f := strings.Fields(out); err != nil || code != 0 || len(f) == 0 || f[0] != vmsAddress(to) {
		t.Errorf("VM %d resolves %s as %q (exit code %d, %v), want %s", from, name, out, code, err, vmsAddress(to))
	}
}

// A job process runs every VM of its job spec, here two from the test
// image on the built job binary: each resolves the other by name and opens
// a TCP connection to it; stopping VM 1 leaves VM 0 running and reports VM
// 1's VMExited; stopping VM 0 ends the job process once both VM children
// are reaped.
func TestJobRunsSeveralVMs(t *testing.T) {
	t.Parallel()
	j := launchVMs(t, vmsSpec(t, "several-vms", "db"))
	j.connect(t, 0, 1)
	j.connect(t, 1, 0)

	j.stop(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), vmsEventBound)
	defer cancel()
	if code, out, err := j.exec(ctx, 0, "hostname"); err != nil || code != 0 || strings.TrimSpace(out) != "several-vms" {
		t.Fatalf("VM 0 after VM 1 stopped: exit code %d, %v, output %q", code, err, out)
	}
	select {
	case ev := <-j.proc.Events:
		t.Fatalf("an event after VM 1 stopped, with VM 0 running: %v", ev)
	default:
	}

	j.stop(t, 0)
	select {
	case <-j.proc.Exited:
	case <-time.After(vmsEventBound):
		t.Fatalf("the job process still runs %v after VM 0 stopped", vmsEventBound)
	}
	if ev, ok := <-j.proc.Events; ok {
		t.Errorf("an event after both VMs stopped: %v", ev)
	}
	for i, pid := range j.pids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("VM %d's VM child %d is not reaped: %v", i, pid, err)
		}
	}
}
