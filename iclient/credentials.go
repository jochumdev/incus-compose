package iclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A helper reads a keyring, it does not wait on a human.
const credHelperTimeout = 30 * time.Second

// credEntry is one remote's resolved login, or why there is none. The helper
// runs once per entry, whatever it answers.
type credEntry struct {
	once sync.Once

	username string
	password string
	err      error
}

// applyCredentials moves a registry's login out of its addresses and into info.
// Nothing that logs or formats an address can leak it afterwards, and the
// address is a URL again only where incusd's API demands one.
//
// A credentials helper wins over an address that carries its own, matching what
// the incus CLI does with the same remote.
func (c *Config) applyCredentials(remote string, r ConfigRemote, info *ConfigRemoteInfo) error {
	// rollingAddrs built this slice, so rewriting it does not touch the Config.
	for i, addr := range info.Addrs {
		parsed, err := url.Parse(addr)
		if err != nil {
			return fmt.Errorf("%q: parsing the registry address: %w", remote, err)
		}

		if parsed.User == nil {
			continue
		}

		if info.Username == "" {
			info.Username = parsed.User.Username()
			info.Password, _ = parsed.User.Password()
		}

		parsed.User = nil
		info.Addrs[i] = parsed.String()
	}

	if r.CredHelper == "" {
		return nil
	}

	entry := c.credentials(remote, r.CredHelper, info.Addrs[0])
	if entry.err != nil {
		return entry.err
	}

	info.Username, info.Password = entry.username, entry.password

	return nil
}

// credentials runs remote's helper, at most once per Config. ReadConfig gave
// every remote an entry of its own, so nothing here writes the map and two
// registries resolve at the same time. A Config built by hand has no entry and
// resolves without memoising.
func (c *Config) credentials(remote string, helper string, addr string) *credEntry {
	entry, ok := c.creds[remote]
	if !ok {
		entry = &credEntry{}
	}

	entry.once.Do(func() { entry.fill(helper, addr) })

	return entry
}

// fill makes the call the incus CLI makes: the registry host on stdin, a
// docker credentials helper's JSON back.
func (e *credEntry) fill(helper string, addr string) {
	parsed, err := url.Parse(addr)
	if err != nil {
		e.err = fmt.Errorf("%w %q: parsing the registry address: %w", ErrCredHelper, helper, err)

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), credHelperTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, helper, "get") //nolint:gosec // the helper is named by the user's own configuration
	cmd.Stdin = strings.NewReader(parsed.Host)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err = cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			e.err = fmt.Errorf("%w %q: %w: %s", ErrCredHelper, helper, err, detail)

			return
		}

		e.err = fmt.Errorf("%w %q: %w", ErrCredHelper, helper, err)

		return
	}

	// The wire names are docker's, not ours.
	var res struct {
		Username string
		Secret   string
	}

	err = json.Unmarshal(stdout.Bytes(), &res)
	if err != nil {
		e.err = fmt.Errorf("%w %q: decoding its response: %w", ErrCredHelper, helper, err)

		return
	}

	e.username, e.password = res.Username, res.Secret
}
