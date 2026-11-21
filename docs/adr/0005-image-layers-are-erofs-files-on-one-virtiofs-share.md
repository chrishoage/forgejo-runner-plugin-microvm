# Image layers are EROFS files shared over one virtiofs share

The image cache converts each image layer once, from its uncompressed tar stream, into an uncompressed EROFS file named by its diffID, `<cache>/layers/<diffID hex>.erofs`, with go-erofs and continuity's `tarconv`, in the plugin process and without privilege. A VM sees its image through its layer link directory, `<cache>/jobs/<id>/<n>/`: one hard link per layer, named by position. libkrun shares that directory into the VM as one read-only virtiofs share, beside the VM's boot root, which it shares read-only as the VM's root. Inside the VM, the agent mounts each layer file as a file-backed EROFS (no loop device) and stacks them with one overlay, built with `fsopen` and one `lowerdir+` per layer, top first, over the boot root, with its upper and work directories on the VM's ext4 scratch disk. It then moves the overlay to `/` and chroots.

This fixes the cache's format, what the guest kernel must provide (`EROFS_FS_BACKED_BY_FILE`, overlay's `lowerdir+` from Linux 6.8: libkrunfw 5.6.4, Linux 6.12, has both) and how the agent boots.

## Considered options

- **Flatten each image into a directory tree per VM, shared over virtiofs** (the First workflow's way): every Start writes the whole image again, nothing is shared between images, and owners, special bits, device nodes and extended attributes need privilege or a user namespace on the host to reproduce.
- **One merged EROFS file per image:** one file per image, but a layer shared by several images is stored once for each, and a new tag of an image rewrites all of it.
- **A read-only block device per layer:** libkrun allows 3 mounts plus disks per VM on x86_64, far below the 120 layers an image may have (GST-3).
- **Unpack layers on the host and stack them with the host's overlayfs or loop-mounted EROFS:** needs privilege, loop devices or host EROFS support, which the plugin must not assume.

## Consequences

- A layer shared by several images is stored once (IMG-5), and its file's presence is the cache's index. Eviction is one unlink per layer.
- The guest's root keeps everything a tar carries, owners included, with no user namespace or subordinate ids on the host (IMG-4). The host only ever reads the layer files.
- Each VM can read only its own image's layers, because its share is its own link directory (IMG-8), and the host enforces read-only, so the guest can't write a layer even after remounting the share read-write (IMG-9).
- A layer that hard-links a file from an earlier layer can't be represented: a hard link can't cross two EROFS files. Start fails with an error saying the image cache does not support it (IMG-10).
- An image may have at most 499 layers: overlay stacks at most 500, and the boot root is one of them.
- The guest kernel's requirements are the libraries' minimums (`docs/development.md`); an older libkrunfw would fail at boot, not at startup.
