/*
Copyright cloudscale.ch
Copyright 2018 DigitalOcean

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"k8s.io/mount-utils"
	kexec "k8s.io/utils/exec"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	diskIDPath = "/dev/disk/by-id"
)

type findmntResponse struct {
	FileSystems []fileSystem `json:"filesystems"`
}

type fileSystem struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Propagation string `json:"propagation"`
	FsType      string `json:"fstype"`
	Options     string `json:"options"`
}

const (
	// blkidExitStatusNoIdentifiers defines the exit code returned from blkid indicating that no devices have been found. See http://www.polarhome.com/service/man/?qf=blkid&tf=2&of=Alpinelinux for details.
	blkidExitStatusNoIdentifiers = 2
)

type volumeStatistics struct {
	availableBytes, totalBytes, usedBytes    int64
	availableInodes, totalInodes, usedInodes int64
}

// Mounter is responsible for formatting and mounting volumes
// TODO(timoreimann): find a more suitable name since the interface encompasses
// more than just mounting functionality by now.
type Mounter interface {
	// Format formats the source with the given filesystem type
	Format(source, fsType string, luksContext LuksContext) error

	// Mount mounts source to target with the given fstype and options.
	Mount(source, target, fsType string, luksContext LuksContext, options ...string) error

	// Unmount unmounts the given target
	Unmount(target string, luksContext LuksContext) error

	// IsFormatted checks whether the source device is formatted or not. It
	// returns true if the source device is already formatted.
	IsFormatted(source string, luksContext LuksContext) (bool, error)

	// IsMounted checks whether the target path is a correct mount (i.e:
	// propagated). It returns true if it's mounted. An error is returned in
	// case of system errors or if it's mounted incorrectly.
	IsMounted(target string) (bool, error)

	// Used to find a path in /dev/disk/by-id with a serial that we have from
	// the cloudscale API.
	FinalizeVolumeAttachmentAndFindPath(logger *logrus.Entry, VolumeId string) (*string, error)

	// GetStatistics returns capacity-related volume statistics for the given
	// volume path.
	GetStatistics(volumePath string) (volumeStatistics, error)

	// IsBlockDevice checks whether the device at the path is a block device
	IsBlockDevice(volumePath string) (bool, error)
}

// TODO(arslan): this is Linux only for now. Refactor this into a package with
// architecture specific code in the future, such as mounter_darwin.go,
// mounter_linux.go, etc..
type mounter struct {
	log      *logrus.Entry
	kMounter *mount.SafeFormatAndMount
}

// newMounter returns a new mounter instance
func newMounter(log *logrus.Entry) *mounter {
	kMounter := &mount.SafeFormatAndMount{
		Interface: mount.New(""),
		Exec:      kexec.New(),
	}

	return &mounter{
		kMounter: kMounter,
		log:      log,
	}
}

func (m *mounter) Format(source, fsType string, luksContext LuksContext) error {
	mkfsCmd := fmt.Sprintf("mkfs.%s", fsType)

	_, err := exec.LookPath(mkfsCmd)
	if err != nil {
		if err == exec.ErrNotFound {
			return fmt.Errorf("%q executable not found in $PATH", mkfsCmd)
		}
		return err
	}

	mkfsArgs := []string{}

	if fsType == "" {
		return errors.New("fs type is not specified for formatting the volume")
	}

	if source == "" {
		return errors.New("source is not specified for formatting the volume")
	}

	mkfsArgs = append(mkfsArgs, source)
	if fsType == "ext4" || fsType == "ext3" {
		mkfsArgs = []string{
			"-F",  // Force flag
			"-m0", // Zero blocks reserved for privileged processes
			source,
		}
	}

	if !luksContext.EncryptionEnabled {
		m.log.WithFields(logrus.Fields{
			"cmd":  mkfsCmd,
			"args": mkfsArgs,
		}).Info("executing format command")

		out, err := exec.Command(mkfsCmd, mkfsArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("formatting disk failed: %v cmd: '%s %s' output: %q",
				err, mkfsCmd, strings.Join(mkfsArgs, " "), string(out))
		}

		return nil
	} else {
		err := luksContext.validate()
		if err != nil {
			return err
		}
		err = luksFormat(source, mkfsCmd, mkfsArgs, luksContext, m.log)
		if err != nil {
			return err
		}
		return nil
	}
}

func (m *mounter) Mount(source, target, fsType string, luksContext LuksContext, options ...string) error {
	if source == "" {
		return errors.New("source is not specified for mounting the volume")
	}

	if target == "" {
		return errors.New("target is not specified for mounting the volume")
	}

	// This is a raw block device mount. Create the mount point as a file
	// since bind mount device node requires it to be a file
	if fsType == "" {
		// create directory for target, os.Mkdirall is noop if directory exists
		err := os.MkdirAll(filepath.Dir(target), 0750)
		if err != nil {
			return fmt.Errorf("failed to create target directory for raw block bind mount: %v", err)
		}

		file, err := os.OpenFile(target, os.O_CREATE, 0660)
		if err != nil {
			return fmt.Errorf("failed to create target file for raw block bind mount: %v", err)
		}
		file.Close()
	} else {
		// create target, os.Mkdirall is noop if directory exists
		err := os.MkdirAll(target, 0750)
		if err != nil {
			return err
		}
	}

	if luksContext.EncryptionEnabled && luksContext.VolumeLifecycle == VolumeLifecycleNodeStageVolume {
		luksSource, err := luksPrepareMount(source, luksContext, m.log)
		if err != nil {
			m.log.WithFields(logrus.Fields{
				"error":  err.Error(),
				"volume": luksContext.VolumeName,
			}).Error("failed to prepare luks volume for mounting")
			return err
		}
		source = luksSource
	}

	m.log.WithFields(logrus.Fields{
		"options": options,
	}).Info("executing mount command")
	err := m.kMounter.Mount(source, target, fsType, options)
	if err != nil {
		return err
	}

	return nil
}

func (m *mounter) Unmount(target string, luksContext LuksContext) error {
	if target == "" {
		return errors.New("target is not specified for unmounting the volume")
	}

	// if this is the unmount call after the mount-bind has been removed,
	// a luks volume needs to be closed after unmounting; get the source
	// of the mount to check if that is a luks volume
	mountSources, err := getMountSources(target)

	err = mount.CleanupMountPoint(target, m.kMounter, true)
	if err != nil {
		return err
	}

	// if this is the unstaging process, check if the source is a luks volume and close it
	if luksContext.VolumeLifecycle == VolumeLifecycleNodeUnstageVolume {
		for _, source := range mountSources {
			isLuksMapping, mappingName, err := isLuksMapping(source)
			if err != nil {
				return err
			}
			if isLuksMapping {
				err := luksClose(mappingName, m.log)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// gets the mount sources of a mountpoint
func getMountSources(target string) ([]string, error) {
	_, err := exec.LookPath("findmnt")
	if err != nil {
		if err == exec.ErrNotFound {
			return nil, fmt.Errorf("%q executable not found in $PATH", "findmnt")
		}
		return nil, err
	}
	out, err := exec.Command("sh", "-c", fmt.Sprintf("findmnt -o SOURCE -n -M %s", target)).CombinedOutput()
	if err != nil {
		// findmnt exits with non zero exit status if it couldn't find anything
		if strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("checking mounted failed: %v cmd: %q output: %q",
			err, "findmnt", string(out))
	}
	return strings.Split(string(out), "\n"), nil
}

func (m *mounter) IsFormatted(source string, luksContext LuksContext) (bool, error) {
	if !luksContext.EncryptionEnabled {
		return isVolumeFormatted(source, m.log)
	}

	formatted, err := isLuksVolumeFormatted(source, luksContext, m.log)
	if err != nil {
		return false, err
	}
	return formatted, nil
}

func isVolumeFormatted(source string, log *logrus.Entry) (bool, error) {
	if source == "" {
		return false, errors.New("source is not specified")
	}

	blkidCmd := "blkid"
	_, err := exec.LookPath(blkidCmd)
	if err != nil {
		if err == exec.ErrNotFound {
			return false, fmt.Errorf("%q executable not found in $PATH", blkidCmd)
		}
		return false, err
	}

	blkidArgs := []string{source}

	log.WithFields(logrus.Fields{
		"cmd":  blkidCmd,
		"args": blkidArgs,
	}).Info("checking if source is formatted")

	exitCode := 0
	cmd := exec.Command(blkidCmd, blkidArgs...)
	err = cmd.Run()
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			return false, fmt.Errorf("checking formatting failed: %v cmd: %q, args: %q", err, blkidCmd, blkidArgs)
		}
		ws := exitError.Sys().(syscall.WaitStatus)
		exitCode = ws.ExitStatus()
		if exitCode == blkidExitStatusNoIdentifiers {
			return false, nil
		} else {
			return false, fmt.Errorf("checking formatting failed: %v cmd: %q, args: %q", err, blkidCmd, blkidArgs)
		}
	}

	return true, nil
}

func (m *mounter) IsMounted(target string) (bool, error) {
	if target == "" {
		return false, errors.New("target is not specified for checking the mount")
	}

	findmntCmd := "findmnt"
	_, err := exec.LookPath(findmntCmd)
	if err != nil {
		if err == exec.ErrNotFound {
			return false, fmt.Errorf("%q executable not found in $PATH", findmntCmd)
		}
		return false, err
	}

	findmntArgs := []string{"-o", "TARGET,PROPAGATION,FSTYPE,OPTIONS", "-M", target, "-J"}

	m.log.WithFields(logrus.Fields{
		"cmd":  findmntCmd,
		"args": findmntArgs,
	}).Info("checking if target is mounted")

	out, err := exec.Command(findmntCmd, findmntArgs...).CombinedOutput()
	if err != nil {
		// findmnt exits with non zero exit status if it couldn't find anything
		if strings.TrimSpace(string(out)) == "" {
			return false, nil
		}

		return false, fmt.Errorf("checking mounted failed: %v cmd: %q output: %q",
			err, findmntCmd, string(out))
	}

	// no response means there is no mount
	if string(out) == "" {
		return false, nil
	}

	var resp *findmntResponse
	err = json.Unmarshal(out, &resp)
	if err != nil {
		return false, fmt.Errorf("couldn't unmarshal data: %q: %s", string(out), err)
	}

	targetFound := false
	for _, fs := range resp.FileSystems {
		// check if the mount is propagated correctly. It should be set to shared.
		if fs.Propagation != "shared" {
			return true, fmt.Errorf("mount propagation for target %q is not enabled", target)
		}

		// the mountpoint should match as well
		if fs.Target == target {
			targetFound = true
		}
	}

	return targetFound, nil
}

// Copyright note for the functions below. Originally taken from
// https://github.com/kubernetes/cloud-provider-openstack/blob/v1.16.0/pkg/volume/cinder/cinder_util.go
// Sleightly modified.
/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

func guessDiskIDPathByVolumeID(volumeID string) *string {
	// Get the first part of the UUID.
	// The linux kernel limits volume serials to 20 bytes:
	// include/uapi/linux/virtio_blk.h:#define VIRTIO_BLK_ID_BYTES 20 /* ID string length */
	linuxSerial := volumeID[:20]

	globExpr := diskIDPath + "/*" + linuxSerial + "*"
	matches, _ := filepath.Glob(globExpr)
	if len(matches) > 0 {
		return &matches[0]
	}
	return nil
}

