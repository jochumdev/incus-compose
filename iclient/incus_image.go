package iclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusImagesPath is the collection every image call hangs off.
const incusImagesPath = "/images"

// GetImage returns one image and its ETag.
func (c *Connection) GetImage(ctx context.Context, fingerprint string, args *GetImageArgs) (*api.Image, string, error) {
	if args == nil {
		args = &GetImageArgs{}
	}

	var query url.Values

	if args.Secret != "" {
		query = url.Values{}
		query.Set("secret", args.Secret)
	}

	image := api.Image{}

	etag, err := c.getStruct(ctx, incusImagesPath+"/"+url.PathEscape(fingerprint), query, &image)
	if err != nil {
		return nil, "", err
	}

	return &image, etag, nil
}

// GetImageAlias resolves an alias to the image behind it.
func (c *Connection) GetImageAlias(ctx context.Context, name string, args *GetImageAliasArgs) (*api.ImageAliasesEntry, string, error) {
	if args == nil {
		args = &GetImageAliasArgs{}
	}

	path := incusImagesPath + "/aliases/" + url.PathEscape(name)
	if args.Type != "" {
		path = incusImagesPath + "/aliases/" + url.PathEscape(args.Type) + "/" + url.PathEscape(name)
	}

	alias := api.ImageAliasesEntry{}

	etag, err := c.getStruct(ctx, path, nil, &alias)
	if err != nil {
		return nil, "", err
	}

	return &alias, etag, nil
}

// CreateImage adds an image, and is also how one is copied in: give
// image.Source a server, a protocol and an alias and incusd fetches it.
//
// With args the tarballs are uploaded instead, and image carries only the
// aliases and properties to record.
func (c *Connection) CreateImage(ctx context.Context, image api.ImagesPost, args *ImageCreateArgs) (<-chan api.Operation, error) {
	if args == nil {
		return c.asyncOperation(ctx, http.MethodPost, incusImagesPath, image, "")
	}

	if args.MetaFile == nil {
		return nil, errors.New("uploading an image: the metadata file is required")
	}

	header := imageUploadHeader(image)

	// A unified image is one tarball; a split one needs the two parts named.
	if args.RootfsFile == nil {
		return c.asyncUpload(ctx, incusImagesPath, args.MetaFile, "application/octet-stream", header)
	}

	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)

	go func() {
		// A read error aborts the request rather than uploading a truncated image.
		_ = writer.CloseWithError(writeImageParts(form, args))
	}()

	return c.asyncUpload(ctx, incusImagesPath, reader, form.FormDataContentType(), header)
}

// imageUploadHeader carries what an upload cannot put in the body, the body
// being the tarballs. Leaving it out silently drops the image's aliases.
func imageUploadHeader(image api.ImagesPost) http.Header {
	header := http.Header{}

	if image.Public {
		header.Set("X-Incus-public", "true")
	}

	if image.Filename != "" {
		header.Set("X-Incus-filename", image.Filename)
	}

	if len(image.Properties) > 0 {
		properties := url.Values{}
		for key, value := range image.Properties {
			properties.Set(key, value)
		}

		header.Set("X-Incus-properties", properties.Encode())
	}

	if len(image.Profiles) > 0 {
		profiles := url.Values{}
		for _, profile := range image.Profiles {
			profiles.Add("profile", profile)
		}

		header.Set("X-Incus-profiles", profiles.Encode())
	}

	if len(image.Aliases) > 0 {
		aliases := url.Values{}
		for _, alias := range image.Aliases {
			aliases.Add("alias", alias.Name)
		}

		header.Set("X-Incus-aliases", aliases.Encode())
	}

	return header
}

// writeImageParts streams the metadata and rootfs tarballs into the form.
func writeImageParts(form *multipart.Writer, args *ImageCreateArgs) error {
	part, err := form.CreateFormFile("metadata", args.MetaName)
	if err != nil {
		return err
	}

	_, err = io.Copy(part, args.MetaFile)
	if err != nil {
		return err
	}

	// The field name is how the server tells a container rootfs from a disk image.
	field := "rootfs"
	if args.Type == "virtual-machine" {
		field = "rootfs.img"
	}

	part, err = form.CreateFormFile(field, args.RootfsName)
	if err != nil {
		return err
	}

	_, err = io.Copy(part, args.RootfsFile)
	if err != nil {
		return err
	}

	return form.Close()
}

// UpdateImage replaces an image's configuration.
func (c *Connection) UpdateImage(ctx context.Context, fingerprint string, image api.ImagePut, etag string) error {
	_, _, err := c.do(ctx, http.MethodPut, incusImagesPath+"/"+url.PathEscape(fingerprint), nil, image, etag)

	return err
}

// DeleteImage removes an image and follows the operation.
func (c *Connection) DeleteImage(ctx context.Context, fingerprint string) (<-chan api.Operation, error) {
	return c.asyncOperation(ctx, http.MethodDelete, incusImagesPath+"/"+url.PathEscape(fingerprint), nil, "")
}

// CreateImageSecret mints a one-time token for fetching a non-public image.
//
// This is a token operation: it never reaches a terminal state. Read the first
// value, whose Metadata carries the secret, and cancel the context.
func (c *Connection) CreateImageSecret(ctx context.Context, fingerprint string) (<-chan api.Operation, error) {
	path := incusImagesPath + "/" + url.PathEscape(fingerprint) + "/secret"

	return c.asyncOperation(ctx, http.MethodPost, path, nil, "")
}

// CopyImage copies an image from another incus connection into this one.
func (c *Connection) CopyImage(ctx context.Context, source *Connection, fingerprint string, args *ImageCopyArgs) (<-chan api.Operation, error) {
	if args == nil {
		args = &ImageCopyArgs{}
	}

	image, _, err := source.GetImage(ctx, fingerprint, nil)
	if err != nil {
		return nil, fmt.Errorf("reading the source image %q: %w", fingerprint, err)
	}

	info, err := source.GetConnectionInfo(ctx)
	if err != nil {
		return nil, err
	}

	if len(info.Addresses) == 0 {
		// incusd dials the source over the network even when it is itself.
		return nil, fmt.Errorf("copying %q: %w", fingerprint, ErrConnectionNoAddress)
	}

	mode := args.Mode
	if mode == "" {
		mode = "pull"
	}

	post := api.ImagesPost{
		Aliases: args.Aliases,
		ImagePut: api.ImagePut{
			Public:     args.Public,
			AutoUpdate: args.AutoUpdate,
			Profiles:   args.Profiles,
		},
		Source: &api.ImagesPostSource{
			ImageSource: api.ImageSource{
				Server:      info.Addresses[0],
				Protocol:    "incus",
				Certificate: info.Certificate,
				ImageType:   args.Type,
			},
			Type:        "image",
			Mode:        mode,
			Fingerprint: fingerprint,
			Project:     info.Project,
		},
	}

	// A private image is only fetchable with a token.
	if !image.Public {
		// Its own context, since a token operation never terminates.
		secretCtx, release := context.WithCancel(ctx)
		defer release()

		updates, err := source.CreateImageSecret(secretCtx, fingerprint)
		if err != nil {
			return nil, err
		}

		// The first value carries the token; ranging on would wait for it to expire.
		op, ok := <-updates
		if !ok {
			return nil, fmt.Errorf("minting a secret for %q: the operation reported nothing", fingerprint)
		}

		secret, ok := op.Metadata["secret"].(string)
		if !ok {
			return nil, fmt.Errorf("minting a secret for %q: no secret in the operation metadata", fingerprint)
		}

		post.Source.Secret = secret
	}

	return c.CreateImage(ctx, post, nil)
}
