// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/madmin-go"
	minioClient "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/replication"
	"github.com/minio/minio-go/v7/pkg/set"
	"github.com/minio/minio/internal/auth"
	sreplication "github.com/minio/minio/internal/bucket/replication"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/minio/internal/sync/errgroup"
	"github.com/minio/pkg/bucket/policy"
	bktpolicy "github.com/minio/pkg/bucket/policy"
	iampolicy "github.com/minio/pkg/iam/policy"
)

const (
	srStatePrefix = minioConfigPrefix + "/site-replication"
	srStateFile   = "state.json"
)

const (
	srStateFormatVersion1 = 1
)

var (
	errSRCannotJoin = SRError{
		Cause: errors.New("this site is already configured for site-replication"),
		Code:  ErrSiteReplicationInvalidRequest,
	}
	errSRDuplicateSites = SRError{
		Cause: errors.New("duplicate sites provided for site-replication"),
		Code:  ErrSiteReplicationInvalidRequest,
	}
	errSRSelfNotFound = SRError{
		Cause: errors.New("none of the given sites correspond to the current one"),
		Code:  ErrSiteReplicationInvalidRequest,
	}
	errSRPeerNotFound = SRError{
		Cause: errors.New("peer not found"),
		Code:  ErrSiteReplicationInvalidRequest,
	}
	errSRNotEnabled = SRError{
		Cause: errors.New("site replication is not enabled"),
		Code:  ErrSiteReplicationInvalidRequest,
	}
)

func errSRInvalidRequest(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationInvalidRequest,
	}
}

func errSRPeerResp(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationPeerResp,
	}
}

func errSRBackendIssue(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationBackendIssue,
	}
}

func errSRServiceAccount(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationServiceAccountError,
	}
}

func errSRBucketConfigError(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationBucketConfigError,
	}
}

func errSRBucketMetaError(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationBucketMetaError,
	}
}

func errSRIAMError(err error) SRError {
	return SRError{
		Cause: err,
		Code:  ErrSiteReplicationIAMError,
	}
}

var errSRObjectLayerNotReady = SRError{
	Cause: fmt.Errorf("object layer not ready"),
	Code:  ErrServerNotInitialized,
}

func getSRStateFilePath() string {
	return srStatePrefix + SlashSeparator + srStateFile
}

// SRError - wrapped error for site replication.
type SRError struct {
	Cause error
	Code  APIErrorCode
}

func (c SRError) Error() string {
	return c.Cause.Error()
}

func wrapSRErr(err error) SRError {
	return SRError{Cause: err, Code: ErrInternalError}
}

// SiteReplicationSys - manages cluster-level replication.
type SiteReplicationSys struct {
	sync.RWMutex

	enabled bool

	// In-memory and persisted multi-site replication state.
	state srState
}

type srState srStateV1

// srStateV1 represents version 1 of the site replication state persistence
// format.
type srStateV1 struct {
	Name string `json:"name"`

	// Peers maps peers by their deploymentID
	Peers                   map[string]madmin.PeerInfo `json:"peers"`
	ServiceAccountAccessKey string                     `json:"serviceAccountAccessKey"`
}

// srStateData represents the format of the current `srStateFile`.
type srStateData struct {
	Version int `json:"version"`

	SRState srStateV1 `json:"srState"`
}

// Init - initialize the site replication manager.
func (c *SiteReplicationSys) Init(ctx context.Context, objAPI ObjectLayer) error {
	err := c.loadFromDisk(ctx, objAPI)
	if err == errConfigNotFound {
		return nil
	}

	c.RLock()
	defer c.RUnlock()
	if c.enabled {
		logger.Info("Cluster Replication initialized.")
	}

	return err
}

func (c *SiteReplicationSys) loadFromDisk(ctx context.Context, objAPI ObjectLayer) error {
	buf, err := readConfig(ctx, objAPI, getSRStateFilePath())
	if err != nil {
		return err
	}

	// attempt to read just the version key in the state file to ensure we
	// are reading a compatible version.
	var ver struct {
		Version int `json:"version"`
	}
	err = json.Unmarshal(buf, &ver)
	if err != nil {
		return err
	}
	if ver.Version != srStateFormatVersion1 {
		return fmt.Errorf("Unexpected ClusterRepl state version: %d", ver.Version)
	}

	var sdata srStateData
	err = json.Unmarshal(buf, &sdata)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()
	c.state = srState(sdata.SRState)
	c.enabled = true
	return nil
}

func (c *SiteReplicationSys) saveToDisk(ctx context.Context, state srState) error {
	sdata := srStateData{
		Version: srStateFormatVersion1,
		SRState: srStateV1(state),
	}
	buf, err := json.Marshal(sdata)
	if err != nil {
		return err
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		return errServerNotInitialized
	}
	err = saveConfig(ctx, objAPI, getSRStateFilePath(), buf)
	if err != nil {
		return err
	}

	for _, e := range globalNotificationSys.ReloadSiteReplicationConfig(ctx) {
		logger.LogIf(ctx, e)
	}

	c.Lock()
	defer c.Unlock()
	c.state = state
	c.enabled = true
	return nil
}

const (
	// Access key of service account used for perform cluster-replication
	// operations.
	siteReplicatorSvcAcc = "site-replicator-0"
)

// PeerSiteInfo is a wrapper struct around madmin.PeerSite with extra info on site status
type PeerSiteInfo struct {
	madmin.PeerSite
	self         bool
	DeploymentID string
	Replicated   bool // true if already participating in site replication
	Empty        bool // true if cluster has no buckets
}

// getSiteStatuses gathers more info on the sites being added
func (c *SiteReplicationSys) getSiteStatuses(ctx context.Context, sites []madmin.PeerSite) (psi []PeerSiteInfo, err SRError) {
	for _, v := range sites {
		admClient, err := getAdminClient(v.Endpoint, v.AccessKey, v.SecretKey)
		if err != nil {
			return psi, errSRPeerResp(fmt.Errorf("unable to create admin client for %s: %w", v.Name, err))
		}
		info, err := admClient.ServerInfo(ctx)
		if err != nil {
			return psi, errSRPeerResp(fmt.Errorf("unable to fetch server info for %s: %w", v.Name, err))
		}

		deploymentID := info.DeploymentID
		pi := PeerSiteInfo{
			PeerSite:     v,
			DeploymentID: deploymentID,
			Empty:        true,
		}

		if deploymentID == globalDeploymentID {
			objAPI := newObjectLayerFn()
			if objAPI == nil {
				return psi, errSRObjectLayerNotReady
			}
			res, err := objAPI.ListBuckets(ctx)
			if err != nil {
				return psi, errSRBackendIssue(err)
			}
			if len(res) > 0 {
				if len(res) != 1 || (res[0].Name != "root") {
					pi.Empty = false
				}
			}
			pi.self = true
		} else {
			s3Client, err := getS3Client(v)
			if err != nil {
				return psi, errSRPeerResp(fmt.Errorf("unable to create s3 client for %s: %w", v.Name, err))
			}
			buckets, err := s3Client.ListBuckets(ctx)
			if err != nil {
				return psi, errSRPeerResp(fmt.Errorf("unable to list buckets for %s: %v", v.Name, err))
			}
			if len(buckets) > 0 {
				if len(buckets) != 1 || (buckets[0].Name != "root") {
					logger.Info("peer has buckets: ", buckets)
					pi.Empty = false
				}
			}
		}
		psi = append(psi, pi)
	}
	return
}

