package iclient

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

// waitOperation drains an operation channel and returns its outcome, which is
// the last value: the channel closes on a terminal state.
func waitOperation(t *testing.T, updates <-chan api.Operation) api.Operation {
	t.Helper()

	last := api.Operation{}

	for update := range updates {
		last = update
	}

	require.True(t, last.StatusCode.IsFinal(), "the channel closed before a terminal state: %+v", last)
	require.Empty(t, last.Err, "operation failed")

	return last
}

// testProject creates a throwaway project and removes it on cleanup.
func testProject(t *testing.T, conn *Connection, prefix string) string {
	t.Helper()

	name := fmt.Sprintf("%s-%d", prefix, rand.Uint32())

	err := conn.CreateProject(t.Context(), api.ProjectsPost{
		Name: name,
		ProjectPut: api.ProjectPut{
			Config: map[string]string{"features.images": "true"},
		},
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		// t.Context is already canceled by the time cleanup runs.
		_ = conn.DeleteProject(context.Background(), name, &DeleteProjectArgs{Force: true})
	})

	return name
}

// TestIncusImagePullAndCopy is the flow the image resource performs: pull an
// OCI image from a registry into one project, then copy it into another.
// Neither hop touches a registry from here; incusd does the fetching.
func TestIncusImagePullAndCopy(t *testing.T) {
	skipE2E(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	cache := testProject(t, conn, "iclient-cache")
	target := testProject(t, conn, "iclient-target")

	cacheConn := conn.WithProject(cache)
	targetConn := conn.WithProject(target)

	// The remote comes from the Incus configuration, so this follows whatever
	// mirror docker.io is pointed at rather than reaching the registry direct.
	config, err := ReadConfig("")
	require.NoError(t, err)

	registry, err := config.RemoteInfos("docker.io")
	require.NoError(t, err)
	require.Equal(t, "oci", registry.Protocol, "docker.io is not an OCI remote here")

	// Hop A: registry to the cache project. The alias is fully qualified, a
	// registry having no notion of Docker Hub's implicit library/ prefix.
	pullCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	updates, err := cacheConn.CreateImage(pullCtx, api.ImagesPost{
		Source: &api.ImagesPostSource{
			ImageSource: api.ImageSource{
				Alias:       "library/alpine:latest",
				Server:      registry.Addrs[0],
				Protocol:    registry.Protocol,
				Certificate: registry.ServerCert,
			},
			Type: "image",
			Mode: "pull",
		},
	}, nil)
	require.NoError(t, err)

	pulled := waitOperation(t, updates)

	fingerprint := operationFingerprint(t, pulled)
	require.NotEmpty(t, fingerprint)

	image, _, err := cacheConn.GetImage(ctx, fingerprint, nil)
	require.NoError(t, err)
	require.Equal(t, fingerprint, image.Fingerprint)

	// Hop B: cache project to the target project. CopyImage owns the secret a
	// private image needs.
	updates, err = targetConn.CopyImage(ctx, cacheConn, fingerprint, nil)
	require.NoError(t, err)

	waitOperation(t, updates)

	copied, _, err := targetConn.GetImage(ctx, fingerprint, nil)
	require.NoError(t, err)
	require.Equal(t, fingerprint, copied.Fingerprint, "the copy keeps the fingerprint")

	// The cache still holds it: a copy is not a move.
	_, _, err = cacheConn.GetImage(ctx, fingerprint, nil)
	require.NoError(t, err)
}

// operationFingerprint returns the image an operation created. Incus reports
// it in the operation metadata, not in Resources, which stays empty here.
func operationFingerprint(t *testing.T, op api.Operation) string {
	t.Helper()

	fingerprint, ok := op.Metadata["fingerprint"].(string)
	require.True(t, ok, "no fingerprint in the operation metadata: %+v", op.Metadata)

	return fingerprint
}

// tarOf builds a tarball holding the given files.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	archive := tar.NewWriter(buf)

	for name, content := range files {
		require.NoError(t, archive.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}))

		_, err := io.WriteString(archive, content)
		require.NoError(t, err)
	}

	require.NoError(t, archive.Close())

	return buf.Bytes()
}

// TestIncusWriteImageParts pins the multipart field names. The server tells a
// container rootfs from a disk image by them, so they are not cosmetic.
func TestIncusWriteImageParts(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		kind string
		want string
	}{
		{"", "rootfs"},
		{"container", "rootfs"},
		{"virtual-machine", "rootfs.img"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()

			body := &bytes.Buffer{}
			form := multipart.NewWriter(body)

			require.NoError(t, writeImageParts(form, &ImageCreateArgs{
				MetaFile:   strings.NewReader("meta"),
				MetaName:   "metadata.tar",
				RootfsFile: strings.NewReader("root"),
				RootfsName: "rootfs.tar",
				Type:       tt.kind,
			}))

			parts := map[string]string{}

			reader := multipart.NewReader(body, form.Boundary())

			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}

				content, err := io.ReadAll(part)
				require.NoError(t, err)

				parts[part.FormName()] = string(content)
			}

			require.Equal(t, map[string]string{"metadata": "meta", tt.want: "root"}, parts)
		})
	}
}

