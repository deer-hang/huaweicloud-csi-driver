package services

import (
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"k8s.io/apimachinery/pkg/util/wait"
	"strconv"
	"time"

	"github.com/chnsz/golangsdk"
	"github.com/chnsz/golangsdk/openstack/sfs_turbo/v1/shares"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	log "k8s.io/klog/v2"
)

const (
	shareCreating  = "100"
	shareAvailable = "200"

	shareSubExpanding     = "121"
	shareSubExpandSuccess = "221"
	shareSubExpandError   = "321"

	shareDescription = "provisioned-by=sfsturbo.csi.huaweicloud.org"
)

func CreateShareCompleted(client *golangsdk.ServiceClient, createOpts *shares.CreateOpts) (
	*shares.TurboResponse, error) {
	turboResponse, err := CreateShare(client, createOpts)
	if err != nil {
		return nil, err
	}
	log.V(4).Infof("[DEBUG] create share response detail: %v", protosanitizer.StripSecrets(turboResponse))
	err = WaitForShareAvailable(client, turboResponse.ID)
	if err != nil {
		return nil, err
	}
	return turboResponse, nil
}

const (
	DefaultInitDelay = 15 * time.Second
	DefaultFactor    = 1.02
	DefaultSteps     = 30
)

// WaitForShareAvailable create new share from sfs turbo.(it will cost few minutes)
func WaitForShareAvailable(client *golangsdk.ServiceClient, shareID string) error {
	condition := func() (bool, error) {
		share, err := GetShare(client, shareID)
		if err != nil {
			return false, status.Errorf(codes.Internal,
				"Failed to query share %s when wait share available: %v", shareID, err)
		}
		log.V(4).Infof("[DEBUG] WaitForShareAvailable query detail: %v", protosanitizer.StripSecrets(share))
		if share.Status == shareAvailable {
			return true, nil
		}
		if share.Status == shareCreating {
			return false, nil
		}
		return false, status.Error(codes.Internal, "created share status is not available")
	}
	backoff := wait.Backoff{
		Duration: DefaultInitDelay,
		Factor:   DefaultFactor,
		Steps:    DefaultSteps,
	}
	return wait.ExponentialBackoff(backoff, condition)
}

func CreateShare(client *golangsdk.ServiceClient, createOpts *shares.CreateOpts) (*shares.TurboResponse, error) {
	createOpts.Description = shareDescription
	share, err := shares.Create(client, createOpts).Extract()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create share, err: %v", err)
	}
	return share, nil
}

func DeleteShareCompleted(client *golangsdk.ServiceClient, shareID string) error {
	if _, err := GetShare(client, shareID); err != nil {
		if common.IsNotFound(err) {
			log.V(4).Infof("[DEBUG] share %s not found, assuming it to be already deleted", shareID)
			return nil
		}
		return status.Errorf(codes.Internal, "Failed to query volume %s, error: %v", shareID, err)
	}
	if err := DeleteShare(client, shareID); err != nil {
		return status.Errorf(codes.Internal, "Failed to delete share, err: %v", err)
	}
	return WaitForShareDeleted(client, shareID)
}

func DeleteShare(client *golangsdk.ServiceClient, shareID string) error {
	return shares.Delete(client, shareID).ExtractErr()

}

// WaitForShareDeleted delete share from sfs
func WaitForShareDeleted(client *golangsdk.ServiceClient, shareID string) error {
	return common.WaitForCompleted(func() (bool, error) {
		if _, err := GetShare(client, shareID); err != nil {
			if common.IsNotFound(err) {
				// resource not exist
				return true, nil
			}
			return false, status.Errorf(codes.Internal,
				"Failed to query share %s when wait share deleted: %v", shareID, err)
		}
		return false, nil
	})
}

func GetShare(client *golangsdk.ServiceClient, shareID string) (*shares.Turbo, error) {
	return shares.Get(client, shareID).Extract()
}

func ListTotalShares(client *golangsdk.ServiceClient) ([]shares.Turbo, error) {
	page, err := shares.List(client)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to query list total shares: %v", err)
	}
	return page, nil
}

func ListPageShares(client *golangsdk.ServiceClient, opts shares.ListOpts) (*shares.PagedList, error) {
	pageList, err := shares.ListPage(client, opts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to query list page shares: %v", err)
	}
	log.V(4).Infof("[DEBUG] Query list page shares detail: %v", pageList)
	return pageList, nil
}

func ExpandShareCompleted(client *golangsdk.ServiceClient, id string, newSize int) error {
	if err := ExpandShare(client, id, newSize); err != nil {
		return err
	}
	if err := WaitForShareExpanded(client, id); err != nil {
		return err
	}
	share, err := GetShare(client, id)
	if err != nil {
		return err
	}
	shareSize, err := strconv.ParseFloat(share.Size, 64)
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to convert string size to number size")
	}
	if int(shareSize) != newSize {
		return status.Errorf(codes.Internal, "Failed to expand share size, cause get an unexpected volume size")
	}
	return nil
}

func ExpandShare(client *golangsdk.ServiceClient, id string, newSize int) error {
	opt := shares.ExpandOpts{
		Extend: shares.ExtendOpts{
			NewSize: newSize,
		},
	}
	log.V(4).Infof("[DEBUG] Expand volume %s, and the options is %#v", id, opt)
	job, err := shares.Expand(client, id, opt).Extract()
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to expand volume: %s, err: %v", id, err)
	}
	log.V(4).Infof("[DEBUG] The volume expanding is submitted successfully and the job is running, job: %v", job)
	return nil
}

// WaitForShareExpanded delete share from sfs
func WaitForShareExpanded(client *golangsdk.ServiceClient, shareID string) error {
	return common.WaitForCompleted(func() (bool, error) {
		share, err := GetShare(client, shareID)
		if err != nil {
			return false, status.Errorf(codes.Internal,
				"Failed to query share %s when wait share expand: %v", shareID, err)
		}
		log.V(4).Infof("[DEBUG] query share detail: %v", protosanitizer.StripSecrets(share))
		if share.SubStatus == shareSubExpandSuccess {
			return true, nil
		}
		if share.SubStatus == shareSubExpanding {
			return false, nil
		}
		if share.SubStatus == shareSubExpandError {
			return false, status.Errorf(codes.Internal, "Failed to expand share, cause sub status error")
		}
		return false, status.Error(codes.Internal, "expand share sub status is not success")
	})
}
