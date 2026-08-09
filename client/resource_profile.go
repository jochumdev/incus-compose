package client

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	incusApi "github.com/lxc/incus/v7/shared/api"

	"github.com/lxc/incus-compose/iclient"
)

// ProfileConfig configures profile creation from a source profile.
type ProfileConfig struct {
	// SourceServer is the Incus server to copy the profile from.
	// If nil, uses the global Incus client.
	SourceServer *iclient.Connection

	// SourceProject is the project containing the source profile.
	SourceProject string

	// SourceProfile is the name of the profile to copy from.
	SourceProfile string

	// NetworkOnly copies only NIC devices from the source profile.
	NetworkOnly bool
}

// ProfileState is what the last fetch read back from Incus.
type ProfileState struct {
	// IncusProfile is nil until the profile is ensured.
	IncusProfile *incusApi.Profile
	ETag         string
}

// Profile represents an Incus profile resource.
type Profile struct {
	*BaseResource

	incusName string
	created   bool

	// mu serializes the actions; two workers may share one resource object.
	// Nothing the actions call may take it again - it is not reentrant.
	mu sync.Mutex

	client *Client
	Config ProfileConfig

	// state is swapped whole, so a reader never sees a half-updated profile.
	state atomic.Pointer[ProfileState]
}

// GetConfig returns the configuration.
func (c *ProfileConfig) GetConfig() any {
	return c
}

// newProfile returns an existing Profile or creates a new one.
// If a profile with the same name exists, it is returned.
func newProfile(c *Client, name string, configGetter Config) (*Profile, error) {
	if configGetter == nil {
		return nil, ErrUnknownConfig.WithKindName(KindProfile, name)
	}

	var config *ProfileConfig
	cConfig, ok := configGetter.GetConfig().(*ProfileConfig)
	if !ok {
		return nil, ErrUnknownConfig.WithKindName(KindProfile, name)
	}
	config = cConfig

	profile := &Profile{
		BaseResource: NewBaseResource(KindProfile, name, PriorityProfile),
		incusName:    SanitizeProjectName(name),
		client:       c,
		Config:       *config,
	}

	// Every accessor dereferences this, so it must never be nil.
	profile.state.Store(&ProfileState{})

	return profile, nil
}

// String is for debugging.
func (r *Profile) String() string {
	return fmt.Sprintf("%v(%v)", r.kind, r.incusName)
}

// IncusName returns the sanitized profile name used in Incus.
func (r *Profile) IncusName() string {
	return r.incusName
}

// IsEnsured returns true if the profile state has been fetched from Incus.
func (r *Profile) IsEnsured() bool {
	return r.State().IncusProfile != nil
}

// State returns the profile state as of the last fetch. It is replaced whole,
// never written into, so the result stays consistent for as long as it is held.
func (r *Profile) State() *ProfileState {
	return r.state.Load()
}

// clearState forgets the fetched profile.
func (r *Profile) clearState() {
	r.state.Store(&ProfileState{})
}

// Created returns true if the profile was created during the last Ensure call.
func (r *Profile) Created() bool {
	return r.created
}

// Ensure retrieves an existing resource or creates a new one if args.Create is true.
func (r *Profile) Ensure(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	options := NewOptions(opts...)

	if err := r.client.hookBefore(ctx, ActionEnsure, r, options, nil); err != nil {
		return err
	}

	_, err := r.client.Connection()
	if err != nil {
		return r.client.hookAfter(ctx, ActionEnsure, r, options, err)
	}

	// Try to get existing
	err = r.get(ctx)
	if err == nil {
		if r.Config.SourceProfile != "" {
			err = r.updateFromSource(ctx)
		}
		err = r.client.hookAfter(ctx, ActionEnsure, r, options, err)

		return err
	}

	if !options.Create || !errors.Is(err, ErrNotFound) {
		err = r.client.hookAfter(ctx, ActionEnsure, r, options, err)

		return err
	}

	err = r.create(ctx)

	// A concurrent creator may have won the race; adopt whatever is there.
	if err != nil && r.get(ctx) == nil {
		err = nil
	}

	err = r.client.hookAfter(ctx, ActionEnsure, r, options, err)

	return err
}

func (r *Profile) get(ctx context.Context) error {
	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	profile, eTag, err := conn.GetProfile(ctx, r.incusName)
	if err != nil {
		r.clearState()
		return ErrNotFound.Wrap(err)
	}

	r.state.Store(&ProfileState{IncusProfile: profile, ETag: eTag})

	return nil
}