// AddPeerClusters - add cluster sites for replication configuration.
func (c *SiteReplicationSys) AddPeerClusters(ctx context.Context, psites []madmin.PeerSite) (madmin.ReplicateAddStatus, error) {
	sites, serr := c.getSiteStatuses(ctx, psites)
	if serr.Cause != nil {
		return madmin.ReplicateAddStatus{}, serr
	}
	var (
		currSites            madmin.SiteReplicationInfo
		currDeploymentIDsSet = set.NewStringSet()
		err                  error
	)
	if c.enabled {
		currSites, err = c.GetClusterInfo(ctx)
		if err != nil {
			return madmin.ReplicateAddStatus{}, errSRBackendIssue(err)
		}
		for _, v := range currSites.Sites {
			currDeploymentIDsSet.Add(v.DeploymentID)
		}
	}
	deploymentIDsSet := set.NewStringSet()
	localHasBuckets := false
	nonLocalPeerWithBuckets := ""
	selfIdx := -1
	for i, v := range sites {
		// deploymentIDs must be unique
		if deploymentIDsSet.Contains(v.DeploymentID) {
			return madmin.ReplicateAddStatus{}, errSRDuplicateSites
		}
		deploymentIDsSet.Add(v.DeploymentID)

		if v.self {
			selfIdx = i
			localHasBuckets = !v.Empty
			continue
		}
		if !v.Empty && !currDeploymentIDsSet.Contains(v.DeploymentID) {
			nonLocalPeerWithBuckets = v.Name
		}
	}
	if c.enabled {
		// If current cluster is already SR enabled and no new site being added ,fail.
		if currDeploymentIDsSet.Equals(deploymentIDsSet) {
			return madmin.ReplicateAddStatus{}, errSRCannotJoin
		}
		if len(currDeploymentIDsSet.Intersection(deploymentIDsSet)) != len(currDeploymentIDsSet) {
			diffSlc := getMissingSiteNames(currDeploymentIDsSet, deploymentIDsSet, currSites.Sites)
			return madmin.ReplicateAddStatus{}, errSRInvalidRequest(fmt.Errorf("all existing replicated sites must be specified - missing %s", strings.Join(diffSlc, " ")))
		}
	}
	// For this `add` API, either all clusters must be empty or the local
	// cluster must be the only one having some buckets.

	if localHasBuckets && nonLocalPeerWithBuckets != "" {
		return madmin.ReplicateAddStatus{}, errSRInvalidRequest(errors.New("only one cluster may have data when configuring site replication"))
	}

	if !localHasBuckets && nonLocalPeerWithBuckets != "" {
		return madmin.ReplicateAddStatus{}, errSRInvalidRequest(fmt.Errorf("please send your request to the cluster containing data/buckets: %s", nonLocalPeerWithBuckets))
	}

	// validate that all clusters are using the same (LDAP based)
	// external IDP.
	pass, err := c.validateIDPSettings(ctx, sites)
	if err != nil {
		return madmin.ReplicateAddStatus{}, err
	}
	if !pass {
		return madmin.ReplicateAddStatus{}, errSRInvalidRequest(errors.New("all cluster sites must have the same (LDAP) IDP settings"))
	}

	// FIXME: Ideally, we also need to check if there are any global IAM
	// policies and any (LDAP user created) service accounts on the other
	// peer clusters, and if so, reject the cluster replicate add request.
	// This is not yet implemented.

	// VALIDATIONS COMPLETE.

	// Create a common service account for all clusters, with root
	// permissions.

	// Create a local service account.

	// Generate a secret key for the service account if not created already.
	var secretKey string
	svcCred, _, err := globalIAMSys.getServiceAccount(ctx, siteReplicatorSvcAcc)
	switch {
	case err == errNoSuchServiceAccount:
		_, secretKey, err = auth.GenerateCredentials()
		if err != nil {
			return madmin.ReplicateAddStatus{}, errSRServiceAccount(fmt.Errorf("unable to create local service account: %w", err))
		}
		svcCred, err = globalIAMSys.NewServiceAccount(ctx, sites[selfIdx].AccessKey, nil, newServiceAccountOpts{
			accessKey: siteReplicatorSvcAcc,
			secretKey: secretKey,
		})
		if err != nil {
			return madmin.ReplicateAddStatus{}, errSRServiceAccount(fmt.Errorf("unable to create local service account: %w", err))
		}
	case err == nil:
		secretKey = svcCred.SecretKey
	default:
		return madmin.ReplicateAddStatus{}, errSRBackendIssue(err)
	}

	joinReq := madmin.SRPeerJoinReq{
		SvcAcctAccessKey: svcCred.AccessKey,
		SvcAcctSecretKey: secretKey,
		Peers:            make(map[string]madmin.PeerInfo),
	}

	for _, v := range sites {
		joinReq.Peers[v.DeploymentID] = madmin.PeerInfo{
			Endpoint:     v.Endpoint,
			Name:         v.Name,
			DeploymentID: v.DeploymentID,
		}
	}

	addedCount := 0
	var (
		peerAddErr error
		admClient  *madmin.AdminClient
	)

	for _, v := range sites {
		if v.self {
			continue
		}
		switch {
		case currDeploymentIDsSet.Contains(v.DeploymentID):
			admClient, err = c.getAdminClient(ctx, v.DeploymentID)
		default:
			admClient, err = getAdminClient(v.Endpoint, v.AccessKey, v.SecretKey)
		}
		if err != nil {
			peerAddErr = errSRPeerResp(fmt.Errorf("unable to create admin client for %s: %w", v.Name, err))
			break
		}
		joinReq.SvcAcctParent = v.AccessKey
		err = admClient.SRPeerJoin(ctx, joinReq)
		if err != nil {
			peerAddErr = errSRPeerResp(fmt.Errorf("unable to link with peer %s: %w", v.Name, err))
			break
		}
		addedCount++
	}

	if peerAddErr != nil {
		if addedCount == 0 {
			return madmin.ReplicateAddStatus{}, peerAddErr
		}
		// In this case, it means at least one cluster was added
		// successfully, we need to send a response to the client with
		// some details - FIXME: the disks on this cluster would need to
		// be cleaned to recover.
		partial := madmin.ReplicateAddStatus{
			Status:    madmin.ReplicateAddStatusPartial,
			ErrDetail: peerAddErr.Error(),
		}

		return partial, nil
	}

	// Other than handling existing buckets, we can now save the cluster
	// replication configuration state.
	state := srState{
		Name:                    sites[selfIdx].Name,
		Peers:                   joinReq.Peers,
		ServiceAccountAccessKey: svcCred.AccessKey,
	}

	if err = c.saveToDisk(ctx, state); err != nil {
		return madmin.ReplicateAddStatus{
			Status:    madmin.ReplicateAddStatusPartial,
			ErrDetail: fmt.Sprintf("unable to save cluster-replication state on local: %v", err),
		}, nil
	}

	result := madmin.ReplicateAddStatus{
		Success: true,
		Status:  madmin.ReplicateAddStatusSuccess,
	}

	if err := c.syncLocalToPeers(ctx); err != nil {
		result.InitialSyncErrorMessage = err.Error()
	}

	return result, nil
}

// PeerJoinReq - internal API handler to respond to a peer cluster's request
// to join.
func (c *SiteReplicationSys) PeerJoinReq(ctx context.Context, arg madmin.SRPeerJoinReq) error {
	var ourName string
	for d, p := range arg.Peers {
		if d == globalDeploymentID {
			ourName = p.Name
			break
		}
	}
	if ourName == "" {
		return errSRSelfNotFound
	}

	_, _, err := globalIAMSys.GetServiceAccount(ctx, arg.SvcAcctAccessKey)
	if err == errNoSuchServiceAccount {
		_, err = globalIAMSys.NewServiceAccount(ctx, arg.SvcAcctParent, nil, newServiceAccountOpts{
			accessKey: arg.SvcAcctAccessKey,
			secretKey: arg.SvcAcctSecretKey,
		})
	}
	if err != nil {
		return errSRServiceAccount(fmt.Errorf("unable to create service account on %s: %v", ourName, err))
	}

	state := srState{
		Name:                    ourName,
		Peers:                   arg.Peers,
		ServiceAccountAccessKey: arg.SvcAcctAccessKey,
	}
	if err = c.saveToDisk(ctx, state); err != nil {
		return errSRBackendIssue(fmt.Errorf("unable to save cluster-replication state to disk on %s: %v", ourName, err))
	}
	return nil
}

// GetIDPSettings returns info about the configured identity provider. It is
// used to validate that all peers have the same IDP.
func (c *SiteReplicationSys) GetIDPSettings(ctx context.Context) madmin.IDPSettings {
	s := madmin.IDPSettings{}
	s.LDAP = madmin.LDAPSettings{
		IsLDAPEnabled:          globalLDAPConfig.Enabled,
		LDAPUserDNSearchBase:   globalLDAPConfig.UserDNSearchBaseDN,
		LDAPUserDNSearchFilter: globalLDAPConfig.UserDNSearchFilter,
		LDAPGroupSearchBase:    globalLDAPConfig.GroupSearchBaseDistName,
		LDAPGroupSearchFilter:  globalLDAPConfig.GroupSearchFilter,
	}
	s.OpenID = globalOpenIDConfig.GetSettings()
	if s.OpenID.Enabled {
		s.OpenID.Region = globalSite.Region
	}
	return s
}

func (c *SiteReplicationSys) validateIDPSettings(ctx context.Context, peers []PeerSiteInfo) (bool, error) {
	s := make([]madmin.IDPSettings, 0, len(peers))
	for _, v := range peers {
		if v.self {
			s = append(s, c.GetIDPSettings(ctx))
			continue
		}

		admClient, err := getAdminClient(v.Endpoint, v.AccessKey, v.SecretKey)
		if err != nil {
			return false, errSRPeerResp(fmt.Errorf("unable to create admin client for %s: %w", v.Name, err))
		}

		is, err := admClient.SRPeerGetIDPSettings(ctx)
		if err != nil {
			return false, errSRPeerResp(fmt.Errorf("unable to fetch IDP settings from %s: %v", v.Name, err))
		}
		s = append(s, is)
	}

	for i := 1; i < len(s); i++ {
		if !reflect.DeepEqual(s[i], s[0]) {
			return false, nil
		}
	}
	return true, nil
}

// GetClusterInfo - returns site replication information.
func (c *SiteReplicationSys) GetClusterInfo(ctx context.Context) (info madmin.SiteReplicationInfo, err error) {
	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return info, nil
	}

	info.Enabled = true
	info.Name = c.state.Name
	info.Sites = make([]madmin.PeerInfo, 0, len(c.state.Peers))
	for _, peer := range c.state.Peers {
		info.Sites = append(info.Sites, peer)
	}
	sort.SliceStable(info.Sites, func(i, j int) bool {
		return info.Sites[i].Name < info.Sites[j].Name
	})

	info.ServiceAccountAccessKey = c.state.ServiceAccountAccessKey
	return info, nil
}

