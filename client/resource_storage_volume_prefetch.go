package client

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"time"

	"github.com/avast/retry-go/v5"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/pkg/sftp"

	"github.com/lxc/incus-compose/iclient"
)

// TempInstanceKey marks an instance incus-compose created to read an image with.
const TempInstanceKey = "user.incus-compose.temp"

// prefetch fills the volume with what the image holds at Config.Prefetch.
// An image has no file API, so its bytes are read from an instance of its own.
func (r *StorageVolume) prefetch(ctx context.Context) error {
	img, ok := r.Config.ImageResource.(*Image)
	if !ok {
		return ErrUnknownResource.WithResource(r.Config.ImageResource)
	}

	if !img.IsEnsured() {
		return ErrNotEnsured.WithResource(img)
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	name := "ic-seed-" + SanitizeIncusName(RandString(16), MaxIncusNameLen-8)

	op, err := conn.CreateInstance(ctx, incusApi.InstancesPost{
		Name: name,
		Type: incusApi.InstanceTypeContainer,
		Source: incusApi.InstanceSource{
			Type:        "image",
			Fingerprint: img.State().IncusAlias.Target,
		},
		InstancePut: incusApi.InstancePut{
			Description: fmt.Sprintf(r.client.Config().DescriptionFormat, name),
			Config:      map[string]string{TempInstanceKey: "true"},
			Devices: map[string]map[string]string{
				"root": {"type": "disk", "path": "/", "pool": r.Config.Pool},
			},
		},
	})
	if err != nil {
		return ErrCreate.WithText("creating an instance to read the image").Wrap(err)
	}

	_, err = iclient.WaitOperation(ctx, op)
	if err != nil {
		return ErrCreate.WithText("creating an instance to read the image").Wrap(err)
	}

	defer func() {
		r.client.WarnError(func() error {
			deleteOp, err := conn.DeleteInstance(context.WithoutCancel(ctx), name)
			if err != nil {
				return err
			}

			_, err = iclient.WaitOperation(context.WithoutCancel(ctx), deleteOp)

			return err
		}, "Failed to remove the instance a prefetch read the image from")
	}()

	// The instance stays stopped, so this is the image's own filesystem: a
	// stopped instance mounts no disk devices.
	src, err := retry.NewWithData[*sftp.Client](
		retry.Context(ctx),
		retry.Attempts(3),
		retry.Delay(2*time.Second),
		retry.LastErrorOnly(true),
	).Do(func() (*sftp.Client, error) {
		return conn.GetInstanceFileSFTP(ctx, name)
	})
	if err != nil {
		return ErrCreate.WithText("connecting to instance SFTP").Wrap(err)
	}

	defer r.client.WarnError(src.Close, "Failed to close an instance sFTP connection")

	from, err := src.Lstat(r.Config.Prefetch)
	if err != nil || !from.IsDir() {
		r.client.LogDebug("Nothing to prefetch", "resource", r, "path", r.Config.Prefetch)

		return nil
	}

	dst, err := r.SFTP(ctx)
	if err != nil {
		return err
	}

	defer r.client.WarnError(dst.Close, "Failed to close a volume sFTP connection")

	// The mount point's own ownership and mode come with it, as docker's does.
	err = sftpSetOwnerMode(dst, "/", sftpArgs(from))
	if err != nil {
		return err
	}

	progress := &prefetchProgress{volume: r}

	return r.prefetchDir(src, dst, r.Config.Prefetch, "/", progress)
}

// prefetchProgress reports what a copy has moved so far. There is no total to
// count against: the size of a path inside an image is only knowable by walking
// it, which is the copy itself.
type prefetchProgress struct {
	volume *StorageVolume
	files  int
	bytes  int64
	last   time.Time
}

// add records one copied file and reports it, at most a few times a second.
func (p *prefetchProgress) add(size int64) {
	p.files++
	p.bytes += size

	if time.Since(p.last) < 200*time.Millisecond {
		return
	}

	p.last = time.Now()

	files := "files"
	if p.files == 1 {
		files = "file"
	}

	p.volume.client.globalClient.emitProgress(ActionEnsure, p.volume, Options{}, Progress{
		Percent: -1,
		Text: fmt.Sprintf("Prefetching %s: %d %s, %s", p.volume.Config.Prefetch, p.files, files,
			units.GetByteSizeString(p.bytes, 2)),
	})
}

// prefetchDir copies one directory's contents between two SFTP endpoints.
func (r *StorageVolume) prefetchDir(src *sftp.Client, dst *sftp.Client, from string, to string, progress *prefetchProgress) error {
	entries, err := src.ReadDir(from)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		source := path.Join(from, entry.Name())
		target := path.Join(to, entry.Name())

		// ReadDir follows symlinks, so ask again without following.
		info, err := src.Lstat(source)
		if err != nil {
			return err
		}

		if info.IsDir() {
			err = sftpCreateFile(r.client, dst, target, sftpArgs(info), false)
			if err != nil {
				return err
			}

			err = r.prefetchDir(src, dst, source, target, progress)
			if err != nil {
				return err
			}

			continue
		}

		if !info.Mode().IsRegular() {
			r.client.LogWarn("Not prefetching what is neither a file nor a directory",
				"resource", r, "path", source, "mode", info.Mode())

			continue
		}

		err = r.prefetchFile(src, dst, source, target, info)
		if err != nil {
			return err
		}

		progress.add(info.Size())
	}

	return nil
}

// prefetchFile streams one regular file between two SFTP endpoints.
func (r *StorageVolume) prefetchFile(src *sftp.Client, dst *sftp.Client, from string, to string, info fs.FileInfo) error {
	f, err := src.Open(from)
	if err != nil {
		return err
	}

	defer r.client.WarnError(f.Close, "Failed to close a prefetched file")

	args := sftpArgs(info)
	args.Content = f

	return sftpCreateFile(r.client, dst, to, args, true)
}

// sftpArgs carries a source entry's mode and owner over to the copy.
func sftpArgs(info fs.FileInfo) instanceFileArgs {
	args := instanceFileArgs{
		Type: "file",
		Mode: int(info.Mode().Perm()),
		UID:  -1,
		GID:  -1,
	}

	if info.IsDir() {
		args.Type = "directory"
	}

	stat, ok := info.Sys().(*sftp.FileStat)
	if ok {
		args.UID, args.GID = int64(stat.UID), int64(stat.GID)
	}

	return args
}
