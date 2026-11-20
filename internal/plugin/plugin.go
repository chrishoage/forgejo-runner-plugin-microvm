// SPDX-License-Identifier: GPL-3.0-or-later

// Package plugin is the plugin: the runner's BackendPlugin gRPC server and the
// job table. It reaches the image cache and job processes through two seams,
// ImageCache and JobLauncher, so its tests run with fakes and no KVM, and the
// agent through internal/agentclient.
package plugin

import (
	"context"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"

	// SHORTCUT(#4): a blank import links the payload into the plugin binary
	// until the plugin writes out the job binary at startup.
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/payload"
)

// ImageCache is the image cache as the plugin uses it. internal/imagecache is
// the real adapter.
type ImageCache interface {
	// Acquire returns req.Ref's image for the VM at index req.VM of req.Job,
	// pulling and converting whatever the cache lacks, and creates that VM's
	// layer link directory, <cache>/jobs/<id>/<n>/, keyed by the index, never
	// the VM's name. Each VM gets its own Image, since each may bring its own
	// image. Today only VM 0, the job's container, acquires one, because the
	// plugin protocol passes no VM options for services.
	Acquire(ctx context.Context, req imagecache.Request) (imagecache.Image, error)
	// Release deletes the layer link directories of every VM of job at once,
	// so their layers can be evicted. A job with none is not an error. It
	// waits for any in-flight Acquire for the same job, for any of its VMs,
	// to return first, so a cancelled Acquire can't finish a directory after
	// it is deleted.
	Release(ctx context.Context, job string) error
}

// JobLauncher starts and kills job processes. internal/joblaunch is the real
// adapter.
type JobLauncher interface {
	// Launch starts a job process, with its stderr on the job process log at
	// logPath, and writes spec to its control channel. ctx bounds the launch
	// only: the job process runs until it exits or Kill. When Launch returns
	// an error, it has killed and reaped every process it started.
	Launch(ctx context.Context, spec *jobv1.Spec, logPath string) (*joblaunch.Process, error)
	// Kill kills the job process's group, which ends its VM children, job
	// network and disk locks, and returns only once every process in the
	// group has exited and been reaped; the plugin is a child subreaper, so
	// that includes the VM children. Only then are the job's disks free. It
	// never waits on p.Events being read. A process that is already gone is
	// not an error.
	Kill(ctx context.Context, p *joblaunch.Process) error
}
