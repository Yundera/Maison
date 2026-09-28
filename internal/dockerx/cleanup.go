package dockerx

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
)

// The reads and removals behind Settings › Resources › Storage. Each one is the
// primitive only: which images, containers and networks are safe to remove is
// decided in internal/cleanup, from the whole box's state, never here.

// ImageInfo is one local image, with what it costs on disk.
type ImageInfo struct {
	ID      string   `json:"id"`
	Tags    []string `json:"tags"`
	Digests []string `json:"-"`
	// Size is the image's full size, layers it shares with other images included.
	// SharedSize is the part of it that other images also hold — so Size-SharedSize
	// is what removing this one image alone gives back. -1 when the daemon did not
	// compute it.
	Size       int64 `json:"size"`
	SharedSize int64 `json:"-"`
	// Containers is how many containers, in any state, use the image. -1 when the
	// daemon did not count them.
	Containers int64 `json:"-"`
}

// ContainerInfo is one container of any kind — compose-managed or not.
type ContainerInfo struct {
	ID         string
	Name       string
	Image      string
	ImageID    string
	State      string
	Project    string
	Service    string
	WorkingDir string
	// ConfigFiles is compose's comma-separated list of the files the project was
	// brought up from, as the HOST spells them.
	ConfigFiles string
	// Networks are the ids of every network the container is attached to. A
	// stopped container keeps these, and starting it again fails if one is gone.
	Networks []string
	// SizeRw is the container's writable layer. Only DiskUsage fills it.
	SizeRw int64
}

// NetworkInfo is one Docker network.
type NetworkInfo struct {
	ID     string
	Name   string
	Scope  string
	Labels map[string]string
}

// DiskUsage is Docker's own accounting of what it holds on disk.
type DiskUsage struct {
	Images     []ImageInfo
	Containers []ContainerInfo
	// LayersSize is the images' total on disk, each shared layer counted once —
	// the figure `docker system df` shows, unlike a sum of the Size fields.
	LayersSize   int64
	VolumesBytes int64
	VolumesCount int
	// BuildCacheBytes is the whole build cache; BuildCacheReclaimable the part no
	// running build holds.
	BuildCacheBytes       int64
	BuildCacheReclaimable int64
}

// DiskUsage asks the daemon what its images, containers, volumes and build cache
// occupy. It can take seconds on a box with many volumes: the daemon walks each
// one to size it.
func (c *Client) DiskUsage(ctx context.Context) (DiskUsage, error) {
	du, err := c.cli.DiskUsage(ctx, types.DiskUsageOptions{})
	if err != nil {
		return DiskUsage{}, err
	}
	out := DiskUsage{LayersSize: du.LayersSize}
	for _, im := range du.Images {
		if im == nil {
			continue
		}
		out.Images = append(out.Images, imageInfo(*im))
	}
	for _, ct := range du.Containers {
		if ct == nil {
			continue
		}
		info := containerInfo(*ct)
		info.SizeRw = ct.SizeRw
		out.Containers = append(out.Containers, info)
	}
	for _, v := range du.Volumes {
		if v == nil {
			continue
		}
		out.VolumesCount++
		if v.UsageData != nil && v.UsageData.Size > 0 {
			out.VolumesBytes += v.UsageData.Size
		}
	}
	for _, b := range du.BuildCache {
		if b == nil {
			continue
		}
		out.BuildCacheBytes += b.Size
		if !b.InUse && !b.Shared {
			out.BuildCacheReclaimable += b.Size
		}
	}
	return out, nil
}

func imageInfo(im image.Summary) ImageInfo {
	var tags []string
	for _, t := range im.RepoTags {
		// "<none>:<none>" is how the daemon spells an untagged image.
		if t != "" && t != "<none>:<none>" {
			tags = append(tags, t)
		}
	}
	var digests []string
	for _, d := range im.RepoDigests {
		if d != "" && d != "<none>@<none>" {
			digests = append(digests, d)
		}
	}
	return ImageInfo{
		ID:         im.ID,
		Tags:       tags,
		Digests:    digests,
		Size:       im.Size,
		SharedSize: im.SharedSize,
		Containers: im.Containers,
	}
}

func containerInfo(ct types.Container) ContainerInfo {
	name := ""
	if len(ct.Names) > 0 {
		name = strings.TrimPrefix(ct.Names[0], "/")
	}
	var nets []string
	if ct.NetworkSettings != nil {
		for _, n := range ct.NetworkSettings.Networks {
			if n != nil && n.NetworkID != "" {
				nets = append(nets, n.NetworkID)
			}
		}
	}
	return ContainerInfo{
		ID:          ct.ID,
		Name:        name,
		Image:       ct.Image,
		ImageID:     ct.ImageID,
		State:       ct.State,
		Project:     ct.Labels[labelProject],
		Service:     ct.Labels[labelService],
		WorkingDir:  ct.Labels[labelWorkingDir],
		ConfigFiles: ct.Labels[labelConfigFile],
		Networks:    nets,
	}
}

// Images lists every local image, intermediate layers excluded.
func (c *Client) Images(ctx context.Context) ([]ImageInfo, error) {
	list, err := c.cli.ImageList(ctx, image.ListOptions{All: false, SharedSize: true, ContainerCount: true})
	if err != nil {
		return nil, err
	}
	out := make([]ImageInfo, 0, len(list))
	for _, im := range list {
		out = append(out, imageInfo(im))
	}
	return out, nil
}

// Containers lists every container, in any state, compose-managed or not.
func (c *Client) Containers(ctx context.Context) ([]ContainerInfo, error) {
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, ct := range list {
		out = append(out, containerInfo(ct))
	}
	return out, nil
}

// Networks lists every network.
func (c *Client) Networks(ctx context.Context) ([]NetworkInfo, error) {
	list, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]NetworkInfo, 0, len(list))
	for _, n := range list {
		out = append(out, NetworkInfo{ID: n.ID, Name: n.Name, Scope: n.Scope, Labels: n.Labels})
	}
	return out, nil
}

// RemoveImage removes one image reference — a tag, or an id for an untagged image.
//
// Never forced. The daemon's own refusal — an image a container was created from
// since the plan was made — is the last guard against a race with an install, and
// forcing would talk straight past it.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.cli.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
	return err
}

// RemoveContainer stops and removes one container with its anonymous volumes — what
// an uninstall does to each of an app's containers (see RemoveProject). Bind-mounted
// data under the data root is not a volume and is left alone.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	_ = c.cli.ContainerStop(ctx, id, container.StopOptions{})
	return c.cli.ContainerRemove(ctx, id, container.RemoveOptions{RemoveVolumes: true, Force: true})
}

// RemoveNetwork removes one network. The daemon refuses one with a container
// attached, which again is the guard against a race.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.cli.NetworkRemove(ctx, id)
}

// PruneBuildCache empties the build cache of everything no running build holds,
// and reports what that gave back.
func (c *Client) PruneBuildCache(ctx context.Context) (uint64, error) {
	rep, err := c.cli.BuildCachePrune(ctx, types.BuildCachePruneOptions{All: true})
	if err != nil {
		return 0, err
	}
	return rep.SpaceReclaimed, nil
}
