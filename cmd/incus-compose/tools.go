package main

import (
	"context"
	"io"
	"os"
	"path"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/client"
	"github.com/lxc/incus-compose/iclient"
)

// toolsVolume holds the helper a one-off runs as its entrypoint. The name is
// long so a compose volume cannot take the same Incus name.
const toolsVolume = "incus-compose-tools"

// toolsMount is where a one-off instance sees toolsVolume.
const toolsMount = "/incus-compose-tools"

// toolsHelperPath is where the helpers image keeps its one binary.
const toolsHelperPath = "/sleep"

// toolsLock serializes installs into the master volume.
const toolsLock = "install"

// toolsLockStale bounds how long a crashed install keeps other runs waiting.
const toolsLockStale = 2 * time.Minute

// ensureTools puts the helpers image's binary in the project's tools volume,
// and returns that volume with the path a one-off runs as its entrypoint.
// image belongs to sys, the system project holding the master copy.
func ensureTools(ctx context.Context, c *client.Client, sys *client.Client, image *client.Image) (*client.StorageVolume, string, error) {
	conn, err := sys.Connection()
	if err != nil {
		return nil, "", err
	}

	fingerprint := image.State().IncusAlias.Target

	// The architecture is whatever the pull resolved to, which is the server's,
	// which is the one every instance here runs on.
	info, _, err := conn.GetImage(ctx, fingerprint, nil)
	if err != nil {
		return nil, "", client.ErrNotFound.WithText("reading the helpers image").Wrap(err)
	}

	// The fingerprint, not the tag: a tag pushed over would otherwise keep
	// naming the helper it no longer holds.
	helper := path.Join("/", fingerprint[:12], "sleep-"+info.Architecture)

	master, err := ensureToolsVolume(ctx, sys)
	if err != nil {
		return nil, "", err
	}

	err = installTools(ctx, sys, master, image, helper)
	if err != nil {
		return nil, "", err
	}

	vol, err := copyTools(ctx, c, sys, master, helper)
	if err != nil {
		return nil, "", err
	}

	return vol, path.Join(toolsMount, helper), nil
}

// ensureToolsVolume returns the client's tools volume, created when missing.
func ensureToolsVolume(ctx context.Context, c *client.Client) (*client.StorageVolume, error) {
	res, err := c.Resource(client.KindStorageVolume, toolsVolume, &client.StorageVolumeConfig{})
	if err != nil {
		return nil, err
	}

	vol, ok := res.(*client.StorageVolume)
	if !ok {
		return nil, client.ErrUnknownResource.WithText(toolsVolume)
	}

	err = client.RunAction(ctx, vol, client.ActionEnsure, client.OptionCreate())
	if err != nil {
		return nil, err
	}

	return vol, nil
}

// installTools copies the helper into the master volume unless it is already
// there. The check runs twice, once before the lock so the common case never
// takes it.
func installTools(ctx context.Context, sys *client.Client, master *client.StorageVolume, image *client.Image, helper string) error {
	sc, err := master.SFTP(ctx)
	if err != nil {
		return err
	}

	defer sys.WarnError(sc.Close, "Failed to close the tools volume connection")

	_, err = sc.Stat(helper)
	if err == nil {
		return nil
	}

	lock, err := master.Lock(ctx, sc, toolsLock, toolsLockStale)
	if err != nil {
		return client.ErrCreate.WithText("taking the tools lock").Wrap(err)
	}

	defer sys.WarnError(lock.Unlock, "Failed to release the tools lock")

	_, err = sc.Stat(helper)
	if err == nil {
		return nil
	}

	// A stopped instance mounts no disk devices, so this is the image's rootfs.
	src, err := image.SFTP(ctx)
	if err != nil {
		return err
	}

	from, err := src.Open(toolsHelperPath)
	if err != nil {
		return client.ErrNotFound.WithText("the helpers image ships no " + toolsHelperPath).Wrap(err)
	}

	defer sys.WarnError(from.Close, "Failed to close the helper being installed")

	err = sc.MkdirAll(path.Dir(helper))
	if err != nil {
		return client.ErrCreate.WithText("creating " + path.Dir(helper)).Wrap(err)
	}

	// Written under a staging name and renamed, so a reader that skips the lock
	// sees the whole binary or none of it.
	staging := helper + ".tmp"

	to, err := sc.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return client.ErrCreate.WithText("creating " + staging).Wrap(err)
	}

	_, err = io.Copy(to, from)
	if err != nil {
		sys.WarnError(to.Close, "Failed to close a half written helper")

		return client.ErrCreate.WithText("writing " + staging).Wrap(err)
	}

	err = to.Close()
	if err != nil {
		return client.ErrCreate.WithText("writing " + staging).Wrap(err)
	}

	// The one-off runs as the image's own user, so it has to be able to exec it.
	err = sc.Chmod(staging, 0o755)
	if err != nil {
		return client.ErrCreate.WithText("making " + staging + " executable").Wrap(err)
	}

	err = sc.PosixRename(staging, helper)
	if err != nil {
		return client.ErrCreate.WithText("publishing " + helper).Wrap(err)
	}

	return nil
}

