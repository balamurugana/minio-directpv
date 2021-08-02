// This file is part of MinIO Direct CSI
// Copyright (c) 2021 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package drive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	directcsi "github.com/minio/direct-csi/pkg/apis/direct.csi.min.io/v1beta2"
	"github.com/minio/direct-csi/pkg/clientset"
	"github.com/minio/direct-csi/pkg/listener2"
	"github.com/minio/direct-csi/pkg/sys"
	"github.com/minio/direct-csi/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type DriveUpdateType int

const (
	DriveUpdateTypeOwnAndFormat DriveUpdateType = iota + 1
	DriveUpdateTypeUnknown
)

type DriveEventHandler struct {
	directCSIClient clientset.Interface
	kubeClient      kubernetes.Interface
	nodeID          string
	mounter         sys.DriveMounter
	formatter       sys.DriveFormatter
	statter         sys.DriveStatter
}

func NewDriveEventHandler(nodeID string) *DriveEventHandler {
	return &DriveEventHandler{
		directCSIClient: utils.GetDirectClientset(),
		kubeClient:      utils.GetKubeClient(),
		nodeID:          nodeID,
		mounter:         &sys.DefaultDriveMounter{},
		formatter:       &sys.DefaultDriveFormatter{},
		statter:         &sys.DefaultDriveStatter{},
	}
}

func (handler *DriveEventHandler) ListerWatcher() cache.ListerWatcher {
	labelSelector := ""
	if handler.nodeID != "" {
		labelSelector = fmt.Sprintf("%s=%s", utils.NodeLabel, utils.SanitizeLabelV(handler.nodeID))
	}

	optionsModifier := func(options *metav1.ListOptions) {
		options.LabelSelector = labelSelector
	}

	return cache.NewFilteredListWatchFromClient(
		handler.directCSIClient.DirectV1beta2().RESTClient(),
		"DirectCSIDrives",
		"",
		optionsModifier,
	)
}

func (handler *DriveEventHandler) RESTClient() rest.Interface {
	return handler.directCSIClient.DirectV1beta2().RESTClient()
}

func (handler *DriveEventHandler) KubeClient() kubernetes.Interface {
	return handler.kubeClient
}

func (handler *DriveEventHandler) Name() string {
	return "drive"
}

func (handler *DriveEventHandler) ObjectType() runtime.Object {
	return &directcsi.DirectCSIDrive{}
}

func (handler *DriveEventHandler) Handle(ctx context.Context, args listener2.EventArgs) error {
	switch args.Event {
	case listener2.AddEvent, listener2.UpdateEvent:
		return handler.update(ctx, args.Object.(*directcsi.DirectCSIDrive))
	case listener2.DeleteEvent:
		return handler.delete(ctx, args.Object.(*directcsi.DirectCSIDrive))
	}
	return nil
}

