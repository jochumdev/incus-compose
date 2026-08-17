package client

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	rspecs "github.com/opencontainers/runtime-spec/specs-go"
)

// BuildMode controls how build-configured images are treated during Ensure.
type BuildMode int

const (
	// BuildAuto builds the image only when it is missing (default).
	BuildAuto BuildMode = iota
	// BuildForce rebuilds the image even if an existing one is present.
	BuildForce
	// BuildNever never builds; returns an error if the image is missing.
	BuildNever
)

// BuildInfo carries the rebuild mode and optional builder selection for ActionEnsure.
type BuildInfo struct {
	// Mode controls rebuild behavior.
	Mode BuildMode

	// PreferredBuilder is the container builder binary name or absolute path.
	// Empty means auto-detect (tries podman, then docker).
	PreferredBuilder string
}

// BuildConfig holds the parameters read from a compose service's build: block.
type BuildConfig struct {
	// Context is the build context directory (absolute path).
	Context string

	// Dockerfile is an optional path to the Containerfile/Dockerfile, passed to
	// the builder verbatim, so a relative path resolves against the process
	// working directory rather than Context. Empty means the builder uses its
	// default (Containerfile or Dockerfile in Context).
	Dockerfile string

	// DockerfileInline is inline Dockerfile content from compose build.dockerfile_inline.
	DockerfileInline string

	// Target is the Dockerfile stage to build.
	Target string

	// Platform is the OCI platform to build for, for example linux/amd64.
	Platform string

	// Args are build-time variables (--build-arg).
	Args map[string]string

	// NoCache disables layer caching during the build as well as caching
	// the resulting image on the server.
	NoCache bool

	// Pull always attempts to pull a newer version of the base image.
	Pull bool
}

// incusArchToPlatform maps an Incus architecture name to an OCI platform.
func incusArchToPlatform(arch string) (string, bool) {
	switch arch {
	case "x86_64":
		return "linux/amd64", true
	case "i686":
		return "linux/386", true
	case "aarch64":
		return "linux/arm64", true
	case "armv7", "armv7l":
		return "linux/arm/v7", true
	case "armv6", "armv6l":
		return "linux/arm/v6", true
	case "ppc64le":
		return "linux/ppc64le", true
	case "s390x":
		return "linux/s390x", true
	case "riscv64":
		return "linux/riscv64", true
	}
	return "", false
}

func platformToIncusArch(platform string, arches []string) (string, bool) {
	for _, arch := range arches {
		candidate, ok := incusArchToPlatform(arch)
		if ok && candidate == platform {
			return arch, true
		}
	}
	return "", false
}