// copyTools brings the master's helper into the project's own volume, which is
// the only way an instance reaches it: custom volumes are project-scoped, so
// there is nothing to mount across.
func copyTools(ctx context.Context, c *client.Client, sys *client.Client, master *client.StorageVolume, helper string) (*client.StorageVolume, error) {
	if c.IncusProject() == sys.IncusProject() {
		return master, nil
	}

	vol, err := ensureToolsVolume(ctx, c)
	if err != nil {
		return nil, err
	}

	sc, err := vol.SFTP(ctx)
	if err != nil {
		return nil, err
	}

	_, err = sc.Stat(helper)
	c.WarnError(sc.Close, "Failed to close the tools volume connection")

	if err == nil {
		return vol, nil
	}

	conn, err := c.Connection()
	if err != nil {
		return nil, err
	}

	source := master.State().IncusVolume
	req := incusApi.StorageVolumesPost{
		Name:        vol.IncusName(),
		Type:        source.Type,
		ContentType: source.ContentType,
		Source: incusApi.StorageVolumeSource{
			Name:    source.Name,
			Type:    "copy",
			Pool:    master.Config.Pool,
			Project: sys.IncusProject(),
			Refresh: true,
		},
	}

	// Cluster-internal copies must name the member the source volume is on.
	if source.Location != "" && source.Location != "none" {
		req.Source.Location = source.Location
	}

	copyOp, err := conn.CopyStoragePoolVolume(ctx, vol.Config.Pool, req)
	if err == nil {
		_, err = iclient.WaitOperation(ctx, copyOp)
	}

	if err != nil {
		return nil, client.ErrCreate.WithText("copying the tools volume into " + c.IncusProject()).Wrap(err)
	}

	return vol, nil
}

// downloadTools fetches the tools image into the system project, where `run`
// looks for it. It is the only part of a one-off that ever needs the network,
// so fetching it from `pull` is what lets `run` work on a disconnected machine
// later.
//
// A failure only warns: most projects never run a one-off, and `up` reaches
// here too, so an unreachable tools image must not take the whole command
// down. `run` says so loudly enough when it is the one that needs it.
func downloadTools(ctx context.Context, c *client.Client, init string) {
	if init == "" {
		return
	}

	image := resolveImageVersion(init)

	sys, err := c.Global().EnsureProject(systemProject, client.EnsureProjectWithCreate())
	if err != nil {
		c.LogWarn("Not fetching the tools image", "project", systemProject, "error", err)

		return
	}

	res, err := sys.Resource(client.KindImage, image, &client.ImageConfig{})
	if err != nil {
		c.LogWarn("Not fetching the tools image", "image", image, "error", err)

		return
	}

	// No pull mode: the reference is version-pinned, so refreshing it can only
	// delete what is there and fetch the same bytes again - or fail, on a
	// registry that never had this tag.
	err = client.RunAction(ctx, res, client.ActionEnsure, client.OptionCreate())
	if err != nil {
		c.LogWarn("Failed to fetch the tools image, `run` will need it", "image", image, "error", err)
	}
}
