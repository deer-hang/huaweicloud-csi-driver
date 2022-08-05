package evs

import (
	"fmt"
	"github.com/chnsz/golangsdk/openstack/evs/v2/snapshots"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/config"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/chnsz/golangsdk/openstack/evs/v2/cloudvolumes"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	log "k8s.io/klog"

	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/common"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/evs/services"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/utils"
)

type ControllerServer struct {
	Driver *EvsDriver
}

func (cs *ControllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse,
	error) {
	log.Infof("CreateVolume called with args %+v", protosanitizer.StripSecrets(*req))

	volumeName := req.GetName()
	err := createValidation(volumeName, req.GetVolumeCapabilities())
	if err != nil {
		return nil, err
	}

	// Define the parameters for creation
	volSizeBytes := int64(1 * common.GbByteSize)
	if req.GetCapacityRange() != nil {
		volSizeBytes = req.GetCapacityRange().GetRequiredBytes()
	}
	volSizeGB := int(utils.RoundUpSize(volSizeBytes, common.GbByteSize))

	parameters := req.GetParameters()
	volType := parameters["type"]
	dssID := parameters["dssId"]
	scsi := parameters["scsi"]

	shareable := false
	if parameters["shareable"] == "true" {
		shareable = true
	}

	volAvailability := parameters["availability"]
	if len(volAvailability) == 0 {
		// Check from Topology
		if req.GetAccessibilityRequirements() != nil {
			volAvailability = common.GetAZFromTopology(req.GetAccessibilityRequirements(), driverName)
			log.Infof("Get AZ By GetAccessibilityRequirements Availability Zone: %s", volAvailability)
		}
	}

	cc := cs.Driver.cloudCredentials
	// Verify that a volume with the provided name exists for this tenant
	volumes, err := services.ListVolumes(cc, cloudvolumes.ListOpts{
		Name: volumeName,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to query the volume list, "+
			"unable to verify whether the volume exists, error: %s", err))
	}

	if len(volumes) == 1 {
		if volSizeGB != volumes[0].Size {
			return nil, status.Error(codes.AlreadyExists, "Create failed, volume Already exists with same name, "+
				"but different capacity")
		}
		log.Errorf("Volume %s already exists in Availability Zone: %s of size %d GiB",
			volumes[0].ID, volumes[0].AvailabilityZone, volumes[0].Size)
		return buildCreateVolumeRsp(&volumes[0], dssID, req.GetAccessibilityRequirements()), nil
	} else if len(volumes) > 1 {
		return nil, status.Error(codes.AlreadyExists,
			"Create failed, found multiple existing volumes with same name")
	}

	// build the metadata of create option
	metadata := make(map[string]string)
	metadata[CsiClusterNodeIDKey] = cs.Driver.nodeID
	metadata[CreateForVolumeIDKey] = "true"
	metadata[DssIDKey] = dssID

	if scsi != "" && (scsi == "true" || scsi == "false") {
		metadata[HwPassthroughKey] = scsi
	}
	for _, mKey := range []string{PvcNameTag, PvcNsTag, PvNameKey} {
		if v, ok := parameters[mKey]; ok {
			metadata[mKey] = v
		}
	}

	snapshotID := ""
	content := req.GetVolumeContentSource()
	if content != nil && content.GetSnapshot() != nil {
		snapshotID = content.GetSnapshot().GetSnapshotId()
		_, err = services.GetSnapshot(cc, snapshotID)
		if err != nil {
			if common.IsNotFound(err) {
				return nil, status.Errorf(codes.NotFound, "The snapshot(id: %s) does not exist", snapshotID)
			}
			return nil, status.Errorf(codes.Internal, "Failed to retrieve the snapshot %s: %v", snapshotID, err)
		}
	}

	// Create volume
	createOpts := cloudvolumes.CreateOpts{
		Volume: cloudvolumes.VolumeOpts{
			Name:             volumeName,
			Size:             volSizeGB,
			VolumeType:       volType,
			AvailabilityZone: volAvailability,
			SnapshotID:       snapshotID,
			Metadata:         metadata,
			Multiattach:      shareable,
		},
	}
	log.Infof("Create EVS volume options: %#v", createOpts)
	volumeID, err := services.CreateVolumeToCompletion(cc, createOpts)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Create EVS volume failed with error %v", err))
	}

	volume, err := services.GetVolume(cc, volumeID)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to query volume detail by id %s: %v",
			volumeID, err))
	}

	log.Infof("CreateVolume: Successfully created volume %s in Availability Zone: %s of size %d GiB",
		volume.ID, volume.AvailabilityZone, volume.Size)
	return buildCreateVolumeRsp(volume, dssID, req.GetAccessibilityRequirements()), nil
}

func createValidation(volumeName string, volCapabilities []*csi.VolumeCapability) error {
	if len(volumeName) == 0 {
		log.Errorf("Volume capabilities cannot be empty")
		return status.Error(codes.InvalidArgument, "EVS volume name cannot be empty")
	}

	if volCapabilities == nil {
		log.Errorf("Volume capabilities cannot be empty")
		return status.Error(codes.InvalidArgument, "Volume capabilities cannot be empty")
	}

	return nil
}