// MakeBucketHook - called during a regular make bucket call when cluster
// replication is enabled. It is responsible for the creation of the same bucket
// on remote clusters, and creating replication rules on local and peer
// clusters.
func (c *SiteReplicationSys) MakeBucketHook(ctx context.Context, bucket string, opts BucketOptions) error {
	// At this point, the local bucket is created.

	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return nil
	}

	optsMap := make(map[string]string)
	if opts.Location != "" {
		optsMap["location"] = opts.Location
	}
	if opts.LockEnabled {
		optsMap["lockEnabled"] = "true"
		optsMap["versioningEnabled"] = "true"
	}
	if opts.VersioningEnabled {
		optsMap["versioningEnabled"] = "true"
	}

	// Create bucket and enable versioning on all peers.
	makeBucketConcErr := c.concDo(
		func() error {
			err := c.PeerBucketMakeWithVersioningHandler(ctx, bucket, opts)
			logger.LogIf(ctx, c.annotateErr("MakeWithVersioning", err))
			return err
		},
		func(deploymentID string, p madmin.PeerInfo) error {
			admClient, err := c.getAdminClient(ctx, deploymentID)
			if err != nil {
				return err
			}

			err = admClient.SRPeerBucketOps(ctx, bucket, madmin.MakeWithVersioningBktOp, optsMap)
			logger.LogIf(ctx, c.annotatePeerErr(p.Name, "MakeWithVersioning", err))
			return err
		},
	)
	// If all make-bucket-and-enable-versioning operations failed, nothing
	// more to do.
	if makeBucketConcErr.allFailed() {
		return makeBucketConcErr
	}

	// Log any errors in make-bucket operations.
	logger.LogIf(ctx, makeBucketConcErr.summaryErr)

	// Create bucket remotes and add replication rules for the bucket on
	// self and peers.
	makeRemotesConcErr := c.concDo(
		func() error {
			err := c.PeerBucketConfigureReplHandler(ctx, bucket)
			logger.LogIf(ctx, c.annotateErr("ConfigureRepl", err))
			return err
		},
		func(deploymentID string, p madmin.PeerInfo) error {
			admClient, err := c.getAdminClient(ctx, deploymentID)
			if err != nil {
				return err
			}

			err = admClient.SRPeerBucketOps(ctx, bucket, madmin.ConfigureReplBktOp, nil)
			logger.LogIf(ctx, c.annotatePeerErr(p.Name, "ConfigureRepl", err))
			return err
		},
	)
	err := makeRemotesConcErr.summaryErr
	if err != nil {
		return err
	}

	return nil
}

// DeleteBucketHook - called during a regular delete bucket call when cluster
// replication is enabled. It is responsible for the deletion of the same bucket
// on remote clusters.
func (c *SiteReplicationSys) DeleteBucketHook(ctx context.Context, bucket string, forceDelete bool) error {
	// At this point, the local bucket is deleted.

	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return nil
	}

	op := madmin.DeleteBucketBktOp
	if forceDelete {
		op = madmin.ForceDeleteBucketBktOp
	}

	// Send bucket delete to other clusters.
	cErr := c.concDo(nil, func(deploymentID string, p madmin.PeerInfo) error {
		admClient, err := c.getAdminClient(ctx, deploymentID)
		if err != nil {
			return wrapSRErr(err)
		}

		err = admClient.SRPeerBucketOps(ctx, bucket, op, nil)
		logger.LogIf(ctx, c.annotatePeerErr(p.Name, "DeleteBucket", err))
		return err
	})
	return cErr.summaryErr
}

// PeerBucketMakeWithVersioningHandler - creates bucket and enables versioning.
func (c *SiteReplicationSys) PeerBucketMakeWithVersioningHandler(ctx context.Context, bucket string, opts BucketOptions) error {
	objAPI := newObjectLayerFn()
	if objAPI == nil {
		return errServerNotInitialized
	}

	err := objAPI.MakeBucketWithLocation(ctx, bucket, opts)
	if err != nil {
		// Check if this is a bucket exists error.
		_, ok1 := err.(BucketExists)
		_, ok2 := err.(BucketAlreadyExists)
		if !ok1 && !ok2 {
			logger.LogIf(ctx, c.annotateErr("MakeBucketErr on peer call", err))
			return wrapSRErr(err)
		}
	} else {
		// Load updated bucket metadata into memory as new
		// bucket was created.
		globalNotificationSys.LoadBucketMetadata(GlobalContext, bucket)
	}

	meta, err := globalBucketMetadataSys.Get(bucket)
	if err != nil && err != errConfigNotFound {
		logger.LogIf(ctx, c.annotateErr("MakeBucketErr on peer call", err))
		return wrapSRErr(err)
	}

	meta.VersioningConfigXML = enabledBucketVersioningConfig
	if opts.LockEnabled {
		meta.ObjectLockConfigXML = enabledBucketObjectLockConfig
	}

	if err := meta.Save(context.Background(), objAPI); err != nil {
		return wrapSRErr(err)
	}

	globalBucketMetadataSys.Set(bucket, meta)

	// Load updated bucket metadata into memory as new metadata updated.
	globalNotificationSys.LoadBucketMetadata(GlobalContext, bucket)
	return nil
}

// PeerBucketConfigureReplHandler - configures replication remote and
// replication rules to all other peers for the local bucket.
func (c *SiteReplicationSys) PeerBucketConfigureReplHandler(ctx context.Context, bucket string) error {
	creds, err := c.getPeerCreds()
	if err != nil {
		return wrapSRErr(err)
	}

	// The following function, creates a bucket remote and sets up a bucket
	// replication rule for the given peer.
	configurePeerFn := func(d string, peer madmin.PeerInfo) error {
		ep, _ := url.Parse(peer.Endpoint)
		targets := globalBucketTargetSys.ListTargets(ctx, bucket, string(madmin.ReplicationService))
		targetARN := ""
		for _, target := range targets {
			if target.SourceBucket == bucket &&
				target.TargetBucket == bucket &&
				target.Endpoint == ep.Host &&
				target.Secure == (ep.Scheme == "https") &&
				target.Type == madmin.ReplicationService {
				targetARN = target.Arn
				break
			}
		}
		if targetARN == "" {
			bucketTarget := madmin.BucketTarget{
				SourceBucket: bucket,
				Endpoint:     ep.Host,
				Credentials: &madmin.Credentials{
					AccessKey: creds.AccessKey,
					SecretKey: creds.SecretKey,
				},
				TargetBucket:    bucket,
				Secure:          ep.Scheme == "https",
				API:             "s3v4",
				Type:            madmin.ReplicationService,
				Region:          "",
				ReplicationSync: false,
			}
			bucketTarget.Arn = globalBucketTargetSys.getRemoteARN(bucket, &bucketTarget)
			err := globalBucketTargetSys.SetTarget(ctx, bucket, &bucketTarget, false)
			if err != nil {
				logger.LogIf(ctx, c.annotatePeerErr(peer.Name, "Bucket target creation error", err))
				return err
			}
			targets, err := globalBucketTargetSys.ListBucketTargets(ctx, bucket)
			if err != nil {
				return err
			}
			tgtBytes, err := json.Marshal(&targets)
			if err != nil {
				return err
			}
			if err = globalBucketMetadataSys.Update(bucket, bucketTargetsFile, tgtBytes); err != nil {
				return err
			}
			targetARN = bucketTarget.Arn
		}

		// Create bucket replication rule to this peer.

		// To add the bucket replication rule, we fetch the current
		// server configuration, and convert it to minio-go's
		// replication configuration type (by converting to xml and
		// parsing it back), use minio-go's add rule function, and
		// finally convert it back to the server type (again via xml).
		// This is needed as there is no add-rule function in the server
		// yet.

		// Though we do not check if the rule already exists, this is
		// not a problem as we are always using the same replication
		// rule ID - if the rule already exists, it is just replaced.
		replicationConfigS, err := globalBucketMetadataSys.GetReplicationConfig(ctx, bucket)
		if err != nil {
			_, ok := err.(BucketReplicationConfigNotFound)
			if !ok {
				return err
			}
		}
		var replicationConfig replication.Config
		if replicationConfigS != nil {
			replCfgSBytes, err := xml.Marshal(replicationConfigS)
			if err != nil {
				return err
			}
			err = xml.Unmarshal(replCfgSBytes, &replicationConfig)
			if err != nil {
				return err
			}
		}
		var (
			ruleID  = fmt.Sprintf("site-repl-%s", d)
			hasRule bool
			opts    = replication.Options{
				// Set the ID so we can identify the rule as being
				// created for site-replication and include the
				// destination cluster's deployment ID.
				ID: ruleID,

				// Use a helper to generate unique priority numbers.
				Priority: fmt.Sprintf("%d", getPriorityHelper(replicationConfig)),

				Op:         replication.AddOption,
				RuleStatus: "enable",
				DestBucket: targetARN,

				// Replicate everything!
				ReplicateDeletes:        "enable",
				ReplicateDeleteMarkers:  "enable",
				ReplicaSync:             "enable",
				ExistingObjectReplicate: "enable",
			}
		)
		for _, r := range replicationConfig.Rules {
			if r.ID == ruleID {
				hasRule = true
			}
		}
		switch {
		case hasRule:
			err = replicationConfig.EditRule(opts)
		default:
			err = replicationConfig.AddRule(opts)
		}

		if err != nil {
			logger.LogIf(ctx, c.annotatePeerErr(peer.Name, "Error adding bucket replication rule", err))
			return err
		}
		// Now convert the configuration back to server's type so we can
		// do some validation.
		newReplCfgBytes, err := xml.Marshal(replicationConfig)
		if err != nil {
			return err
		}
		newReplicationConfig, err := sreplication.ParseConfig(bytes.NewReader(newReplCfgBytes))
		if err != nil {
			return err
		}
		sameTarget, apiErr := validateReplicationDestination(ctx, bucket, newReplicationConfig)
		if apiErr != noError {
			return fmt.Errorf("bucket replication config validation error: %#v", apiErr)
		}
		err = newReplicationConfig.Validate(bucket, sameTarget)
		if err != nil {
			return err
		}
		// Config looks good, so we save it.
		replCfgData, err := xml.Marshal(newReplicationConfig)
		if err != nil {
			return err
		}
		err = globalBucketMetadataSys.Update(bucket, bucketReplicationConfig, replCfgData)
		logger.LogIf(ctx, c.annotatePeerErr(peer.Name, "Error updating replication configuration", err))
		return err
	}

	errMap := make(map[string]error, len(c.state.Peers))
	for d, peer := range c.state.Peers {
		if d == globalDeploymentID {
			continue
		}
		if err := configurePeerFn(d, peer); err != nil {
			errMap[d] = err
		}
	}
	return c.toErrorFromErrMap(errMap)
}

