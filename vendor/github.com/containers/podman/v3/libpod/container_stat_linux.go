// +build linux

package libpod

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/buildah/copier"
	"github.com/containers/podman/v3/libpod/define"
	"github.com/containers/podman/v3/pkg/copy"
	"github.com/pkg/errors"
)

// statInsideMount stats the specified path *inside* the container's mount and PID
// namespace.  It returns the file info along with the resolved root ("/") and
// the resolved path (relative to the root).
func (c *Container) statInsideMount(ctx context.Context, containerPath string) (*copier.StatForItem, string, string, error) {
	resolvedRoot := "/"
	resolvedPath := c.pathAbs(containerPath)
	var statInfo *copier.StatForItem

	err := c.joinMountAndExec(ctx,
		func() error {
			var statErr error
			statInfo, statErr = secureStat(resolvedRoot, resolvedPath)
			return statErr
		},
	)

	return statInfo, resolvedRoot, resolvedPath, err
}

// statOnHost stats the specified path *on the host*.  It returns the file info
// along with the resolved root and the resolved path.  Both paths are absolute
// to the host's root.  Note that the paths may resolved outside the
// container's mount point (e.g., to a volume or bind mount).
func (c *Container) statOnHost(ctx context.Context, mountPoint string, containerPath string) (*copier.StatForItem, string, string, error) {
	// Now resolve the container's path.  It may hit a volume, it may hit a
	// bind mount, it may be relative.
	resolvedRoot, resolvedPath, err := c.resolvePath(mountPoint, containerPath)
	if err != nil {
		return nil, "", "", err
	}

	statInfo, err := secureStat(resolvedRoot, resolvedPath)
	return statInfo, resolvedRoot, resolvedPath, err
}

func (c *Container) stat(ctx context.Context, containerMountPoint string, containerPath string) (*define.FileInfo, string, string, error) {
	var (
		resolvedRoot     string
		resolvedPath     string
		absContainerPath string
		statInfo         *copier.StatForItem
		statErr          error
	)

	// Make sure that "/" copies the *contents* of the mount point and not
	// the directory.
	if containerPath == "/" {
		containerPath = "/."
	}

	if c.state.State == define.ContainerStateRunning {
		// If the container is running, we need to join it's mount namespace
		// and stat there.
		statInfo, resolvedRoot, resolvedPath, statErr = c.statInsideMount(ctx, containerPath)
	} else {
		// If the container is NOT running, we need to resolve the path
		// on the host.
		statInfo, resolvedRoot, resolvedPath, statErr = c.statOnHost(ctx, containerMountPoint, containerPath)
	}

	if statErr != nil {
		if statInfo == nil {
			return nil, "", "", statErr
		}
		// Not all errors from secureStat map to ErrNotExist, so we
		// have to look into the error string.  Turning it into an
		// ENOENT let's the API handlers return the correct status code
		// which is crucial for the remote client.
		if os.IsNotExist(statErr) || strings.Contains(statErr.Error(), "o such file or directory") {
			statErr = copy.ErrENOENT
		}
	}

	if statInfo.IsSymlink {
		// Evaluated symlinks are always relative to the container's mount point.
		absContainerPath = statInfo.ImmediateTarget
	} else if strings.HasPrefix(resolvedPath, containerMountPoint) {
		// If the path is on the container's mount point, strip it off.
		absContainerPath = strings.TrimPrefix(resolvedPath, containerMountPoint)
		absContainerPath = filepath.Join("/", absContainerPath)
	} else {
		// No symlink and not on the container's mount point, so let's
		// move it back to the original input.  It must have evaluated
		// to a volume or bind mount but we cannot return host paths.
		absContainerPath = containerPath
	}

	// Preserve the base path as specified by the user.  The `filepath`
	// packages likes to remove trailing slashes and dots that are crucial
	// to the copy logic.
	absContainerPath = copy.PreserveBasePath(containerPath, absContainerPath)
	resolvedPath = copy.PreserveBasePath(containerPath, resolvedPath)

	info := &define.FileInfo{
		IsDir:      statInfo.IsDir,
		Name:       filepath.Base(absContainerPath),
		Size:       statInfo.Size,
		Mode:       statInfo.Mode,
		ModTime:    statInfo.ModTime,
		LinkTarget: absContainerPath,
	}

	return info, resolvedRoot, resolvedPath, statErr
}

// secureStat extracts file info for path in a chroot'ed environment in root.
func secureStat(root string, path string) (*copier.StatForItem, error) {
	var glob string
	var err error

	// If root and path are equal, then dir must be empty and the glob must
	// be ".".
	if filepath.Clean(root) == filepath.Clean(path) {
		glob = "."
	} else {
		glob, err = filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
	}

	globStats, err := copier.Stat(root, "", copier.StatOptions{}, []string{glob})
	if err != nil {
		return nil, err
	}

	if len(globStats) != 1 {
		return nil, errors.Errorf("internal error: secureStat: expected 1 item but got %d", len(globStats))
	}

	stat, exists := globStats[0].Results[glob] // only one glob passed, so that's okay
	if !exists {
		return nil, copy.ErrENOENT
	}

	var statErr error
	if stat.Error != "" {
		statErr = errors.New(stat.Error)
	}
	return stat, statErr
}