// buildDetectBuilder returns the path to the container builder.
// If preferredBuilder is non-empty it is resolved via exec.LookPath (works for
// both bare names and absolute paths). Otherwise podman and docker are tried in order.
func buildDetectBuilder(preferredBuilder string) (string, error) {
	if preferredBuilder != "" {
		p, err := exec.LookPath(preferredBuilder)
		if err != nil {
			return "", fmt.Errorf("builder %q not found: %w", preferredBuilder, err)
		}
		return p, nil
	}
	for _, name := range []string{"buildah", "podman", "docker"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no container builder found; install podman or docker, or set a preferred builder")
}

// buildRootfs runs the container builder and returns both the rootfs tar and
// the OCI runtime config.json bytes. The rootfs is a ReadCloser that deletes
// its temp file on Close. stdout/stderr are forwarded.
func buildRootfs(ctx context.Context, c *Client, builder string, cfg *BuildConfig, stdout io.Writer, stderr io.Writer) (io.ReadCloser, []byte, *ocispec.ImageConfig, error) {
	isPodman := strings.HasSuffix(builder, "podman") || strings.HasSuffix(builder, "buildah")
	tmpTag := fmt.Sprintf("ic-compose-build-%x", time.Now().UnixNano())

	rootfsTmp, err := os.CreateTemp("", "incus-compose-rootfs-*.tar")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating temp file: %w", err)
	}
	rootfsPath := rootfsTmp.Name()
	err = rootfsTmp.Close()
	if err != nil {
		return nil, nil, nil, err
	}

	buildCfg, cleanup, err := buildConfigWithInlineDockerfile(cfg)
	if err != nil {
		_ = os.Remove(rootfsPath)
		return nil, nil, nil, err
	}
	defer cleanup()

	args := buildArgs(isPodman, buildCfg, tmpTag, rootfsPath)
	c.LogDebug("Executing", "command", builder, "args", args)

	cmd := exec.CommandContext(ctx, builder, args...) //nolint:gosec
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(rootfsPath)
		return nil, nil, nil, fmt.Errorf("building container image: %w", err)
	}

	defer func() {
		// Remove the temporary image tag; ignore errors (best-effort cleanup).
		c.LogDebug("Executing", "command", builder, "args", []string{"rmi", tmpTag})
		rmi := exec.CommandContext(ctx, builder, "rmi", tmpTag) //nolint:gosec
		rmi.Stdout = stdout
		rmi.Stderr = stderr
		_ = rmi.Run()
	}()

	// Incus reads only Process.{Args,Env,Cwd,User} out of config.json, so inspecting beats saving the image and unpacking it with umoci.
	inspect := exec.CommandContext(ctx, builder, "inspect", tmpTag) //nolint:gosec
	inspect.Stderr = stderr
	c.LogDebug("Executing", "command", builder, "args", inspect.Args[1:])
	out, err := inspect.Output()
	if err != nil {
		_ = os.Remove(rootfsPath)
		return nil, nil, nil, fmt.Errorf("inspecting built image: %w", err)
	}

	// buildah nests the OCI image config differently from podman/docker
	// (buildah: top-level object, config under .OCIv1.config; podman/docker:
	// array of objects, config under .[].Config - podman mirrors docker's
	// inspect format for compatibility), so decode loosely into
	// map[string]any and pull out just the fields we need instead of
	// depending on all three projects' Go types staying field-compatible.
	var imgCfg map[string]any
	if strings.HasSuffix(builder, "buildah") {
		// buildah inspect <tag> | jq '.OCIv1.config'
		var inspected map[string]any
		if err := json.Unmarshal(out, &inspected); err != nil {
			_ = os.Remove(rootfsPath)
			return nil, nil, nil, fmt.Errorf("parsing buildah inspect output: %w", err)
		}
		ociv1, _ := inspected["OCIv1"].(map[string]any)
		imgCfg, _ = ociv1["config"].(map[string]any)
	} else {
		// podman/docker inspect <tag> | jq '.[].Config'
		var inspected []map[string]any
		if err := json.Unmarshal(out, &inspected); err != nil || len(inspected) == 0 {
			_ = os.Remove(rootfsPath)
			return nil, nil, nil, fmt.Errorf("parsing %s inspect output: %w", builder, err)
		}
		imgCfg, _ = inspected[0]["Config"].(map[string]any)
	}

	toStrings := func(v any) []string {
		raw, _ := v.([]any)
		out := make([]string, 0, len(raw))
		for _, e := range raw {
			s, ok := e.(string)
			if ok {
				out = append(out, s)
			}
		}
		return out
	}

	cwd, _ := imgCfg["WorkingDir"].(string)
	if cwd == "" {
		cwd = "/"
	}

	// Only numeric uid[:gid] resolves; a named USER can't be resolved
	// without the rootfs' /etc/passwd and falls back to root, matching
	// the numeric-only restriction on the compose `user:` override.
	user, _ := imgCfg["User"].(string)

	var uid, gid uint64
	if user != "" {
		split := strings.SplitN(user, ":", 2)
		uid, _ = strconv.ParseUint(split[0], 10, 32)
		if len(split) > 1 {
			gid, _ = strconv.ParseUint(split[1], 10, 32)
		}
	}

	env := slices.Clone(ociDefaultEnv)
	for _, entry := range toStrings(imgCfg["Env"]) {
		env = putEnv(env, entry, true)
	}

	home, err := rootfsHome(rootfsPath, uid)
	if err != nil {
		_ = os.Remove(rootfsPath)
		return nil, nil, nil, fmt.Errorf("reading /etc/passwd from the built rootfs: %w", err)
	}
	if home != "" {
		env = putEnv(env, "HOME="+home, false)
	}

	// Every builder spells VOLUME as an object with empty values, not a list.
	rawVolumes, _ := imgCfg["Volumes"].(map[string]any)
	volumes := make(map[string]struct{}, len(rawVolumes))

	for path := range rawVolumes {
		volumes[path] = struct{}{}
	}

	// The runtime spec has one argv, so the split is kept alongside it rather
	// than read back out of Args, which cannot say where the entrypoint ended.
	config := &ocispec.ImageConfig{
		User:       user,
		Entrypoint: toStrings(imgCfg["Entrypoint"]),
		Cmd:        toStrings(imgCfg["Cmd"]),
		WorkingDir: cwd,
		Volumes:    volumes,
	}

	configJSON, err := json.Marshal(rspecs.Spec{
		Version: rspecs.Version,
		Process: &rspecs.Process{
			Args: slices.Concat(config.Entrypoint, config.Cmd),
			Env:  env,
			Cwd:  cwd,
			User: rspecs.User{UID: uint32(uid), GID: uint32(gid)},
		},
	})
	if err != nil {
		_ = os.Remove(rootfsPath)
		return nil, nil, nil, fmt.Errorf("marshaling config.json: %w", err)
	}

	f, err := os.Open(rootfsPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening rootfs: %w", err)
	}
	return &tempFile{File: f, path: rootfsPath}, configJSON, config, nil
}