// PeerBucketDeleteHandler - deletes bucket on local in response to a delete
// bucket request from a peer.
func (c *SiteReplicationSys) PeerBucketDeleteHandler(ctx context.Context, bucket string, forceDelete bool) error {
	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return errSRNotEnabled
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		return errServerNotInitialized
	}

	if globalDNSConfig != nil {
		if err := globalDNSConfig.Delete(bucket); err != nil {
			return err
		}
	}

	err := objAPI.DeleteBucket(ctx, bucket, DeleteBucketOptions{Force: forceDelete})
	if err != nil {
		if globalDNSConfig != nil {
			if err2 := globalDNSConfig.Put(bucket); err2 != nil {
				logger.LogIf(ctx, fmt.Errorf("Unable to restore bucket DNS entry %w, please fix it manually", err2))
			}
		}
		return err
	}

	globalNotificationSys.DeleteBucketMetadata(ctx, bucket)

	return nil
}

// IAMChangeHook - called when IAM items need to be replicated to peer clusters.
// This includes named policy creation, policy mapping changes and service
// account changes.
//
// All policies are replicated.
//
// Policy mappings are only replicated when they are for LDAP users or groups
// (as an external IDP is always assumed when SR is used). In the case of
// OpenID, such mappings are provided from the IDP directly and so are not
// applicable here.
//
// Service accounts are replicated as long as they are not meant for the root
// user.
//
// STS accounts are replicated, but only if the session token is verifiable
// using the local cluster's root credential.
func (c *SiteReplicationSys) IAMChangeHook(ctx context.Context, item madmin.SRIAMItem) error {
	// The IAM item has already been applied to the local cluster at this
	// point, and only needs to be updated on all remote peer clusters.

	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return nil
	}

	cErr := c.concDo(nil, func(d string, p madmin.PeerInfo) error {
		admClient, err := c.getAdminClient(ctx, d)
		if err != nil {
			return wrapSRErr(err)
		}

		err = admClient.SRPeerReplicateIAMItem(ctx, item)
		logger.LogIf(ctx, c.annotatePeerErr(p.Name, "SRPeerReplicateIAMItem", err))
		return err
	})
	return cErr.summaryErr
}

// PeerAddPolicyHandler - copies IAM policy to local. A nil policy argument,
// causes the named policy to be deleted.
func (c *SiteReplicationSys) PeerAddPolicyHandler(ctx context.Context, policyName string, p *iampolicy.Policy) error {
	var err error
	if p == nil {
		err = globalIAMSys.DeletePolicy(ctx, policyName, true)
	} else {
		err = globalIAMSys.SetPolicy(ctx, policyName, *p)
	}
	if err != nil {
		return wrapSRErr(err)
	}
	return nil
}

// PeerIAMUserChangeHandler - copies IAM user to local.
func (c *SiteReplicationSys) PeerIAMUserChangeHandler(ctx context.Context, change *madmin.SRIAMUser) error {
	if change == nil {
		return errSRInvalidRequest(errInvalidArgument)
	}
	var err error
	if change.IsDeleteReq {
		err = globalIAMSys.DeleteUser(ctx, change.AccessKey, true)
	} else {
		if change.UserReq == nil {
			return errSRInvalidRequest(errInvalidArgument)
		}
		err = globalIAMSys.CreateUser(ctx, change.AccessKey, *change.UserReq)
	}
	if err != nil {
		return wrapSRErr(err)
	}
	return nil
}

// PeerGroupInfoChangeHandler - copies group changes to local.
func (c *SiteReplicationSys) PeerGroupInfoChangeHandler(ctx context.Context, change *madmin.SRGroupInfo) error {
	if change == nil {
		return errSRInvalidRequest(errInvalidArgument)
	}
	updReq := change.UpdateReq
	var err error
	if updReq.IsRemove {
		err = globalIAMSys.RemoveUsersFromGroup(ctx, updReq.Group, updReq.Members)
	} else {
		err = globalIAMSys.AddUsersToGroup(ctx, updReq.Group, updReq.Members)
	}
	if err != nil {
		return wrapSRErr(err)
	}
	return nil
}

// PeerSvcAccChangeHandler - copies service-account change to local.
func (c *SiteReplicationSys) PeerSvcAccChangeHandler(ctx context.Context, change *madmin.SRSvcAccChange) error {
	if change == nil {
		return errSRInvalidRequest(errInvalidArgument)
	}
	switch {
	case change.Create != nil:
		var sp *iampolicy.Policy
		var err error
		if len(change.Create.SessionPolicy) > 0 {
			sp, err = iampolicy.ParseConfig(bytes.NewReader(change.Create.SessionPolicy))
			if err != nil {
				return wrapSRErr(err)
			}
		}

		opts := newServiceAccountOpts{
			accessKey:     change.Create.AccessKey,
			secretKey:     change.Create.SecretKey,
			sessionPolicy: sp,
			claims:        change.Create.Claims,
		}
		_, err = globalIAMSys.NewServiceAccount(ctx, change.Create.Parent, change.Create.Groups, opts)
		if err != nil {
			return wrapSRErr(err)
		}

	case change.Update != nil:
		var sp *iampolicy.Policy
		var err error
		if len(change.Update.SessionPolicy) > 0 {
			sp, err = iampolicy.ParseConfig(bytes.NewReader(change.Update.SessionPolicy))
			if err != nil {
				return wrapSRErr(err)
			}
		}
		opts := updateServiceAccountOpts{
			secretKey:     change.Update.SecretKey,
			status:        change.Update.Status,
			sessionPolicy: sp,
		}

		err = globalIAMSys.UpdateServiceAccount(ctx, change.Update.AccessKey, opts)
		if err != nil {
			return wrapSRErr(err)
		}

	case change.Delete != nil:
		err := globalIAMSys.DeleteServiceAccount(ctx, change.Delete.AccessKey, true)
		if err != nil {
			return wrapSRErr(err)
		}

	}

	return nil
}

// PeerPolicyMappingHandler - copies policy mapping to local.
func (c *SiteReplicationSys) PeerPolicyMappingHandler(ctx context.Context, mapping *madmin.SRPolicyMapping) error {
	if mapping == nil {
		return errSRInvalidRequest(errInvalidArgument)
	}
	err := globalIAMSys.PolicyDBSet(ctx, mapping.UserOrGroup, mapping.Policy, mapping.IsGroup)
	if err != nil {
		return wrapSRErr(err)
	}
	return nil
}

// PeerSTSAccHandler - replicates STS credential locally.
func (c *SiteReplicationSys) PeerSTSAccHandler(ctx context.Context, stsCred *madmin.SRSTSCredential) error {
	if stsCred == nil {
		return errSRInvalidRequest(errInvalidArgument)
	}

	// Verify the session token of the stsCred
	claims, err := auth.ExtractClaims(stsCred.SessionToken, globalActiveCred.SecretKey)
	if err != nil {
		logger.LogIf(ctx, err)
		return fmt.Errorf("STS credential could not be verified")
	}

	mapClaims := claims.Map()
	expiry, err := auth.ExpToInt64(mapClaims["exp"])
	if err != nil {
		return fmt.Errorf("Expiry claim was not found: %v", mapClaims)
	}

	cred := auth.Credentials{
		AccessKey:    stsCred.AccessKey,
		SecretKey:    stsCred.SecretKey,
		Expiration:   time.Unix(expiry, 0).UTC(),
		SessionToken: stsCred.SessionToken,
		ParentUser:   stsCred.ParentUser,
		Status:       auth.AccountOn,
	}

	// Extract the username and lookup DN and groups in LDAP.
	ldapUser, isLDAPSTS := claims.Lookup(ldapUserN)
	switch {
	case isLDAPSTS:
		// Need to lookup the groups from LDAP.
		_, ldapGroups, err := globalLDAPConfig.LookupUserDN(ldapUser)
		if err != nil {
			return fmt.Errorf("unable to query LDAP server for %s: %v", ldapUser, err)
		}

		cred.Groups = ldapGroups
	}

	// Set these credentials to IAM.
	if err := globalIAMSys.SetTempUser(ctx, cred.AccessKey, cred, stsCred.ParentPolicyMapping); err != nil {
		return fmt.Errorf("unable to save STS credential and/or parent policy mapping: %v", err)
	}

	return nil
}

