// SPDX-License-Identifier: GPL-3.0-or-later

// Package imagecache is the image cache: it pulls images, converts each layer
// once to EROFS, keeps the cache under its size cap, and makes and removes
// each VM's layer link directory. The plugin reaches it through its
// ImageCache seam.
package imagecache

// Request is what the plugin asks of the image cache for one VM of a job. It
// is a struct, not positional arguments, so fields such as per-image registry
// credentials can be added later without changing callers.
type Request struct {
	// Job is the job's environment id.
	Job string
	// VM is the VM's index in the job spec's VMs; 0 is the job's container.
	// It names the VM's layer link directory, <cache>/jobs/<id>/<n>/. The
	// VM's name is never used, because the workflow chooses a service's name.
	VM int
	// Ref is the image reference to acquire.
	Ref string
	// ForcePull looks a cached tag up again (IMG-6).
	ForcePull bool
}

// Image is an image as the image cache hands it to one VM of a job.
type Image struct {
	// Digest is the image's manifest digest, such as "sha256:…".
	Digest string
	// Env is the image config's environment, as KEY=VALUE in the image's
	// order.
	Env []string
	// User is the image config's USER, empty when it sets none.
	User string
	// WorkingDir is the image config's WORKDIR, empty when it sets none.
	WorkingDir string
	// LinkDir is the VM's layer link directory, <cache>/jobs/<id>/<n>/:
	// hard links to exactly the layers of the VM's image.
	LinkDir string
	// Layers are the image's layers as file names in LinkDir, bottom layer
	// first: 0.erofs, 1.erofs, and so on.
	Layers []string
	// Pulled counts the image's distinct layers the cache lacked when this
	// Acquire looked: each pulled and converted by it, or by another
	// Acquire whose pull it shared. A layer pulled again after it was
	// deleted from under the cache counts once, so Pulled is never more
	// than len(Layers).
	Pulled int
}
