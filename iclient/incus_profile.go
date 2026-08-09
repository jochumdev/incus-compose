package iclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lxc/incus/v7/shared/api"
)

// incusProfilesPath is the collection every profile call hangs off.
const incusProfilesPath = "/profiles"

// GetProfile returns one profile and its ETag.
func (c *Connection) GetProfile(ctx context.Context, name string) (*api.Profile, string, error) {
	profile := api.Profile{}

	etag, err := c.getStruct(ctx, incusProfilesPath+"/"+url.PathEscape(name), nil, &profile)
	if err != nil {
		return nil, "", err
	}

	return &profile, etag, nil
}

// CreateProfile adds a profile.
func (c *Connection) CreateProfile(ctx context.Context, profile api.ProfilesPost) error {
	_, _, err := c.do(ctx, http.MethodPost, incusProfilesPath, nil, profile, "")

	return err
}

// UpdateProfile replaces a profile's configuration.
func (c *Connection) UpdateProfile(ctx context.Context, name string, profile api.ProfilePut, etag string) error {
	_, _, err := c.do(ctx, http.MethodPut, incusProfilesPath+"/"+url.PathEscape(name), nil, profile, etag)

	return err
}

// DeleteProfile removes a profile.
func (c *Connection) DeleteProfile(ctx context.Context, name string) error {
	_, _, err := c.do(ctx, http.MethodDelete, incusProfilesPath+"/"+url.PathEscape(name), nil, nil, "")

	return err
}