// BucketMetaHook - called when bucket meta changes happen and need to be
// replicated to peer clusters.
func (c *SiteReplicationSys) BucketMetaHook(ctx context.Context, item madmin.SRBucketMeta) error {
	// The change has already been applied to the local cluster at this
	// point, and only needs to be updated on all remote peer clusters.

	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return nil
	}

	cErr := c.concDo(nil, func(d string, p madmin.PeerInfo) error {
		admClient, err := c.getAdminClient(ctx, d)
		if err != nil {
			return wrapSRErr(err)
		}

		err = admClient.SRPeerReplicateBucketMeta(ctx, item)
		logger.LogIf(ctx, c.annotatePeerErr(p.Name, "SRPeerReplicateBucketMeta", err))
		return err
	})
	return cErr.summaryErr
}

// PeerBucketPolicyHandler - copies/deletes policy to local cluster.
func (c *SiteReplicationSys) PeerBucketPolicyHandler(ctx context.Context, bucket string, policy *policy.Policy) error {
	if policy != nil {
		configData, err := json.Marshal(policy)
		if err != nil {
			return wrapSRErr(err)
		}

		err = globalBucketMetadataSys.Update(bucket, bucketPolicyConfig, configData)
		if err != nil {
			return wrapSRErr(err)
		}
		return nil
	}

	// Delete the bucket policy
	err := globalBucketMetadataSys.Update(bucket, bucketPolicyConfig, nil)
	if err != nil {
		return wrapSRErr(err)
	}

	return nil
}

// PeerBucketTaggingHandler - copies/deletes tags to local cluster.
func (c *SiteReplicationSys) PeerBucketTaggingHandler(ctx context.Context, bucket string, tags *string) error {
	if tags != nil {
		configData, err := base64.StdEncoding.DecodeString(*tags)
		if err != nil {
			return wrapSRErr(err)
		}
		err = globalBucketMetadataSys.Update(bucket, bucketTaggingConfig, configData)
		if err != nil {
			return wrapSRErr(err)
		}
		return nil
	}

	// Delete the tags
	err := globalBucketMetadataSys.Update(bucket, bucketTaggingConfig, nil)
	if err != nil {
		return wrapSRErr(err)
	}

	return nil
}

// PeerBucketObjectLockConfigHandler - sets object lock on local bucket.
func (c *SiteReplicationSys) PeerBucketObjectLockConfigHandler(ctx context.Context, bucket string, objectLockData *string) error {
	if objectLockData != nil {
		configData, err := base64.StdEncoding.DecodeString(*objectLockData)
		if err != nil {
			return wrapSRErr(err)
		}
		err = globalBucketMetadataSys.Update(bucket, objectLockConfig, configData)
		if err != nil {
			return wrapSRErr(err)
		}
		return nil
	}

	return nil
}

// PeerBucketSSEConfigHandler - copies/deletes SSE config to local cluster.
func (c *SiteReplicationSys) PeerBucketSSEConfigHandler(ctx context.Context, bucket string, sseConfig *string) error {
	if sseConfig != nil {
		configData, err := base64.StdEncoding.DecodeString(*sseConfig)
		if err != nil {
			return wrapSRErr(err)
		}
		err = globalBucketMetadataSys.Update(bucket, bucketSSEConfig, configData)
		if err != nil {
			return wrapSRErr(err)
		}
		return nil
	}

	// Delete sse config
	err := globalBucketMetadataSys.Update(bucket, bucketSSEConfig, nil)
	if err != nil {
		return wrapSRErr(err)
	}
	return nil
}

// getAdminClient - NOTE: ensure to take at least a read lock on SiteReplicationSys
// before calling this.
func (c *SiteReplicationSys) getAdminClient(ctx context.Context, deploymentID string) (*madmin.AdminClient, error) {
	creds, err := c.getPeerCreds()
	if err != nil {
		return nil, err
	}

	peer, ok := c.state.Peers[deploymentID]
	if !ok {
		return nil, errSRPeerNotFound
	}

	return getAdminClient(peer.Endpoint, creds.AccessKey, creds.SecretKey)
}

func (c *SiteReplicationSys) getPeerCreds() (*auth.Credentials, error) {
	creds, ok := globalIAMSys.store.GetUser(c.state.ServiceAccountAccessKey)
	if !ok {
		return nil, errors.New("site replication service account not found")
	}
	return &creds, nil
}

// syncLocalToPeers is used when initially configuring site replication, to
// copy existing buckets, their settings, service accounts and policies to all
// new peers.
func (c *SiteReplicationSys) syncLocalToPeers(ctx context.Context) error {
	// If local has buckets, enable versioning on them, create them on peers
	// and setup replication rules.
	objAPI := newObjectLayerFn()
	if objAPI == nil {
		return errSRObjectLayerNotReady
	}
	buckets, err := objAPI.ListBuckets(ctx)
	if err != nil {
		return errSRBackendIssue(err)
	}
	for _, bucketInfo := range buckets {
		bucket := bucketInfo.Name

		// MinIO does not store bucket location - so we just check if
		// object locking is enabled.
		lockConfig, err := globalBucketMetadataSys.GetObjectLockConfig(bucket)
		if err != nil {
			if _, ok := err.(BucketObjectLockConfigNotFound); !ok {
				return errSRBackendIssue(err)
			}
		}

		var opts BucketOptions
		if lockConfig != nil {
			opts.LockEnabled = lockConfig.ObjectLockEnabled == "Enabled"
		}

		// Now call the MakeBucketHook on existing bucket - this will
		// create buckets and replication rules on peer clusters.
		err = c.MakeBucketHook(ctx, bucket, opts)
		if err != nil {
			return errSRBucketConfigError(err)
		}

		// Replicate bucket policy if present.
		policy, err := globalPolicySys.Get(bucket)
		found := true
		if _, ok := err.(BucketPolicyNotFound); ok {
			found = false
		} else if err != nil {
			return errSRBackendIssue(err)
		}
		if found {
			policyJSON, err := json.Marshal(policy)
			if err != nil {
				return wrapSRErr(err)
			}
			err = c.BucketMetaHook(ctx, madmin.SRBucketMeta{
				Type:   madmin.SRBucketMetaTypePolicy,
				Bucket: bucket,
				Policy: policyJSON,
			})
			if err != nil {
				return errSRBucketMetaError(err)
			}
		}

		// Replicate bucket tags if present.
		tags, err := globalBucketMetadataSys.GetTaggingConfig(bucket)
		found = true
		if _, ok := err.(BucketTaggingNotFound); ok {
			found = false
		} else if err != nil {
			return errSRBackendIssue(err)
		}
		if found {
			tagCfg, err := xml.Marshal(tags)
			if err != nil {
				return wrapSRErr(err)
			}
			tagCfgStr := base64.StdEncoding.EncodeToString(tagCfg)
			err = c.BucketMetaHook(ctx, madmin.SRBucketMeta{
				Type:   madmin.SRBucketMetaTypeTags,
				Bucket: bucket,
				Tags:   &tagCfgStr,
			})
			if err != nil {
				return errSRBucketMetaError(err)
			}
		}

		// Replicate object-lock config if present.
		objLockCfg, err := globalBucketMetadataSys.GetObjectLockConfig(bucket)
		found = true
		if _, ok := err.(BucketObjectLockConfigNotFound); ok {
			found = false
		} else if err != nil {
			return errSRBackendIssue(err)
		}
		if found {
			objLockCfgData, err := xml.Marshal(objLockCfg)
			if err != nil {
				return wrapSRErr(err)
			}
			objLockStr := base64.StdEncoding.EncodeToString(objLockCfgData)
			err = c.BucketMetaHook(ctx, madmin.SRBucketMeta{
				Type:   madmin.SRBucketMetaTypeObjectLockConfig,
				Bucket: bucket,
				Tags:   &objLockStr,
			})
			if err != nil {
				return errSRBucketMetaError(err)
			}
		}

		// Replicate existing bucket bucket encryption settings
		sseConfig, err := globalBucketMetadataSys.GetSSEConfig(bucket)
		found = true
		if _, ok := err.(BucketSSEConfigNotFound); ok {
			found = false
		} else if err != nil {
			return errSRBackendIssue(err)
		}
		if found {
			sseConfigData, err := xml.Marshal(sseConfig)
			if err != nil {
				return wrapSRErr(err)
			}
			sseConfigStr := base64.StdEncoding.EncodeToString(sseConfigData)
			err = c.BucketMetaHook(ctx, madmin.SRBucketMeta{
				Type:      madmin.SRBucketMetaTypeSSEConfig,
				Bucket:    bucket,
				SSEConfig: &sseConfigStr,
			})
			if err != nil {
				return errSRBucketMetaError(err)
			}
		}
	}

	{
		// Replicate IAM policies on local to all peers.
		allPolicies, err := globalIAMSys.ListPolicies(ctx, "")
		if err != nil {
			return errSRBackendIssue(err)
		}

		for pname, policy := range allPolicies {
			policyJSON, err := json.Marshal(policy)
			if err != nil {
				return wrapSRErr(err)
			}
			err = c.IAMChangeHook(ctx, madmin.SRIAMItem{
				Type:   madmin.SRIAMItemPolicy,
				Name:   pname,
				Policy: policyJSON,
			})
			if err != nil {
				return errSRIAMError(err)
			}
		}
	}

	{
		// Replicate policy mappings on local to all peers.
		userPolicyMap := make(map[string]MappedPolicy)
		groupPolicyMap := make(map[string]MappedPolicy)
		globalIAMSys.store.rlock()
		errU := globalIAMSys.store.loadMappedPolicies(ctx, stsUser, false, userPolicyMap)
		errG := globalIAMSys.store.loadMappedPolicies(ctx, stsUser, true, groupPolicyMap)
		globalIAMSys.store.runlock()
		if errU != nil {
			return errSRBackendIssue(errU)
		}
		if errG != nil {
			return errSRBackendIssue(errG)
		}

		for user, mp := range userPolicyMap {
			err := c.IAMChangeHook(ctx, madmin.SRIAMItem{
				Type: madmin.SRIAMItemPolicyMapping,
				PolicyMapping: &madmin.SRPolicyMapping{
					UserOrGroup: user,
					IsGroup:     false,
					Policy:      mp.Policies,
				},
			})
			if err != nil {
				return errSRIAMError(err)
			}
		}

		for group, mp := range groupPolicyMap {
			err := c.IAMChangeHook(ctx, madmin.SRIAMItem{
				Type: madmin.SRIAMItemPolicyMapping,
				PolicyMapping: &madmin.SRPolicyMapping{
					UserOrGroup: group,
					IsGroup:     true,
					Policy:      mp.Policies,
				},
			})
			if err != nil {
				return errSRIAMError(err)
			}
		}
	}

	{
		// Check for service accounts and replicate them. Only LDAP user
		// owned service accounts are supported for this operation.
		serviceAccounts := make(map[string]auth.Credentials)
		globalIAMSys.store.rlock()
		err := globalIAMSys.store.loadUsers(ctx, svcUser, serviceAccounts)
		globalIAMSys.store.runlock()
		if err != nil {
			return errSRBackendIssue(err)
		}
		for user, acc := range serviceAccounts {
			if user == siteReplicatorSvcAcc {
				// skip the site replicate svc account as it is
				// already replicated.
				continue
			}
			claims, err := globalIAMSys.GetClaimsForSvcAcc(ctx, acc.AccessKey)
			if err != nil {
				return errSRBackendIssue(err)
			}
			if claims != nil {
				if _, isLDAPAccount := claims[ldapUserN]; !isLDAPAccount {
					continue
				}
			}
			_, policy, err := globalIAMSys.GetServiceAccount(ctx, acc.AccessKey)
			if err != nil {
				return errSRBackendIssue(err)
			}
			var policyJSON []byte
			if policy != nil {
				policyJSON, err = json.Marshal(policy)
				if err != nil {
					return wrapSRErr(err)
				}
			}
			err = c.IAMChangeHook(ctx, madmin.SRIAMItem{
				Type: madmin.SRIAMItemSvcAcc,
				SvcAccChange: &madmin.SRSvcAccChange{
					Create: &madmin.SRSvcAccCreate{
						Parent:        acc.ParentUser,
						AccessKey:     user,
						SecretKey:     acc.SecretKey,
						Groups:        acc.Groups,
						Claims:        claims,
						SessionPolicy: json.RawMessage(policyJSON),
						Status:        acc.Status,
					},
				},
			})
			if err != nil {
				return errSRIAMError(err)
			}
		}
	}

	return nil
}

