package client

import (
	"context"
	"io"
	"strconv"
	"strings"
)

// OCIUserKey holds an image's own USER when it names a user instead of
// numbering one, which only the image's /etc/passwd can resolve.
const OCIUserKey = "user.incus-compose.oci.user"

// ResolveUser maps a `user[:group]` value to numeric ids, reading the image's
// /etc/passwd and /etc/group when either side is a name.
func (r *Image) ResolveUser(ctx context.Context, user string) (uint64, uint64, error) {
	if user == "" {
		return 0, 0, nil
	}

	name, group, _ := strings.Cut(user, ":")

	uid, uidErr := strconv.ParseUint(name, 10, 32)
	gid, gidErr := strconv.ParseUint(group, 10, 32)

	if uidErr == nil && (group == "" || gidErr == nil) {
		return uid, gid, nil
	}

	if r.NativeIncus() {
		return 0, 0, ErrNoSuchUser.WithText("a native Incus image has no OCI user").WithResource(r)
	}

	if uidErr != nil {
		// Without a group docker takes the user's own, as login would.
		found, primary, err := r.imageIDs(ctx, "/etc/passwd", name)
		if err != nil {
			return 0, 0, err
		}

		uid, gid = found, primary
	}

	if group != "" && gidErr != nil {
		found, _, err := r.imageIDs(ctx, "/etc/group", group)
		if err != nil {
			return 0, 0, err
		}

		gid = found
	}

	return uid, gid, nil
}

// imageIDs looks name up in one of the image's id files.
func (r *Image) imageIDs(ctx context.Context, path string, name string) (uint64, uint64, error) {
	sc, err := r.SFTP(ctx)
	if err != nil {
		return 0, 0, err
	}

	f, err := sc.Open(path)
	if err != nil {
		return 0, 0, ErrNoSuchUser.WithText("the image has no " + path).WithResource(r).Wrap(err)
	}

	defer r.client.WarnError(f.Close, "Failed to close "+path+" of an image")

	body, err := io.ReadAll(f)
	if err != nil {
		return 0, 0, ErrNoSuchUser.WithText("reading " + path + " of the image").WithResource(r).Wrap(err)
	}

	id, secondary, ok := idFields(string(body), name)
	if !ok {
		return 0, 0, ErrNoSuchUser.WithText(name + " in " + path).WithResource(r)
	}

	return id, secondary, nil
}

// idFields returns the ids on name's line of a colon-separated id file: the uid
// and the primary group for /etc/passwd, the gid alone for /etc/group.
func idFields(file string, name string) (uint64, uint64, bool) {
	for _, line := range strings.Split(file, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 3 || fields[0] != name {
			continue
		}

		id, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}

		var secondary uint64

		if len(fields) > 3 {
			secondary, _ = strconv.ParseUint(fields[3], 10, 32)
		}

		return id, secondary, true
	}

	return 0, 0, false
}
