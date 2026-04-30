package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	contracthost "github.com/AgentComputerAI/computer-host/contract"
)

func (d *Daemon) GetStorageReport(ctx context.Context) (*contracthost.GetStorageReportResponse, error) {
	volumes, err := d.store.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	snapshots, err := d.store.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	publishedPorts, err := d.store.ListPublishedPorts(ctx, "")
	if err != nil {
		return nil, err
	}

	pools := make([]contracthost.StoragePoolUsage, 0, 5)
	totalBytes := int64(0)
	addPool := func(pool contracthost.StoragePool, path string) error {
		bytes, err := directorySize(path)
		if err != nil {
			return err
		}
		pools = append(pools, contracthost.StoragePoolUsage{Pool: pool, Bytes: bytes})
		totalBytes += bytes
		return nil
	}

	for _, pool := range []struct {
		name contracthost.StoragePool
		path string
	}{
		{name: contracthost.StoragePoolArtifacts, path: d.config.ArtifactsDir},
		{name: contracthost.StoragePoolMachineDisks, path: d.config.MachineDisksDir},
		{name: contracthost.StoragePoolPublishedPort, path: ""},
		{name: contracthost.StoragePoolSnapshots, path: d.config.SnapshotsDir},
		{name: contracthost.StoragePoolState, path: filepath.Dir(d.config.StatePath)},
	} {
		if err := addPool(pool.name, pool.path); err != nil {
			return nil, err
		}
	}

	machineUsageByID := make(map[contracthost.MachineID]contracthost.MachineStorageUsage)
	for _, volume := range volumes {
		if volume.AttachedMachineID == nil {
			continue
		}
		bytes, err := fileSize(volume.Path)
		if err != nil {
			return nil, err
		}
		usage := machineUsageByID[*volume.AttachedMachineID]
		usage.MachineID = *volume.AttachedMachineID
		if volume.Kind == contracthost.VolumeKindSystem {
			usage.SystemBytes += bytes
		} else {
			usage.UserBytes += bytes
		}
		machineUsageByID[*volume.AttachedMachineID] = usage
	}
	machineUsage := make([]contracthost.MachineStorageUsage, 0, len(machineUsageByID))
	for _, usage := range machineUsageByID {
		machineUsage = append(machineUsage, usage)
	}

	snapshotUsage := make([]contracthost.SnapshotStorageUsage, 0, len(snapshots))
	for _, snapshot := range snapshots {
		bytes, err := fileSize(snapshot.MemFilePath)
		if err != nil {
			return nil, err
		}
		stateBytes, err := fileSize(snapshot.StateFilePath)
		if err != nil {
			return nil, err
		}
		bytes += stateBytes
		for _, diskPath := range snapshot.DiskPaths {
			diskBytes, err := fileSize(diskPath)
			if err != nil {
				return nil, err
			}
			bytes += diskBytes
		}
		snapshotUsage = append(snapshotUsage, contracthost.SnapshotStorageUsage{
			SnapshotID: snapshot.ID,
			Bytes:      bytes,
		})
	}

	return &contracthost.GetStorageReportResponse{
		Report: contracthost.StorageReport{
			GeneratedAt:    time.Now().UTC(),
			TotalBytes:     totalBytes,
			Pools:          pools,
			Machines:       machineUsage,
			Snapshots:      snapshotUsage,
			PublishedPorts: int64(len(publishedPorts)),
		},
	}, nil
}

func directorySize(root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %q: %w", root, err)
	}
	var total int64
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	}); err != nil {
		return 0, fmt.Errorf("walk %q: %w", root, err)
	}
	return total, nil
}

func fileSize(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return info.Size(), nil
}