// Concurrency helpers

type concErr struct {
	numActions int
	errMap     map[string]error
	summaryErr error
}

func (c concErr) Error() string {
	return c.summaryErr.Error()
}

func (c concErr) allFailed() bool {
	return len(c.errMap) == c.numActions
}

func (c *SiteReplicationSys) toErrorFromErrMap(errMap map[string]error) error {
	if len(errMap) == 0 {
		return nil
	}

	msgs := []string{}
	for d, err := range errMap {
		name := c.state.Peers[d].Name
		msgs = append(msgs, fmt.Sprintf("Site %s (%s): %v", name, d, err))
	}
	return fmt.Errorf("Site replication error(s): %s", strings.Join(msgs, "; "))
}

func (c *SiteReplicationSys) newConcErr(numActions int, errMap map[string]error) concErr {
	return concErr{
		numActions: numActions,
		errMap:     errMap,
		summaryErr: c.toErrorFromErrMap(errMap),
	}
}

// concDo calls actions concurrently. selfActionFn is run for the current
// cluster and peerActionFn is run for each peer replication cluster.
func (c *SiteReplicationSys) concDo(selfActionFn func() error, peerActionFn func(deploymentID string, p madmin.PeerInfo) error) concErr {
	depIDs := make([]string, 0, len(c.state.Peers))
	for d := range c.state.Peers {
		depIDs = append(depIDs, d)
	}
	errs := make([]error, len(c.state.Peers))
	var wg sync.WaitGroup
	for i := range depIDs {
		wg.Add(1)
		go func(i int) {
			if depIDs[i] == globalDeploymentID {
				if selfActionFn != nil {
					errs[i] = selfActionFn()
				}
			} else {
				errs[i] = peerActionFn(depIDs[i], c.state.Peers[depIDs[i]])
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	errMap := make(map[string]error, len(c.state.Peers))
	for i, depID := range depIDs {
		if errs[i] != nil {
			errMap[depID] = errs[i]
		}
	}
	numActions := len(c.state.Peers) - 1
	if selfActionFn != nil {
		numActions++
	}
	return c.newConcErr(numActions, errMap)
}

func (c *SiteReplicationSys) annotateErr(annotation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %s: %v", c.state.Name, annotation, err)
}

func (c *SiteReplicationSys) annotatePeerErr(dstPeer string, annotation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s->%s: %s: %v", c.state.Name, dstPeer, annotation, err)
}

// isEnabled returns true if site replication is enabled
func (c *SiteReplicationSys) isEnabled() bool {
	c.RLock()
	defer c.RUnlock()
	return c.enabled
}

// Other helpers

// newRemoteClusterHTTPTransport returns a new http configuration
// used while communicating with the remote cluster.
func newRemoteClusterHTTPTransport() *http.Transport {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			RootCAs:            globalRootCAs,
			ClientSessionCache: tls.NewLRUClientSessionCache(tlsClientSessionCacheSize),
		},
	}
	return tr
}

func getAdminClient(endpoint, accessKey, secretKey string) (*madmin.AdminClient, error) {
	epURL, _ := url.Parse(endpoint)
	client, err := madmin.New(epURL.Host, accessKey, secretKey, epURL.Scheme == "https")
	if err != nil {
		return nil, err
	}
	client.SetCustomTransport(newRemoteClusterHTTPTransport())
	return client, nil
}

func getS3Client(pc madmin.PeerSite) (*minioClient.Client, error) {
	ep, _ := url.Parse(pc.Endpoint)
	return minioClient.New(ep.Host, &minioClient.Options{
		Creds:     credentials.NewStaticV4(pc.AccessKey, pc.SecretKey, ""),
		Secure:    ep.Scheme == "https",
		Transport: newRemoteClusterHTTPTransport(),
	})
}

func getPriorityHelper(replicationConfig replication.Config) int {
	maxPrio := 0
	for _, rule := range replicationConfig.Rules {
		if rule.Priority > maxPrio {
			maxPrio = rule.Priority
		}
	}

	// leave some gaps in priority numbers for flexibility
	return maxPrio + 10
}

// returns a slice with site names participating in site replciation but unspecified while adding
// a new site.
func getMissingSiteNames(oldDeps, newDeps set.StringSet, currSites []madmin.PeerInfo) []string {
	diff := oldDeps.Difference(newDeps)
	var diffSlc []string
	for _, v := range currSites {
		if diff.Contains(v.DeploymentID) {
			diffSlc = append(diffSlc, v.Name)
		}
	}
	return diffSlc
}

type srBucketMetaInfo struct {
	madmin.SRBucketInfo
	DeploymentID string
}

type srPolicy struct {
	policy       json.RawMessage
	DeploymentID string
}

type srUserPolicyMapping struct {
	madmin.SRPolicyMapping
	DeploymentID string
}

type srGroupPolicyMapping struct {
	madmin.SRPolicyMapping
	DeploymentID string
}

// SiteReplicationStatus returns the site replication status across clusters participating in site replication.
func (c *SiteReplicationSys) SiteReplicationStatus(ctx context.Context, objAPI ObjectLayer) (info madmin.SRStatusInfo, err error) {
	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return info, err
	}

	sris := make([]madmin.SRInfo, len(c.state.Peers))
	sriErrs := make([]error, len(c.state.Peers))
	g := errgroup.WithNErrs(len(c.state.Peers))
	var depIDs []string
	for d := range c.state.Peers {
		depIDs = append(depIDs, d)
	}
	for index := range depIDs {
		index := index
		if depIDs[index] == globalDeploymentID {
			g.Go(func() error {
				sris[index], sriErrs[index] = c.SiteReplicationMetaInfo(ctx, objAPI)
				return nil
			}, index)
			continue
		}
		g.Go(func() error {
			admClient, err := c.getAdminClient(ctx, depIDs[index])
			if err != nil {
				return err
			}
			sris[index], sriErrs[index] = admClient.SRMetaInfo(ctx)
			return nil
		}, index)
	}
	// Wait for the go routines.
	g.Wait()

	for _, serr := range sriErrs {
		if serr != nil {
			return info, errSRBackendIssue(serr)
		}
	}
	info.Enabled = true
	info.Sites = make(map[string]madmin.PeerInfo, len(c.state.Peers))
	for d, peer := range c.state.Peers {
		info.Sites[d] = peer
	}

	var maxBuckets int
	depIdxes := make(map[string]int)
	for i, sri := range sris {
		depIdxes[sri.DeploymentID] = i
		if len(sri.Buckets) > maxBuckets {
			maxBuckets = len(sri.Buckets)
		}
	}
	// mapping b/w entity and entity config across sites
	bucketStats := make(map[string][]srBucketMetaInfo)
	policyStats := make(map[string][]srPolicy)
	userPolicyStats := make(map[string][]srUserPolicyMapping)
	groupPolicyStats := make(map[string][]srGroupPolicyMapping)

	numSites := len(sris)
	for _, sri := range sris {
		for b, si := range sri.Buckets {
			if _, ok := bucketStats[si.Bucket]; !ok {
				bucketStats[b] = make([]srBucketMetaInfo, 0, numSites)
			}
			bucketStats[b] = append(bucketStats[b], srBucketMetaInfo{SRBucketInfo: si, DeploymentID: sri.DeploymentID})
		}
		for pname, policy := range sri.Policies {
			if _, ok := policyStats[pname]; !ok {
				policyStats[pname] = make([]srPolicy, 0, numSites)
			}
			policyStats[pname] = append(policyStats[pname], srPolicy{policy: policy, DeploymentID: sri.DeploymentID})
		}
		for user, policy := range sri.UserPolicies {
			if _, ok := userPolicyStats[user]; !ok {
				userPolicyStats[user] = make([]srUserPolicyMapping, 0, numSites)
			}
			userPolicyStats[user] = append(userPolicyStats[user], srUserPolicyMapping{SRPolicyMapping: policy, DeploymentID: sri.DeploymentID})
		}
		for group, policy := range sri.GroupPolicies {
			if _, ok := userPolicyStats[group]; !ok {
				groupPolicyStats[group] = make([]srGroupPolicyMapping, 0, numSites)
			}
			groupPolicyStats[group] = append(groupPolicyStats[group], srGroupPolicyMapping{SRPolicyMapping: policy, DeploymentID: sri.DeploymentID})
		}
	}
	info.StatsSummary = make(map[string]madmin.SRSiteSummary, len(c.state.Peers))
	info.BucketMismatches = make(map[string]map[string]madmin.SRBucketStatsSummary)
	info.PolicyMismatches = make(map[string]map[string]madmin.SRPolicyStatsSummary)
	info.UserMismatches = make(map[string]map[string]madmin.SRUserStatsSummary)
	info.GroupMismatches = make(map[string]map[string]madmin.SRGroupStatsSummary)
	// collect user policy mapping replication status across sites
	for u, pslc := range userPolicyStats {
		policySet := set.NewStringSet()
		uPolicyCount := 0
		for _, ps := range pslc {
			policyBytes, err := json.Marshal(ps)
			if err != nil {
				continue
			}
			uPolicyCount++
			if policyStr := string(policyBytes); !policySet.Contains(policyStr) {
				policySet.Add(policyStr)
			}
		}
		policyMismatch := !isReplicated(uPolicyCount, numSites, policySet)
		if policyMismatch {
			for _, ps := range pslc {
				dID := depIdxes[ps.DeploymentID]
				_, hasUser := sris[dID].UserPolicies[u]

				info.UserMismatches[u][ps.DeploymentID] = madmin.SRUserStatsSummary{
					PolicyMismatch: policyMismatch,
					UserMissing:    !hasUser,
				}
			}
		}
	}

	// collect user policy mapping replication status across sites

	for g, pslc := range groupPolicyStats {
		policySet := set.NewStringSet()
		gPolicyCount := 0
		for _, ps := range pslc {
			policyBytes, err := json.Marshal(ps)
			if err != nil {
				continue
			}
			gPolicyCount++
			if policyStr := string(policyBytes); !policySet.Contains(policyStr) {
				policySet.Add(policyStr)
			}
		}
		policyMismatch := !isReplicated(gPolicyCount, numSites, policySet)
		if policyMismatch {
			for _, ps := range pslc {
				dID := depIdxes[ps.DeploymentID]
				_, hasGroup := sris[dID].GroupPolicies[g]

				info.GroupMismatches[g][ps.DeploymentID] = madmin.SRGroupStatsSummary{
					PolicyMismatch: policyMismatch,
					GroupMissing:   !hasGroup,
				}
			}
		}
	}
	// collect IAM policy replication status across sites

	for p, pslc := range policyStats {
		var policies []*iampolicy.Policy
		uPolicyCount := 0
		for _, ps := range pslc {
			plcy, err := iampolicy.ParseConfig(bytes.NewReader(ps.policy))
			if err != nil {
				continue
			}
			policies = append(policies, plcy)
			uPolicyCount++
			sum := info.StatsSummary[ps.DeploymentID]
			sum.TotalIAMPoliciesCount++
			info.StatsSummary[ps.DeploymentID] = sum
		}
		policyMismatch := !isIAMPolicyReplicated(uPolicyCount, numSites, policies)
		switch {
		case policyMismatch:
			for _, ps := range pslc {
				dID := depIdxes[ps.DeploymentID]
				_, hasPolicy := sris[dID].Policies[p]
				if len(info.PolicyMismatches[p]) == 0 {
					info.PolicyMismatches[p] = make(map[string]madmin.SRPolicyStatsSummary)
				}
				info.PolicyMismatches[p][ps.DeploymentID] = madmin.SRPolicyStatsSummary{
					PolicyMismatch: policyMismatch,
					PolicyMissing:  !hasPolicy,
				}
			}
		default:
			// no mismatch
			for _, s := range pslc {
				sum := info.StatsSummary[s.DeploymentID]
				if !policyMismatch {
					sum.ReplicatedIAMPolicies++
				}
				info.StatsSummary[s.DeploymentID] = sum
			}

		}
	}
	// collect bucket metadata replication stats across sites
	for b, slc := range bucketStats {
		tagSet := set.NewStringSet()
		olockConfigSet := set.NewStringSet()
		var policies []*bktpolicy.Policy
		var replCfgs []*sreplication.Config
		sseCfgSet := set.NewStringSet()
		var tagCount, olockCfgCount, sseCfgCount int
		for _, s := range slc {
			if s.ReplicationConfig != nil {
				cfgBytes, err := base64.StdEncoding.DecodeString(*s.ReplicationConfig)
				if err != nil {
					continue
				}
				cfg, err := sreplication.ParseConfig(bytes.NewReader(cfgBytes))
				if err != nil {
					continue
				}
				replCfgs = append(replCfgs, cfg)
			}
			if s.Tags != nil {
				tagBytes, err := base64.StdEncoding.DecodeString(*s.Tags)
				if err != nil {
					continue
				}
				tagCount++
				if !tagSet.Contains(string(tagBytes)) {
					tagSet.Add(string(tagBytes))
				}
			}
			if len(s.Policy) > 0 {
				plcy, err := bktpolicy.ParseConfig(bytes.NewReader(s.Policy), b)
				if err != nil {
					continue
				}
				policies = append(policies, plcy)
			}
			if s.ObjectLockConfig != nil {
				olockCfgCount++
				if !olockConfigSet.Contains(*s.ObjectLockConfig) {
					olockConfigSet.Add(*s.ObjectLockConfig)
				}
			}
			if s.SSEConfig != nil {
				if !sseCfgSet.Contains(*s.SSEConfig) {
					sseCfgSet.Add(*s.SSEConfig)
				}
				sseCfgCount++
			}
			ss, ok := info.StatsSummary[s.DeploymentID]
			if !ok {
				ss = madmin.SRSiteSummary{}
			}
			// increment total number of replicated buckets
			if len(slc) == numSites {
				ss.ReplicatedBuckets++
			}
			ss.TotalBucketsCount++
			if tagCount > 0 {
				ss.TotalTagsCount++
			}
			if olockCfgCount > 0 {
				ss.TotalLockConfigCount++
			}
			if sseCfgCount > 0 {
				ss.TotalSSEConfigCount++
			}
			if len(policies) > 0 {
				ss.TotalBucketPoliciesCount++
			}
			info.StatsSummary[s.DeploymentID] = ss
		}
		tagMismatch := !isReplicated(tagCount, numSites, tagSet)
		olockCfgMismatch := !isReplicated(olockCfgCount, numSites, olockConfigSet)
		sseCfgMismatch := !isReplicated(sseCfgCount, numSites, sseCfgSet)
		policyMismatch := !isBktPolicyReplicated(numSites, policies)
		replCfgMismatch := !isBktReplCfgReplicated(numSites, replCfgs)
		switch {
		case tagMismatch, olockCfgMismatch, sseCfgMismatch, policyMismatch, replCfgMismatch:
			info.BucketMismatches[b] = make(map[string]madmin.SRBucketStatsSummary, numSites)
			for _, s := range slc {
				dID := depIdxes[s.DeploymentID]
				_, hasBucket := sris[dID].Buckets[s.Bucket]
				info.BucketMismatches[b][s.DeploymentID] = madmin.SRBucketStatsSummary{
					DeploymentID:           s.DeploymentID,
					HasBucket:              hasBucket,
					TagMismatch:            tagMismatch,
					OLockConfigMismatch:    olockCfgMismatch,
					SSEConfigMismatch:      sseCfgMismatch,
					PolicyMismatch:         policyMismatch,
					ReplicationCfgMismatch: replCfgMismatch,
					HasReplicationCfg:      len(replCfgs) > 0,
				}
			}
			fallthrough
		default:
			// no mismatch
			for _, s := range slc {
				sum := info.StatsSummary[s.DeploymentID]
				if !olockCfgMismatch && olockCfgCount == numSites {
					sum.ReplicatedLockConfig++
				}
				if !sseCfgMismatch && sseCfgCount == numSites {
					sum.ReplicatedSSEConfig++
				}
				if !policyMismatch && len(policies) == numSites {
					sum.ReplicatedBucketPolicies++
				}
				if !tagMismatch && tagCount == numSites {
					sum.ReplicatedTags++
				}
				info.StatsSummary[s.DeploymentID] = sum
			}
		}
	}
	// maximum buckets users etc seen across sites
	info.MaxBuckets = len(bucketStats)
	info.MaxUsers = len(userPolicyStats)
	info.MaxGroups = len(groupPolicyStats)
	info.MaxPolicies = len(policyStats)
	return
}

