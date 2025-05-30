// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !nofilesystem
// +build !nofilesystem

package collector

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"golang.org/x/sys/unix"
)

const (
	defMountPointsExcluded = "^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+)($|/)"
	defFSTypesExcluded     = "^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|overlay|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|sysfs|tracefs)$"
)

var mountTimeout = kingpin.Flag("collector.filesystem.mount-timeout",
	"how long to wait for a mount to respond before marking it as stale").
	Hidden().Default("5s").Duration()
var statWorkerCount = kingpin.Flag("collector.filesystem.stat-workers",
	"how many stat calls to process simultaneously").
	Hidden().Default("4").Int()
var stuckMounts = make(map[string]struct{})
var stuckMountsMtx = &sync.Mutex{}

// GetStats returns filesystem stats.
func (c *filesystemCollector) GetStats() ([]filesystemStats, error) {
	mps, err := mountPointDetails(c.logger)
	if err != nil {
		return nil, err
	}
	stats := []filesystemStats{}
	labelChan := make(chan filesystemLabels)
	statChan := make(chan filesystemStats)
	wg := sync.WaitGroup{}

	workerCount := *statWorkerCount
	if workerCount < 1 {
		workerCount = 1
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for labels := range labelChan {
				statChan <- c.processStat(labels)
			}
		}()
	}

	go func() {
		for _, labels := range mps {
			if c.mountPointFilter.ignored(labels.mountPoint) {
				c.logger.Debug("Ignoring mount point", "mountpoint", labels.mountPoint)
				continue
			}
			if c.fsTypeFilter.ignored(labels.fsType) {
				c.logger.Debug("Ignoring fs type", "type", labels.fsType)
				continue
			}

			stuckMountsMtx.Lock()
			if _, ok := stuckMounts[labels.mountPoint]; ok {
				labels.deviceError = "mountpoint timeout"
				stats = append(stats, filesystemStats{
					labels:      labels,
					deviceError: 1,
				})
				c.logger.Debug("Mount point is in an unresponsive state", "mountpoint", labels.mountPoint)
				stuckMountsMtx.Unlock()
				continue
			}

			stuckMountsMtx.Unlock()
			labelChan <- labels
		}
		close(labelChan)
		wg.Wait()
		close(statChan)
	}()

	for stat := range statChan {
		stats = append(stats, stat)
	}
	return stats, nil
}

func (c *filesystemCollector) processStat(labels filesystemLabels) filesystemStats {
	var ro float64
	for _, option := range strings.Split(labels.options, ",") {
		if option == "ro" {
			ro = 1
			break
		}
	}

	success := make(chan struct{})
	go stuckMountWatcher(labels.mountPoint, success, c.logger)

	buf := new(unix.Statfs_t)
	err := unix.Statfs(rootfsFilePath(labels.mountPoint), buf)
	stuckMountsMtx.Lock()
	close(success)

	// If the mount has been marked as stuck, unmark it and log it's recovery.
	if _, ok := stuckMounts[labels.mountPoint]; ok {
		c.logger.Debug("Mount point has recovered, monitoring will resume", "mountpoint", labels.mountPoint)
		delete(stuckMounts, labels.mountPoint)
	}
	stuckMountsMtx.Unlock()

	if err != nil {
		labels.deviceError = err.Error()
		c.logger.Debug("Error on statfs() system call", "rootfs", rootfsFilePath(labels.mountPoint), "err", err)
		return filesystemStats{
			labels:      labels,
			deviceError: 1,
			ro:          ro,
		}
	}

	return filesystemStats{
		labels:    labels,
		size:      float64(buf.Blocks) * float64(buf.Bsize),
		free:      float64(buf.Bfree) * float64(buf.Bsize),
		avail:     float64(buf.Bavail) * float64(buf.Bsize),
		files:     float64(buf.Files),
		filesFree: float64(buf.Ffree),
		ro:        ro,
	}
}

// stuckMountWatcher listens on the given success channel and if the channel closes
// then the watcher does nothing. If instead the timeout is reached, the
// mount point that is being watched is marked as stuck.
func stuckMountWatcher(mountPoint string, success chan struct{}, logger *slog.Logger) {
	mountCheckTimer := time.NewTimer(*mountTimeout)
	defer mountCheckTimer.Stop()
	select {
	case <-success:
		// Success
	case <-mountCheckTimer.C:
		// Timed out, mark mount as stuck
		stuckMountsMtx.Lock()
		select {
		case <-success:
			// Success came in just after the timeout was reached, don't label the mount as stuck
		default:
			logger.Debug("Mount point timed out, it is being labeled as stuck and will not be monitored", "mountpoint", mountPoint)
			stuckMounts[mountPoint] = struct{}{}
		}
		stuckMountsMtx.Unlock()
	}
}

func mountPointDetails(logger *slog.Logger) ([]filesystemLabels, error) {
	file, err := os.Open(procFilePath("1/mountinfo"))
	if errors.Is(err, os.ErrNotExist) {
		// Fallback to `/proc/self/mountinfo` if `/proc/1/mountinfo` is missing due hidepid.
		logger.Debug("Reading root mounts failed, falling back to self mounts", "err", err)
		file, err = os.Open(procFilePath("self/mountinfo"))
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return parseFilesystemLabels(file)
}

func parseFilesystemLabels(r io.Reader) ([]filesystemLabels, error) {
	var filesystems []filesystemLabels

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())

		if len(parts) < 10 {
			return nil, fmt.Errorf("malformed mount point information: %q", scanner.Text())
		}

		major, minor := 0, 0
		_, err := fmt.Sscanf(parts[2], "%d:%d", &major, &minor)
		if err != nil {
			return nil, fmt.Errorf("malformed mount point information: %q", scanner.Text())
		}

		m := 5
		for parts[m+1] != "-" {
			m++
		}

		// Ensure we handle the translation of \040 and \011
		// as per fstab(5).
		parts[4] = strings.ReplaceAll(parts[4], "\\040", " ")
		parts[4] = strings.ReplaceAll(parts[4], "\\011", "\t")

		filesystems = append(filesystems, filesystemLabels{
			device:      parts[m+3],
			mountPoint:  rootfsStripPrefix(parts[4]),
			fsType:      parts[m+2],
			options:     parts[5],
			major:       fmt.Sprint(major),
			minor:       fmt.Sprint(minor),
			deviceError: "",
		})
	}

	return filesystems, scanner.Err()
}
