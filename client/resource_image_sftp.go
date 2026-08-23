package client

import (
	"context"
	"fmt"
	"time"

	"github.com/avast/retry-go/v5"
	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/pkg/sftp"

	"github.com/lxc/incus-compose/iclient"
)

// TempInstanceKey marks an instance incus-compose created to read an image with.
const TempInstanceKey = "user.incus-compose.temp"

// SFTP returns a connection to the image's own filesystem, opened once and
// closed by Done.
func (r *Image) SFTP(ctx context.Context) (*sftp.Client, error) {
	if !r.IsEnsured() {
		return nil, ErrNotEnsured.WithResource(r)
	}

	r.sftpMu.Lock()
	defer r.sftpMu.Unlock()

	if r.sftpConn != nil {
		return r.sftpConn, nil
	}

	name, conn, err := r.createReader(ctx)
	r.sftpName, r.sftpConn = name, conn

	return conn, err
}

// Done closes the image's SFTP connection and removes the instance behind it.
func (r *Image) Done() error {
	r.sftpMu.Lock()
	defer r.sftpMu.Unlock()

	if r.sftpName == "" {
		return nil
	}

	name := r.sftpName

	r.client.WarnError(r.sftpConn.Close, "Failed to close an image sFTP connection")

	r.sftpName = ""
	r.sftpConn = nil

	return r.deleteReader(name)
}

// deleteReader removes an instance the image was read through.
func (r *Image) deleteReader(name string) error {
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	// The run may already be canceled, and the instance still has to go.
	ctx := context.WithoutCancel(r.client.ctx)

	op, err := conn.DeleteInstance(ctx, r.client.incusProject, name)
	if err != nil {
		return err
	}

	_, err = iclient.WaitOperation(ctx, op)

	return err
}

// createReader starts a stopped instance from the image and connects to it. A
// stopped instance mounts no disk devices, so this is the image's own rootfs.
func (r *Image) createReader(ctx context.Context) (string, *sftp.Client, error) {
	conn, err := r.client.Connection()
	if err != nil {
		return "", nil, err
	}

	name := "ic-seed-" + SanitizeIncusName(RandString(16), MaxIncusNameLen-8)

	op, err := conn.CreateInstance(ctx, r.client.incusProject, incusApi.InstancesPost{
		Name: name,
		Type: incusApi.InstanceTypeContainer,
		Source: incusApi.InstanceSource{
			Type:        "image",
			Fingerprint: r.State().IncusAlias.Target,
		},
		InstancePut: incusApi.InstancePut{
			Description: fmt.Sprintf(r.client.Config().DescriptionFormat, name),
			Config:      map[string]string{TempInstanceKey: "true"},
			Devices: map[string]map[string]string{
				"root": {"type": "disk", "path": "/", "pool": r.client.Config().DefaultStoragePool},
			},
		},
	})
	if err == nil {
		_, err = iclient.WaitOperation(ctx, op)
	}

	if err != nil {
		return "", nil, ErrCreate.WithText("creating an instance to read the image").Wrap(err)
	}

	sc, err := retry.NewWithData[*sftp.Client](
		retry.Context(ctx),
		retry.Attempts(3),
		retry.Delay(2*time.Second),
		retry.LastErrorOnly(true),
	).Do(func() (*sftp.Client, error) {
		return conn.GetInstanceFileSFTP(ctx, r.client.incusProject, name)
	})
	if err != nil {
		r.client.WarnError(func() error { return r.deleteReader(name) },
			"Failed to remove the instance an image was read through")

		return "", nil, ErrCreate.WithText("connecting to instance SFTP").Wrap(err)
	}

	return name, sc, nil
}