// isReplicated returns true if count of replicated matches the number of
// sites and there is atmost one unique entry in the set.
func isReplicated(cntReplicated, total int, valSet set.StringSet) bool {
	if cntReplicated > 0 && cntReplicated < total {
		return false
	}
	if len(valSet) > 1 {
		// mismatch - one or more sites has differing tags/policy
		return false
	}
	return true
}

// isIAMPolicyReplicated returns true if count of replicated IAM policies matches total
// number of sites and IAM policies are identical.
func isIAMPolicyReplicated(cntReplicated, total int, policies []*iampolicy.Policy) bool {
	if cntReplicated > 0 && cntReplicated != total {
		return false
	}
	// check if policies match between sites
	var prev *iampolicy.Policy
	for i, p := range policies {
		if i == 0 {
			prev = p
			continue
		}
		if !prev.Equals(*p) {
			return false
		}
	}
	return true
}

// isBktPolicyReplicated returns true if count of replicated bucket policies matches total
// number of sites and bucket policies are identical.
func isBktPolicyReplicated(total int, policies []*bktpolicy.Policy) bool {
	if len(policies) > 0 && len(policies) != total {
		return false
	}
	// check if policies match between sites
	var prev *bktpolicy.Policy
	for i, p := range policies {
		if i == 0 {
			prev = p
			continue
		}
		if !prev.Equals(*p) {
			return false
		}
	}
	return true
}