func (m *mounter) FinalizeVolumeAttachmentAndFindPath(logger *logrus.Entry, volumeID string) (*string, error) {
	numTries := 0
	for {
		probeAttachedVolume(logger)

		diskIDPath := guessDiskIDPathByVolumeID(volumeID)
		if diskIDPath != nil {
			return diskIDPath, nil
		}

		numTries++
		if numTries == 10 {
			break
		}
		time.Sleep(time.Second)
	}
	return nil, errors.New("Could not attach disk: Timeout after 10s")
}

func probeAttachedVolume(logger *logrus.Entry) error {
	// rescan scsi bus
	scsiHostRescan()

	// udevadm settle waits for udevd to process the device creation
	// events for all hardware devices, thus ensuring that any device
	// nodes have been created successfully before proceeding.
	argsSettle := []string{"settle"}
	cmdSettle := exec.Command("udevadm", argsSettle...)
	_, errSettle := cmdSettle.CombinedOutput()
	if errSettle != nil {
		logger.Errorf("error running udevadm settle %v\n", errSettle)
	}

	args := []string{"trigger"}
	cmd := exec.Command("udevadm", args...)
	_, err := cmd.CombinedOutput()
	if err != nil {
		logger.Errorf("error running udevadm trigger %v\n", err)
		return err
	}
	logger.Debugf("Successfully probed all attachments")
	return nil
}