func (handler *DriveEventHandler) update(ctx context.Context, drive *directcsi.DirectCSIDrive) error {
	directCSIClient := handler.directCSIClient.DirectV1beta2()

	klog.V(5).Infof("drive update called on %s", drive.Name)

	ownAndFormat := func(ctx context.Context, drive *directcsi.DirectCSIDrive) bool {
		return drive.Spec.DirectCSIOwned && drive.Spec.RequestedFormat != nil
	}

	driveUpdateType := func(ctx context.Context, drive *directcsi.DirectCSIDrive) DriveUpdateType {
		if ownAndFormat(ctx, drive) {
			return DriveUpdateTypeOwnAndFormat
		}
		return DriveUpdateTypeUnknown
	}

	isDuplicateUUID := func(ctx context.Context, driveName, fsUUID string) (bool, error) {
		driveList, err := directCSIClient.DirectCSIDrives().List(ctx, metav1.ListOptions{
			TypeMeta: utils.DirectCSIDriveTypeMeta(),
		})
		if err != nil {
			return false, err
		}
		drives := driveList.Items
		for _, drive := range drives {
			if drive.Status.NodeName != handler.nodeID || drive.Name == driveName {
				continue
			}
			if drive.Status.FilesystemUUID == fsUUID {
				return true, nil
			}
		}

		return false, nil
	}

	getFileSystemUUID := func() (string, error) {
		UUID := drive.Status.FilesystemUUID
		if UUID == "" {
			UUID = uuid.New().String()
		} else {
			// Check if the FilesystemUUID is already taken by other drives in the same node
			isDup, err := isDuplicateUUID(ctx, drive.Name, UUID)
			if err != nil {
				return "", err
			}
			if isDup {
				UUID = uuid.New().String()
			}
		}
		return UUID, nil
	}

	var updateErr error
	switch driveUpdateType(ctx, drive) {
	case DriveUpdateTypeOwnAndFormat:
		klog.V(3).Infof("owning and formatting drive %s", drive.Name)
		force := drive.Spec.RequestedFormat.Force
		mounted := drive.Status.Mountpoint != ""
		formatted := drive.Status.Filesystem != ""

		switch drive.Status.DriveStatus {
		case directcsi.DriveStatusReleased,
			directcsi.DriveStatusUnavailable,
			directcsi.DriveStatusReady,
			directcsi.DriveStatusTerminating,
			directcsi.DriveStatusInUse:
			klog.V(3).Infof("rejected request to format a %s drive: %s", string(drive.Status.DriveStatus), drive.Name)
			return nil
		case directcsi.DriveStatusAvailable:
			UUID, err := getFileSystemUUID()
			if err != nil {
				klog.Error(err)
				return err
			}
			drive.Status.FilesystemUUID = UUID
			directCSIPath := sys.GetDirectCSIPath(drive.Status.FilesystemUUID)
			directCSIMount := filepath.Join(sys.MountRoot, drive.Status.FilesystemUUID)
			if err := handler.formatter.MakeBlockFile(directCSIPath, drive.Status.MajorNumber, drive.Status.MinorNumber); err != nil {
				klog.Error(err)
				updateErr = err
			}

			source := directCSIPath
			target := directCSIMount
			mountOpts := drive.Spec.RequestedFormat.MountOptions
			if updateErr == nil {
				if !formatted || force {
					if mounted {
						if err := handler.mounter.UnmountDrive(source); err != nil {
							err = fmt.Errorf("failed to unmount drive: %s %v", drive.Name, err)
							klog.Error(err)
							updateErr = err
						} else {
							drive.Status.Mountpoint = ""
							mounted = false
						}
					}

					if updateErr == nil {
						if err := handler.formatter.FormatDrive(ctx, drive.Status.FilesystemUUID, source, force); err != nil {
							err = fmt.Errorf("failed to format drive: %s %v", drive.Name, err)
							klog.Error(err)
							updateErr = err
						} else {
							drive.Status.Filesystem = string(sys.FSTypeXFS)
							drive.Status.AllocatedCapacity = int64(0)
							formatted = true
						}
					}
				}
			}

			if updateErr == nil {
				if formatted && !mounted {
					if err := handler.mounter.MountDrive(source, target, mountOpts); err != nil {
						err = fmt.Errorf("failed to mount drive: %s %v", drive.Name, err)
						klog.Error(err)
						updateErr = err
					} else {
						drive.Status.Mountpoint = target
						drive.Status.MountOptions = mountOpts
						freeCapacity, sErr := handler.statter.GetFreeCapacityFromStatfs(drive.Status.Mountpoint)
						if sErr != nil {
							klog.Error(sErr)
							updateErr = sErr
						} else {
							mounted = true
							drive.Status.FreeCapacity = freeCapacity
							drive.Status.AllocatedCapacity = drive.Status.TotalCapacity - drive.Status.FreeCapacity
						}
					}
				}
			}

			utils.UpdateCondition(drive.Status.Conditions,
				string(directcsi.DirectCSIDriveConditionOwned),
				utils.BoolToCondition(formatted && mounted),
				string(directcsi.DirectCSIDriveReasonAdded),
				func() string {
					if updateErr != nil {
						return updateErr.Error()
					}
					return ""
				}(),
			)
			utils.UpdateCondition(drive.Status.Conditions,
				string(directcsi.DirectCSIDriveConditionMounted),
				utils.BoolToCondition(mounted),
				string(directcsi.DirectCSIDriveReasonAdded),
				func() string {
					if mounted {
						return string(directcsi.DirectCSIDriveMessageMounted)
					}
					return string(directcsi.DirectCSIDriveMessageNotMounted)
				}(),
			)
			utils.UpdateCondition(drive.Status.Conditions,
				string(directcsi.DirectCSIDriveConditionFormatted),
				utils.BoolToCondition(formatted),
				string(directcsi.DirectCSIDriveReasonAdded),
				func() string {
					if formatted {
						return string(directcsi.DirectCSIDriveMessageFormatted)
					}
					return string(directcsi.DirectCSIDriveMessageNotFormatted)
				}(),
			)

			if updateErr == nil {
				drive.Finalizers = []string{
					directcsi.DirectCSIDriveFinalizerDataProtection,
				}
				drive.Status.DriveStatus = directcsi.DriveStatusReady
				drive.Spec.RequestedFormat = nil
			}

			if drive, err = directCSIClient.DirectCSIDrives().Update(ctx, drive, metav1.UpdateOptions{
				TypeMeta: utils.DirectCSIDriveTypeMeta(),
			}); err != nil {
				return err
			}
			return updateErr
		}
	case DriveUpdateTypeUnknown:
		return fmt.Errorf("Unknown update type")
	}
	return nil
}

func (handler *DriveEventHandler) delete(ctx context.Context, drive *directcsi.DirectCSIDrive) error {
	directCSIClient := handler.directCSIClient.DirectV1beta2()
	if drive.Status.DriveStatus != directcsi.DriveStatusTerminating {
		drive.Status.DriveStatus = directcsi.DriveStatusTerminating
		if _, err := directCSIClient.DirectCSIDrives().Update(ctx, drive, metav1.UpdateOptions{
			TypeMeta: utils.DirectCSIDriveTypeMeta(),
		}); err != nil {
			return err
		}
	}

	finalizers := drive.GetFinalizers()
	if len(finalizers) == 0 {
		return nil
	}

	if len(finalizers) > 1 {
		return fmt.Errorf("cannot delete drive in use")
	}
	finalizer := finalizers[0]

	if finalizer != directcsi.DirectCSIDriveFinalizerDataProtection {
		return fmt.Errorf("invalid state reached. Report this issue at https://github.com/minio/direct-csi/issues")
	}

	if err := sys.SafeUnmount(filepath.Join(sys.MountRoot, drive.Name), nil); err != nil {
		return err
	}

	drive.Finalizers = []string{}
	_, err := directCSIClient.DirectCSIDrives().Update(
		ctx, drive, metav1.UpdateOptions{
			TypeMeta: utils.DirectCSIDriveTypeMeta(),
		},
	)
	return err
}

func StartController(ctx context.Context, nodeID string) error {
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	listener := listener2.NewListener(NewDriveEventHandler(nodeID), "drive-controller", hostname, 40)
	return listener.Run(ctx)
}