// isBktReplCfgReplicated returns true if all the sites have same number
// of replication rules with all replication features enabled.
func isBktReplCfgReplicated(total int, cfgs []*sreplication.Config) bool {
	cntReplicated := len(cfgs)
	if cntReplicated > 0 && cntReplicated != len(cfgs) {
		return false
	}
	// check if policies match between sites
	var prev *sreplication.Config
	for i, c := range cfgs {
		if i == 0 {
			prev = c
			continue
		}
		if len(prev.Rules) != len(c.Rules) {
			return false
		}
		if len(c.Rules) != total-1 {
			return false
		}
		for _, r := range c.Rules {
			if !strings.HasPrefix(r.ID, "site-repl-") {
				return false
			}
			if r.DeleteMarkerReplication.Status == sreplication.Disabled ||
				r.DeleteReplication.Status == sreplication.Disabled ||
				r.ExistingObjectReplication.Status == sreplication.Disabled ||
				r.SourceSelectionCriteria.ReplicaModifications.Status == sreplication.Disabled {
				return false
			}
		}
	}
	return true
}

// SiteReplicationMetaInfo returns the metadata info on buckets, policies etc for the replicated site
func (c *SiteReplicationSys) SiteReplicationMetaInfo(ctx context.Context, objAPI ObjectLayer) (info madmin.SRInfo, err error) {
	if objAPI == nil {
		return info, errSRObjectLayerNotReady
	}

	c.RLock()
	defer c.RUnlock()
	if !c.enabled {
		return info, nil
	}
	buckets, err := objAPI.ListBuckets(ctx)
	if err != nil {
		return info, errSRBackendIssue(err)
	}
	info.DeploymentID = globalDeploymentID

	info.Buckets = make(map[string]madmin.SRBucketInfo, len(buckets))
	for _, bucketInfo := range buckets {
		bucket := bucketInfo.Name
		bms := madmin.SRBucketInfo{Bucket: bucket}
		// Get bucket policy if present.
		policy, err := globalPolicySys.Get(bucket)
		found := true
		if _, ok := err.(BucketPolicyNotFound); ok {
			found = false
		} else if err != nil {
			return info, errSRBackendIssue(err)
		}
		if found {
			policyJSON, err := json.Marshal(policy)
			if err != nil {
				return info, wrapSRErr(err)
			}
			bms.Policy = policyJSON
		}

		// Get bucket tags if present.
		tags, err := globalBucketMetadataSys.GetTaggingConfig(bucket)
		found = true
		if _, ok := err.(BucketTaggingNotFound); ok {
			found = false
		} else if err != nil {
			return info, errSRBackendIssue(err)
		}
		if found {
			tagBytes, err := xml.Marshal(tags)
			if err != nil {
				return info, wrapSRErr(err)
			}
			tagCfgStr := base64.StdEncoding.EncodeToString(tagBytes)
			bms.Tags = &tagCfgStr
		}

		// Get object-lock config if present.
		objLockCfg, err := globalBucketMetadataSys.GetObjectLockConfig(bucket)
		found = true
		if _, ok := err.(BucketObjectLockConfigNotFound); ok {
			found = false
		} else if err != nil {
			return info, errSRBackendIssue(err)
		}
		if found {
			objLockCfgData, err := xml.Marshal(objLockCfg)
			if err != nil {
				return info, wrapSRErr(err)
			}
			objLockStr := base64.StdEncoding.EncodeToString(objLockCfgData)
			bms.ObjectLockConfig = &objLockStr
		}

		// Get existing bucket bucket encryption settings
		sseConfig, err := globalBucketMetadataSys.GetSSEConfig(bucket)
		found = true
		if _, ok := err.(BucketSSEConfigNotFound); ok {
			found = false
		} else if err != nil {
			return info, errSRBackendIssue(err)
		}
		if found {
			sseConfigData, err := xml.Marshal(sseConfig)
			if err != nil {
				return info, wrapSRErr(err)
			}
			sseConfigStr := base64.StdEncoding.EncodeToString(sseConfigData)
			bms.SSEConfig = &sseConfigStr
		}
		// Get replication config if present
		rcfg, err := globalBucketMetadataSys.GetReplicationConfig(ctx, bucket)
		found = true
		if _, ok := err.(BucketReplicationConfigNotFound); ok {
			found = false
		} else if err != nil {
			return info, errSRBackendIssue(err)
		}
		if found {
			rcfgXML, err := xml.Marshal(rcfg)
			if err != nil {
				return info, wrapSRErr(err)
			}
			rcfgXMLStr := base64.StdEncoding.EncodeToString(rcfgXML)
			bms.ReplicationConfig = &rcfgXMLStr
		}
		info.Buckets[bucket] = bms
	}

	{
		// Replicate IAM policies on local to all peers.
		allPolicies, err := globalIAMSys.ListPolicies(ctx, "")
		if err != nil {
			return info, errSRBackendIssue(err)
		}
		info.Policies = make(map[string]json.RawMessage, len(allPolicies))
		for pname, policy := range allPolicies {
			policyJSON, err := json.Marshal(policy)
			if err != nil {
				return info, wrapSRErr(err)
			}
			info.Policies[pname] = json.RawMessage(policyJSON)
		}
	}

	{
		// Replicate policy mappings on local to all peers.
		userPolicyMap := make(map[string]MappedPolicy)
		groupPolicyMap := make(map[string]MappedPolicy)
		globalIAMSys.store.rlock()
		errU := globalIAMSys.store.loadMappedPolicies(ctx, stsUser, false, userPolicyMap)
		errG := globalIAMSys.store.loadMappedPolicies(ctx, stsUser, true, groupPolicyMap)
		globalIAMSys.store.runlock()
		if errU != nil {
			return info, errSRBackendIssue(errU)
		}
		if errG != nil {
			return info, errSRBackendIssue(errG)
		}
		info.UserPolicies = make(map[string]madmin.SRPolicyMapping, len(userPolicyMap))
		info.GroupPolicies = make(map[string]madmin.SRPolicyMapping, len(c.state.Peers))
		for user, mp := range userPolicyMap {
			info.UserPolicies[user] = madmin.SRPolicyMapping{
				IsGroup:     false,
				UserOrGroup: user,
				Policy:      mp.Policies,
			}
		}

		for group, mp := range groupPolicyMap {
			info.UserPolicies[group] = madmin.SRPolicyMapping{
				IsGroup:     true,
				UserOrGroup: group,
				Policy:      mp.Policies,
			}
		}
	}
	return info, nil
}