// ociDefaultEnv is what umoci seeds a runtime spec with, so a built image gets the environment.* keys a pulled one does.
var ociDefaultEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"TERM=xterm",
}

// putEnv sets entry in env, replacing a value already there only when clobber.
func putEnv(env []string, entry string, clobber bool) []string {
	name, _, ok := strings.Cut(entry, "=")
	if !ok {
		return env
	}

	for i, e := range env {
		if strings.HasPrefix(e, name+"=") {
			if clobber {
				env[i] = entry
			}
			return env
		}
	}
	return append(env, entry)
}

// rootfsHome returns uid's home from the rootfs tar's /etc/passwd, or "" when there is no entry.
func rootfsHome(path string, uid uint64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}

		if strings.TrimPrefix(hdr.Name, "./") != "etc/passwd" {
			continue
		}

		scanner := bufio.NewScanner(tr)
		for scanner.Scan() {
			fields := strings.Split(scanner.Text(), ":")
			if len(fields) >= 6 && fields[2] == strconv.FormatUint(uid, 10) {
				return fields[5], nil
			}
		}
		return "", scanner.Err()
	}
}

func buildConfigWithInlineDockerfile(cfg *BuildConfig) (*BuildConfig, func(), error) {
	if cfg.DockerfileInline == "" {
		return cfg, func() {}, nil
	}
	if cfg.Dockerfile != "" {
		return nil, func() {}, fmt.Errorf("build.dockerfile and build.dockerfile_inline cannot both be set")
	}

	f, err := os.CreateTemp("", "incus-compose-Dockerfile-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("creating inline Dockerfile: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(cfg.DockerfileInline); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("writing inline Dockerfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("closing inline Dockerfile: %w", err)
	}

	buildCfg := *cfg
	buildCfg.Dockerfile = path
	return &buildCfg, func() { _ = os.Remove(path) }, nil
}

func buildArgs(isPodman bool, cfg *BuildConfig, tmpTag, dest string) []string {
	args := []string{}
	if !isPodman {
		args = append(args, "buildx")
	}
	args = append(args, "build", "--tag", tmpTag)
	if cfg.Dockerfile != "" {
		args = append(args, "--file", cfg.Dockerfile)
	}
	if cfg.Platform != "" {
		args = append(args, "--platform", cfg.Platform)
	}
	if cfg.Target != "" {
		args = append(args, "--target", cfg.Target)
	}
	for k, v := range cfg.Args {
		args = append(args, "--build-arg", k+"="+v)
	}
	if cfg.NoCache {
		args = append(args, "--no-cache")
	}
	if cfg.Pull {
		args = append(args, "--pull")
	}
	args = append(args, "--output", "type=tar,dest="+dest)
	if !isPodman {
		// buildx's --output alone only exports the tar; it does not load
		// the image into the local docker store the way buildah/podman's
		// own `build` command does regardless of --output. --load is
		// needed so the image can be inspected afterward for config.json.
		args = append(args, "--load")
	}
	args = append(args, cfg.Context)
	return args
}

type tempFile struct {
	*os.File
	path string
}

// Close closes the file and removes it from disk.
func (t *tempFile) Close() error {
	err := t.File.Close()
	// _ = os.Remove(t.path)
	return err
}

// buildMetadataTar returns an in-memory tar containing metadata.yaml (JSON
// content per Incus convention) and, when provided, an OCI config.json.
func buildMetadataTar(name, arch string, configJSON []byte) (io.Reader, error) {
	metaJSON, err := json.Marshal(incusApi.ImageMetadata{
		Architecture: arch,
		CreationDate: time.Now().Unix(),
		Properties: map[string]string{
			"description": name + " (built by incus-compose)",
			"type":        "oci",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling image metadata: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{
		Name: "metadata.yaml",
		Mode: 0o644,
		Size: int64(len(metaJSON)),
	}); err != nil {
		return nil, fmt.Errorf("writing metadata tar header: %w", err)
	}
	if _, err := tw.Write(metaJSON); err != nil {
		return nil, fmt.Errorf("writing metadata.yaml: %w", err)
	}

	if len(configJSON) > 0 {
		if err := tw.WriteHeader(&tar.Header{
			Name: "config.json",
			Mode: 0o644,
			Size: int64(len(configJSON)),
		}); err != nil {
			return nil, fmt.Errorf("writing config.json tar header: %w", err)
		}
		if _, err := tw.Write(configJSON); err != nil {
			return nil, fmt.Errorf("writing config.json: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing metadata tar: %w", err)
	}
	return &buf, nil
}