func scsiHostRescan() {
	scsiPath := "/sys/class/scsi_host/"
	if dirs, err := ioutil.ReadDir(scsiPath); err == nil {
		for _, f := range dirs {
			name := scsiPath + f.Name() + "/scan"
			data := []byte("- - -")
			ioutil.WriteFile(name, data, 0666)
		}
	}
}

func (m *mounter) GetStatistics(volumePath string) (volumeStatistics, error) {
	isBlock, err := m.IsBlockDevice(volumePath)
	if err != nil {
		return volumeStatistics{}, fmt.Errorf("failed to determine if volume %s is block device: %v", volumePath, err)
	}

	if isBlock {
		// See http://man7.org/linux/man-pages/man8/blockdev.8.html for details
		output, err := exec.Command("blockdev", "getsize64", volumePath).CombinedOutput()
		if err != nil {
			return volumeStatistics{}, fmt.Errorf("error when getting size of block volume at path %s: output: %s, err: %v", volumePath, string(output), err)
		}
		strOut := strings.TrimSpace(string(output))
		gotSizeBytes, err := strconv.ParseInt(strOut, 10, 64)
		if err != nil {
			return volumeStatistics{}, fmt.Errorf("failed to parse size %s into int", strOut)
		}

		return volumeStatistics{
			totalBytes: gotSizeBytes,
		}, nil
	}

	var statfs unix.Statfs_t
	// See http://man7.org/linux/man-pages/man2/statfs.2.html for details.
	err = unix.Statfs(volumePath, &statfs)
	if err != nil {
		return volumeStatistics{}, err
	}

	volStats := volumeStatistics{
		availableBytes: int64(statfs.Bavail) * int64(statfs.Bsize),
		totalBytes:     int64(statfs.Blocks) * int64(statfs.Bsize),
		usedBytes:      (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize),

		availableInodes: int64(statfs.Ffree),
		totalInodes:     int64(statfs.Files),
		usedInodes:      int64(statfs.Files) - int64(statfs.Ffree),
	}

	return volStats, nil
}

func (m *mounter) IsBlockDevice(devicePath string) (bool, error) {
	var stat unix.Stat_t
	err := unix.Stat(devicePath, &stat)
	if err != nil {
		return false, err
	}

	return (stat.Mode & unix.S_IFMT) == unix.S_IFBLK, nil
}