func buildCreateVolumeRsp(vol *cloudvolumes.Volume, dssID string, accessibleTopologyReq *csi.TopologyRequirement) *csi.
	CreateVolumeResponse {
	var contentSource *csi.VolumeContentSource
	if vol.SnapshotID != "" {
		contentSource = &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: vol.SnapshotID,
				},
			},
		}
	}

	if vol.SourceVolID != "" {
		contentSource = &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: vol.SourceVolID,
				},
			},
		}
	}

	accessibleTopology := []*csi.Topology{
		{
			Segments: map[string]string{topologyKey: vol.AvailabilityZone},
		},
	}

	VolumeContext := make(map[string]string)
	if dssID != "" {
		VolumeContext[DssIDKey] = dssID
	}
	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           vol.ID,
			CapacityBytes:      int64(vol.Size * common.GbByteSize),
			AccessibleTopology: accessibleTopology,
			ContentSource:      contentSource,
			VolumeContext:      VolumeContext,
		},
	}
	return resp
}

func (cs *ControllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse,
	error) {
	log.Infof("DeleteVolume: called with args %+v", protosanitizer.StripSecrets(*req))
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume: Volume ID cannot be empty")
	}

	if err := services.DeleteVolume(cs.Driver.cloudCredentials, volumeID); err != nil {
		if common.IsNotFound(err) {
			log.Infof("Volume %s does not exist, skip deleting", volumeID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Failed to delete volume, id: %s, error: %v", volumeID, err))
	}
	log.Infof("DeleteVolume: Successfully deleted volume %s", volumeID)

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerGetVolume(_ context.Context, req *csi.ControllerGetVolumeRequest) (
	*csi.ControllerGetVolumeResponse, error) {
	klog.Infof("ControllerGetVolume: called with args %+v", protosanitizer.StripSecrets(*req))

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerGetVolume: Volume ID cannot be empty")
	}

	volume, err := services.GetVolume(cs.Driver.cloudCredentials, volumeID)
	if err != nil {
		if common.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "Volume %s not found", volumeID)
		}
		return nil, status.Error(codes.Internal, fmt.Sprintf("ControllerGetVolume failed with error %v", err))
	}

	volumeStatus := &csi.ControllerGetVolumeResponse_VolumeStatus{}
	for _, attachment := range volume.Attachments {
		volumeStatus.PublishedNodeIds = append(volumeStatus.PublishedNodeIds, attachment.ServerID)
	}

	return &csi.ControllerGetVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: int64(volume.Size * 1024 * 1024 * 1024),
		},
		Status: volumeStatus,
	}, nil
}

func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerUnpublishVolume(_ context.Context, req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ListVolumes(_ context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse,
	error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	klog.V(4).Infof("CreateSnapshot called with request %v", *req)
	credentials := cs.Driver.cloudCredentials
	name := req.GetName()
	volumeId := req.GetSourceVolumeId()
	if err := checkCreateSnapshotParamEmpty(name, volumeId); err != nil {
		return nil, err
	}
	if response, err := checkRepeatSnapshotName(credentials, name, volumeId); response != nil || err != nil {
		return response, err
	}
	if err := checkVolumeIsExist(credentials, volumeId); err != nil {
		return nil, err
	}
	return doSnapshotCreate(credentials, name, volumeId)
}

func checkCreateSnapshotParamEmpty(name string, volumeId string) error {
	if volumeId == "" {
		return status.Error(codes.InvalidArgument, "CreateSnapshot SourceVolumeId cannot be empty")
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "CreateSnapshot Name cannot be empty")
	}
	return nil
}

func checkRepeatSnapshotName(credentials *config.CloudCredentials, name string, volumeId string) (*csi.CreateSnapshotResponse, error) {
	listOpts := snapshots.ListOpts{
		Name: name,
	}
	listSnapshots, err := services.ListSnapshots(credentials, listOpts)
	if err != nil {
		klog.Errorf("Failed to query snapshot list: %v", err)
		return nil, status.Error(codes.Internal, "Failed to get snapshots")
	}
	if len(listSnapshots) == 1 {
		snap := &listSnapshots[0]
		if snap.VolumeID != volumeId {
			// same name with different volumeId
			return nil, status.Error(codes.AlreadyExists, "Snapshot with given name already exists, with different source volume ID")
		}
		klog.V(3).Infof("Found existing snapshot %s on %s", name, volumeId)
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SnapshotId:     snap.ID,
				SizeBytes:      int64(snap.Size * 1024 * 1024 * 1024),
				SourceVolumeId: snap.VolumeID,
				CreationTime:   timestamppb.New(snap.CreatedAt),
				ReadyToUse:     true,
			},
		}, nil
	}
	if len(listSnapshots) > 1 {
		// many repeat name
		klog.Errorf("Found multiple existing snapshots with selected name (%s) during create", name)
		return nil, status.Error(codes.Internal, "Multiple snapshots reported by Cinder with same name")
	}
	return nil, nil
}

