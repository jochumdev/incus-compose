package iclient

import (
	"io"

	"github.com/lxc/incus/v7/shared/api"
)

// ConnectionInfo describes how a Connection reaches its server.
type ConnectionInfo struct {
	// Addresses the server says it can be reached on, plus the one dialed.
	Addresses []string

	// Certificate is the server certificate pinned for this remote, if any.
	Certificate string

	Protocol string
	URL      string

	// SocketPath is empty for anything but a unix remote.
	SocketPath string

	Project string
	Target  string
}

// ImageCreateArgs uploads an image body directly, rather than having the
// server fetch it from somewhere.
//
// A nil RootfsFile means MetaFile is a unified image carrying both.
type ImageCreateArgs struct {
	MetaFile io.Reader
	MetaName string

	RootfsFile io.Reader
	RootfsName string

	// Type is the image type, container or virtual-machine.
	Type string
}

// InstanceConsoleArgs is where a console attach sends what it reads.
type InstanceConsoleArgs struct {
	// Output receives the console stream. Input is never forwarded: this
	// attaches to watch a console, not to drive one.
	Output io.Writer
}

// ImageCopyArgs narrows a copy between two connections.
type ImageCopyArgs struct {
	// Aliases to add to the copy.
	Aliases []api.ImageAlias

	// AutoUpdate has the server keep the copy in step with its source.
	AutoUpdate bool

	// Public makes the copy readable without a token.
	Public bool

	// Type is the image type to resolve to.
	Type string

	// Mode is the transfer direction: pull (the default), push or relay.
	Mode string

	// Profiles to apply on the target.
	Profiles []string
}

// GetImageArgs narrows an image read. A nil one is the zero value.
type GetImageArgs struct {
	// Secret reads an image the server has not made public, using a token
	// minted from its /secret endpoint.
	Secret string
}

// GetImageAliasArgs narrows an alias read. A nil one is the zero value.
type GetImageAliasArgs struct {
	// Type picks between the container and the virtual-machine image behind
	// one alias. Empty takes whichever the server returns first.
	Type string
}

// GetStoragePoolVolumeArgs narrows a volume read. A nil one is the zero value.
type GetStoragePoolVolumeArgs struct {
	// Full also fetches snapshots and backups. Without it those fields of
	// the returned StorageVolumeFull are zero.
	Full bool
}

// DeleteProjectArgs widens a project delete. A nil one is the zero value.
type DeleteProjectArgs struct {
	// Force deletes the instances, volumes and images in the project too.
	// Without it Incus refuses to remove a project that holds anything.
	Force bool
}

// GetInstanceArgs narrows a single instance read. A nil one is the zero value.
type GetInstanceArgs struct {
	// Full also fetches state, snapshots and backups. Without it those
	// fields of the returned InstanceFull are zero.
	Full bool
}

// GetInstancesArgs narrows a listing. A nil one, like the zero value, lists
// the connection's own project.
type GetInstancesArgs struct {
	// Type limits the listing to containers or to virtual machines.
	Type api.InstanceType

	// Full also fetches state, snapshots and backups.
	Full bool

	// AllProjects lists every project the certificate may see. Instance
	// names are not unique across projects, so GetInstanceNames refuses it.
	AllProjects bool

	// Filters are server-side selectors written as "key=value", e.g.
	// "status=Running". They need the api_filtering extension.
	Filters []string
}