// TestIncusImageUploadHeader pins how an upload describes itself; anything left
// out of these headers is silently dropped.
func TestIncusImageUploadHeader(t *testing.T) {
	t.Parallel()

	header := imageUploadHeader(api.ImagesPost{
		Aliases:  []api.ImageAlias{{Name: "one"}, {Name: "two"}},
		Filename: "built.tar",
		ImagePut: api.ImagePut{
			Public:     true,
			Profiles:   []string{"default", "extra"},
			Properties: map[string]string{"oci.uid": "1000"},
		},
	})

	require.Equal(t, "alias=one&alias=two", header.Get("X-Incus-aliases"))
	require.Equal(t, "profile=default&profile=extra", header.Get("X-Incus-profiles"))
	require.Equal(t, "oci.uid=1000", header.Get("X-Incus-properties"))
	require.Equal(t, "true", header.Get("X-Incus-public"))
	require.Equal(t, "built.tar", header.Get("X-Incus-filename"))
}

// TestIncusImageUploadHeaderEmpty: a header the server would read as a value
// must not be sent when there is no value.
func TestIncusImageUploadHeaderEmpty(t *testing.T) {
	t.Parallel()

	require.Empty(t, imageUploadHeader(api.ImagesPost{}))
}

// TestIncusUploadHeaderReachesTheWire is the other half: the header the upload
// builds has to survive the request, not just be assembled correctly.
func TestIncusUploadHeaderReachesTheWire(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{}`)

	_, _, err := conn.send(t.Context(), http.MethodPost, "/1.0/images",
		strings.NewReader("tarball"), "application/octet-stream", "",
		imageUploadHeader(api.ImagesPost{Aliases: []api.ImageAlias{{Name: "built"}}}))
	require.NoError(t, err)

	sent := seen.all()
	require.Len(t, sent, 1)
	require.Equal(t, "alias=built", sent[0].header.Get("X-Incus-aliases"))
	require.Equal(t, "application/octet-stream", sent[0].header.Get("Content-Type"))
	require.Equal(t, "tarball", sent[0].body)
}

func TestIncusCreateImageRefusesWithoutMetadata(t *testing.T) {
	t.Parallel()

	conn, seen := recordingServer(t, `{}`)

	_, err := conn.CreateImage(t.Context(), api.ImagesPost{}, &ImageCreateArgs{
		RootfsFile: strings.NewReader("root"),
	})
	require.Error(t, err)
	require.Empty(t, seen.all(), "it must refuse before asking the server")
}

// TestIncusCreateImageUploadAgainstRealIncus imports a split image the way the
// compose `build:` path does, from tarballs rather than from a remote.
func TestIncusCreateImageUploadAgainstRealIncus(t *testing.T) {
	skipLocal(t)
	t.Parallel()

	ctx := t.Context()
	conn := testConnection(t)

	project := testProject(t, conn, "iclient-upload")
	projectConn := conn.WithProject(project)

	server, _, err := conn.GetServer(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, server.Environment.Architectures)

	metadata, err := json.Marshal(api.ImageMetadata{
		Architecture: server.Environment.Architectures[0],
		CreationDate: time.Now().Unix(),
		Properties:   map[string]string{"description": "iclient upload test"},
	})
	require.NoError(t, err)

	const alias = "iclient-uploaded"

	updates, err := projectConn.CreateImage(ctx, api.ImagesPost{
		Aliases:  []api.ImageAlias{{Name: alias}},
		ImagePut: api.ImagePut{Public: true, Properties: map[string]string{"iclient.test": "yes"}},
	}, &ImageCreateArgs{
		MetaFile:   bytes.NewReader(tarOf(t, map[string]string{"metadata.yaml": string(metadata)})),
		MetaName:   "metadata.tar",
		RootfsFile: bytes.NewReader(tarOf(t, map[string]string{"etc/hostname": "iclient\n"})),
		RootfsName: "rootfs.tar",
	})
	require.NoError(t, err)

	imported := waitOperation(t, updates)

	fingerprint := operationFingerprint(t, imported)
	require.NotEmpty(t, fingerprint)

	// Only a lookup by alias notices an upload whose headers went missing.
	resolved, _, err := projectConn.GetImageAlias(ctx, alias, nil)
	require.NoError(t, err, "the upload did not carry its aliases")
	require.Equal(t, fingerprint, resolved.Target)

	image, etag, err := projectConn.GetImage(ctx, fingerprint, nil)
	require.NoError(t, err)
	require.Equal(t, "iclient upload test", image.Properties["description"],
		"the server reads the metadata out of the uploaded tarball")
	require.Equal(t, "yes", image.Properties["iclient.test"], "the upload did not carry its properties")
	require.True(t, image.Public, "the upload did not carry its public flag")

	// Proves the image is usable and not merely accepted: a conditional update
	// needs the ETag the upload's own read handed back.
	require.NoError(t, projectConn.UpdateImage(ctx, fingerprint, api.ImagePut{
		Properties: image.Properties,
		Public:     false,
	}, etag))

	updates, err = projectConn.DeleteImage(ctx, fingerprint)
	require.NoError(t, err)

	waitOperation(t, updates)

	_, _, err = projectConn.GetImage(ctx, fingerprint, nil)
	require.Error(t, err)
}