func checkVolumeIsExist(credentials *config.CloudCredentials, volumeId string) error {
	volume, err := services.GetVolume(credentials, volumeId)
	if err != nil {
		klog.Errorf("Failed to query volume: %v", err)
		return err
	}
	if volume == nil {
		return status.Error(codes.Internal, "Volume id not exist")
	}
	return nil
}

func doSnapshotCreate(credentials *config.CloudCredentials, name string, volumeId string) (*csi.CreateSnapshotResponse, error) {
	opts := &snapshots.CreateOpts{
		VolumeID: volumeId,
		Name:     name,
	}
	snap, err := services.CreateSnapshot(credentials, opts)
	if err != nil {
		klog.Errorf("Failed to Create snapshot: %v", err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("CreateSnapshot failed with error %v", err))
	}
	klog.V(3).Infof("CreateSnapshot %s on %s", name, volumeId)

	err = services.WaitSnapshotReady(credentials, snap.ID)
	if err != nil {
		klog.Errorf("Failed to WaitSnapshotReady: %v", err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("CreateSnapshot failed with error %v", err))
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snap.ID,
			SizeBytes:      int64(snap.Size * 1024 * 1024 * 1024),
			SourceVolumeId: snap.VolumeID,
			CreationTime:   timestamppb.New(snap.CreatedAt),
			ReadyToUse:     true,
		},
	}, nil
}

func (cs *ControllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.V(4).Infof("DeleteSnapshot called with request %v", *req)
	credentials := cs.Driver.cloudCredentials
	id := req.GetSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID must be provided in DeleteSnapshot request")
	}

	if err := services.DeleteSnapshot(credentials, id); err != nil {
		if common.IsNotFound(err) {
			klog.V(3).Infof("Snapshot %s is already deleted.", id)
			return &csi.DeleteSnapshotResponse{}, nil
		}
		klog.Errorf("Failed to Delete snapshot: %v", err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("DeleteSnapshot failed with error %v", err))
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *ControllerServer) ListSnapshots(_ context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	klog.V(4).Infof("ListSnapshots called with request %v", *req)
	credentials := cs.Driver.cloudCredentials
	if snapshotId := req.GetSnapshotId(); snapshotId != "" {
		return querySnapshotBySnapshotId(credentials, snapshotId)
	}
	return querySnapshotPageList(credentials, req)
}

func querySnapshotBySnapshotId(credentials *config.CloudCredentials, id string) (*csi.ListSnapshotsResponse, error) {
	snapshot, err := services.GetSnapshot(credentials, id)
	if err != nil {
		if common.IsNotFound(err) {
			klog.V(3).Infof("Snapshot %s not found", id)
			return &csi.ListSnapshotsResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "Failed to GetSnapshot %s : %v", id, err)
	}

	snapshotEntry := csi.Snapshot{
		SizeBytes:      int64(snapshot.Size * 1024 * 1024 * 1024),
		SnapshotId:     snapshot.ID,
		SourceVolumeId: snapshot.VolumeID,
		CreationTime:   timestamppb.New(snapshot.CreatedAt),
		ReadyToUse:     true,
	}
	responseEntry := csi.ListSnapshotsResponse_Entry{
		Snapshot: &snapshotEntry,
	}
	return &csi.ListSnapshotsResponse{Entries: []*csi.ListSnapshotsResponse_Entry{&responseEntry}}, nil
}

func querySnapshotPageList(credentials *config.CloudCredentials, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	availableStatus := "available"
	opts := snapshots.ListOpts{
		VolumeID: req.GetSourceVolumeId(),
		Status:   availableStatus, // Only retrieve snapshots that are available
		Limit:    int(req.MaxEntries),
	}

	listSnapshots, err := services.List(credentials, opts)
	if err != nil {
		klog.Errorf("Failed to ListSnapshots: %v", err)
		return nil, status.Errorf(codes.Internal, "ListSnapshots failed with error %v", err)
	}

	var responses []*csi.ListSnapshotsResponse_Entry
	for _, element := range listSnapshots {
		snapshotElement := csi.Snapshot{
			SizeBytes:      int64(element.Size * 1024 * 1024 * 1024),
			SnapshotId:     element.ID,
			SourceVolumeId: element.VolumeID,
			CreationTime:   timestamppb.New(element.CreatedAt),
			ReadyToUse:     true,
		}
		responseEntryElement := csi.ListSnapshotsResponse_Entry{
			Snapshot: &snapshotElement,
		}
		responses = append(responses, &responseEntryElement)
	}

	return &csi.ListSnapshotsResponse{Entries: responses}, nil
}

// ControllerGetCapabilities implements the default GRPC callout.
// Default supports all capabilities
func (cs *ControllerServer) ControllerGetCapabilities(_ context.Context, req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.Driver.cscap,
	}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (
	*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}