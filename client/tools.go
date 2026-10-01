package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
)

// DefaultSleepImage ships the blocking helper a one-off or init runner executes as its entrypoint.
const DefaultSleepImage = "ghcr.io/invalid/invalid/sleep:{version}"

// DefaultToolsVolume holds the default helper tools volume name.
// Set to an invalid value because client is used as a library and callers must configure it.
const DefaultToolsVolume = "invalid"

// DefaultToolsMount holds the default helper tools mount path.
// Set to an invalid value because client is used as a library and callers must configure it.
const DefaultToolsMount = "/invalid"

// toolsHelperPath is where the helpers image keeps its one binary.
const toolsHelperPath = "/sleep"

// toolsLock serializes installs into the master volume.
const toolsLock = "install"

// toolsLockStale bounds how long a crashed install keeps other runs waiting.
const toolsLockStale = 2 * time.Minute

// EnsureTools puts the helpers image's binary in the project's tools volume,
// and returns that volume with the path an instance runs as its entrypoint.
func (c *Client) EnsureTools(ctx context.Context) (*StorageVolume, string, error) {
	toolsVol := c.config.ToolsVolume
	if toolsVol == "" || toolsVol == DefaultToolsVolume {
		return nil, "", errors.New("tools volume is not configured")
	}

	toolsMount := c.config.ToolsMount
	if toolsMount == "" || toolsMount == DefaultToolsMount || !strings.HasPrefix(toolsMount, "/") {
		return nil, "", errors.New("tools mount is not configured")
	}

	imageName := c.config.SleepImage
	if imageName == "" || imageName == DefaultSleepImage {
		return nil, "", errors.New("sleep image is not configured")
	}

	globalProj := c.config.GlobalProject

	sys, err := c.Global().EnsureProject(globalProj, EnsureProjectWithCreate())
	if err != nil {
		return nil, "", fmt.Errorf("getting the global project %q: %w", globalProj, err)
	}

	release, err := c.Global().LockGlobalProject(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("locking the global project: %w", err)
	}
	release()

	defer sys.WarnError(sys.Done, "Failure during Client.Done() on the global project")

	res, err := sys.Resource(KindImage, imageName, &ImageConfig{})
	if err != nil {
		return nil, "", err
	}

	err = RunAction(ctx, res, ActionEnsure, OptionCreate())
	if err != nil {
		return nil, "", fmt.Errorf("fetching the tools image %q: %w", imageName, err)
	}

	image, ok := res.(*Image)
	if !ok {
		return nil, "", ErrUnknownResource.WithText(imageName)
	}

	conn, err := sys.Connection()
	if err != nil {
		return nil, "", err
	}

	fingerprint := image.State().IncusAlias.Target
	info, _, err := conn.GetImage(ctx, sys.IncusProject(), fingerprint, nil)
	if err != nil {
		return nil, "", ErrNotFound.WithText("reading the helpers image").Wrap(err)
	}

	helper := path.Join("/", fingerprint[:12], "sleep-"+info.Architecture)

	master, err := EnsureToolsVolume(ctx, sys)
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

// EnsureToolsVolume returns the client's tools volume, created when missing.
func EnsureToolsVolume(ctx context.Context, c *Client) (*StorageVolume, error) {
	volName := c.config.ToolsVolume
	if volName == "" || volName == DefaultToolsVolume {
		return nil, errors.New("tools volume is not configured")
	}

	res, err := c.Resource(KindStorageVolume, volName, &StorageVolumeConfig{})
	if err != nil {
		return nil, err
	}

	vol, ok := res.(*StorageVolume)
	if !ok {
		return nil, ErrUnknownResource.WithText(volName)
	}

	err = RunAction(ctx, vol, ActionEnsure, OptionCreate())
	if err != nil {
		return nil, err
	}

	return vol, nil
}

func installTools(ctx context.Context, sys *Client, master *StorageVolume, image *Image, helper string) error {
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
		return ErrCreate.WithText("taking the tools lock").Wrap(err)
	}

	defer sys.WarnError(lock.Unlock, "Failed to release the tools lock")

	_, err = sc.Stat(helper)
	if err == nil {
		return nil
	}

	src, err := image.SFTP(ctx)
	if err != nil {
		return err
	}

	from, err := src.Open(toolsHelperPath)
	if err != nil {
		return ErrNotFound.WithText("the helpers image ships no " + toolsHelperPath).Wrap(err)
	}

	defer sys.WarnError(from.Close, "Failed to close the helper being installed")

	err = sc.MkdirAll(path.Dir(helper))
	if err != nil {
		return ErrCreate.WithText("creating " + path.Dir(helper)).Wrap(err)
	}

	staging := helper + ".tmp"

	to, err := sc.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return ErrCreate.WithText("creating " + staging).Wrap(err)
	}

	_, err = io.Copy(to, from)
	if err != nil {
		sys.WarnError(to.Close, "Failed to close a half written helper")

		return ErrCreate.WithText("writing " + staging).Wrap(err)
	}

	err = to.Close()
	if err != nil {
		return ErrCreate.WithText("writing " + staging).Wrap(err)
	}

	err = sc.Chmod(staging, 0o755)
	if err != nil {
		return ErrCreate.WithText("making " + staging + " executable").Wrap(err)
	}

	err = sc.PosixRename(staging, helper)
	if err != nil {
		return ErrCreate.WithText("publishing " + helper).Wrap(err)
	}

	return nil
}

func copyTools(ctx context.Context, c *Client, sys *Client, master *StorageVolume, helper string) (*StorageVolume, error) {
	if c.IncusProject() == sys.IncusProject() {
		return master, nil
	}

	vol, err := EnsureToolsVolume(ctx, c)
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

	release, err := c.Global().Lock(ctx, "tools/copy/"+c.IncusProject(), toolsLockStale)
	if err != nil {
		return nil, ErrCreate.WithText("locking tools volume copy for " + c.IncusProject()).Wrap(err)
	}
	defer release()

	sc, err = vol.SFTP(ctx)
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

	if source.Location != "" && source.Location != "none" {
		req.Source.Location = source.Location
	}

	copyOp, err := conn.CopyStoragePoolVolume(ctx, c.IncusProject(), vol.Config.Pool, req)
	if err == nil {
		_, err = iclient.WaitOperation(ctx, copyOp)
	}

	if err != nil {
		return nil, ErrCreate.WithText("copying the tools volume into " + c.IncusProject()).Wrap(err)
	}

	return vol, nil
}