func (r *Profile) create(ctx context.Context) error {
	var postArgs incusApi.ProfilesPost
	if r.Config.SourceProfile != "" {
		sourceProfile, err := r.sourceProfile(ctx)
		if err != nil {
			return err
		}

		profilePut := r.profilePutFromSource(sourceProfile)
		profilePut.Description = fmt.Sprintf(r.client.Config().DescriptionFormat, r.Name())
		postArgs = incusApi.ProfilesPost{
			Name:       r.incusName,
			ProfilePut: profilePut,
		}
	} else {
		postArgs = incusApi.ProfilesPost{
			Name: r.incusName,
			ProfilePut: incusApi.ProfilePut{
				Description: fmt.Sprintf(r.client.Config().DescriptionFormat, r.Name()),
			},
		}
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	if err := conn.CreateProfile(ctx, postArgs); err != nil {
		return fmt.Errorf("creating profile %s: %w", r.Name(), err)
	}

	profile, eTag, err := conn.GetProfile(ctx, r.incusName)
	if err != nil {
		return fmt.Errorf("fetching created profile %s: %w", r.Name(), err)
	}

	r.state.Store(&ProfileState{IncusProfile: profile, ETag: eTag})
	r.created = true

	return nil
}

func (r *Profile) sourceProfile(ctx context.Context) (*incusApi.Profile, error) {
	sourceServer := r.Config.SourceServer
	if sourceServer == nil {
		gConn, err := r.client.GlobalConnection()
		if err != nil {
			return nil, err
		}
		sourceServer = gConn
	}

	if r.Config.SourceProject != "" {
		sourceServer = sourceServer.WithProject(r.Config.SourceProject)
	}

	sourceProfile, _, err := sourceServer.GetProfile(ctx, r.Config.SourceProfile)
	if err != nil {
		return nil, fmt.Errorf("getting source profile %s:%s: %w", r.Config.SourceProject, r.Config.SourceProfile, err)
	}

	return sourceProfile, nil
}

func (r *Profile) profilePutFromSource(sourceProfile *incusApi.Profile) incusApi.ProfilePut {
	if !r.Config.NetworkOnly {
		return incusApi.ProfilePut{
			Config:      sourceProfile.Config,
			Devices:     sourceProfile.Devices,
			Description: fmt.Sprintf(r.client.Config().DescriptionFormat, r.Name()),
		}
	}

	devices := map[string]map[string]string{}
	for name, device := range sourceProfile.Devices {
		if device["type"] == "nic" {
			devices[name] = maps.Clone(device)
		}
	}

	return incusApi.ProfilePut{Devices: devices}
}

func (r *Profile) updateFromSource(ctx context.Context) error {
	sourceProfile, err := r.sourceProfile(ctx)
	if err != nil {
		return err
	}

	state := r.State()

	profilePut := r.profilePutFromSource(sourceProfile)
	if r.Config.NetworkOnly {
		profilePut.Config = maps.Clone(state.IncusProfile.Config)
		profilePut.Description = state.IncusProfile.Description
		for name, device := range state.IncusProfile.Devices {
			if device["type"] != "nic" {
				profilePut.Devices[name] = maps.Clone(device)
			}
		}
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	if err := conn.UpdateProfile(ctx, r.incusName, profilePut, state.ETag); err != nil {
		return fmt.Errorf("updating profile %s from source %s:%s: %w", r.Name(), r.Config.SourceProject, r.Config.SourceProfile, err)
	}

	return r.get(ctx)
}

// HasDevice returns true if the profile has a device with the given name.
func (r *Profile) HasDevice(name string) bool {
	if !r.IsEnsured() {
		return false
	}

	profile := r.State().IncusProfile
	if len(profile.Devices) > 0 {
		for devName := range profile.Devices {
			if devName == name {
				return true
			}
		}
	}

	return false
}

// Delete removes the profile from Incus.
func (r *Profile) Delete(ctx context.Context, opts ...Option) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.IsEnsured() {
		r.client.resources.Remove(r)
		return nil // Nothing to delete
	}

	if err := r.get(ctx); err != nil {
		// Already gone server side
		r.client.resources.Remove(r)
		return err
	}

	options := NewOptions(opts...)

	if err := r.client.hookBefore(ctx, ActionDelete, r, options, nil); err != nil {
		r.clearState()

		r.client.resources.Remove(r)
		return err
	}

	conn, err := r.client.Connection()
	if err != nil {
		return err
	}

	// Do the actual work
	err = conn.DeleteProfile(ctx, r.incusName)
	err = r.client.hookAfter(ctx, ActionDelete, r, options, err)

	r.clearState()
	r.client.resources.Remove(r)

	return err
}

var (
	_ Resource   = (*Profile)(nil)
	_ EnsureAble = (*Profile)(nil)
	_ DeleteAble = (*Profile)(nil)
)
