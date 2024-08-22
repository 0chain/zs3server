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
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/minio/minio/internal/logger"

	"github.com/minio/minio/internal/sync/errgroup"
	"github.com/minio/pkg/bucket/policy"
)

func concurrentDecryptETag(ctx context.Context, objects []ObjectInfo) {
	g := errgroup.WithNErrs(len(objects)).WithConcurrency(500)
	for index := range objects {
		index := index
		g.Go(func() error {
			size, err := objects[index].GetActualSize()
			if err == nil {
				objects[index].Size = size
			}
			objects[index].ETag = objects[index].GetActualETag(nil)
			return nil
		}, index)
	}
	g.Wait()
}

func mergeListObjects(l1, l2 []ObjectInfo) []ObjectInfo {
	mergedMap := make(map[string]ObjectInfo)

	// Helper function to add/update map entries
	addOrUpdate := func(obj ObjectInfo) {
		if existingObj, found := mergedMap[obj.Name]; !found || obj.ModTime.After(existingObj.ModTime) {
			mergedMap[obj.Name] = obj
		}
	}
	for _, obj := range l1 {
		addOrUpdate(obj)
	}
	for _, obj := range l2 {
		addOrUpdate(obj)
	}

	mergedList := make([]ObjectInfo, 0, len(mergedMap))
	for _, obj := range mergedMap {
		mergedList = append(mergedList, obj)
	}

	return mergedList
}

func mergePrefixes(l1, l2 []string) []string {
	mergedMap := make(map[string]bool)

	// Helper function to add/update map entries
	addOrUpdate := func(pre string) {
		if _, found := mergedMap[pre]; !found {
			mergedMap[pre] = true
		}
	}
	for _, pre := range l1 {
		addOrUpdate(pre)
	}
	for _, pre := range l2 {
		addOrUpdate(pre)
	}

	mergedList := make([]string, 0, len(mergedMap))
	for pre, _ := range mergedMap {
		mergedList = append(mergedList, pre)
	}

	return mergedList
}

// Validate all the ListObjects query arguments, returns an APIErrorCode
// if one of the args do not meet the required conditions.
// Special conditions required by MinIO server are as below
//   - delimiter if set should be equal to '/', otherwise the request is rejected.
//   - marker if set should have a common prefix with 'prefix' param, otherwise
//     the request is rejected.
func validateListObjectsArgs(marker, delimiter, encodingType string, maxKeys int) APIErrorCode {
	// Max keys cannot be negative.
	if maxKeys < 0 {
		return ErrInvalidMaxKeys
	}

	if encodingType != "" {
		// AWS S3 spec only supports 'url' encoding type
		if !strings.EqualFold(encodingType, "url") {
			return ErrInvalidEncodingMethod
		}
	}

	return ErrNone
}

// ListObjectVersions - GET Bucket Object versions
// You can use the versions subresource to list metadata about all
// of the versions of objects in a bucket.
func (api objectAPIHandlers) ListObjectVersionsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListObjectVersions")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.ListBucketVersionsAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	urlValues := r.Form

	// Extract all the listBucketVersions query params to their native values.
	prefix, marker, delimiter, maxkeys, encodingType, versionIDMarker, errCode := getListBucketObjectVersionsArgs(urlValues)
	if errCode != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(errCode), r.URL)
		return
	}

	// Validate the query params before beginning to serve the request.
	if s3Error := validateListObjectsArgs(marker, delimiter, encodingType, maxkeys); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	listObjectVersions := objectAPI.ListObjectVersions

	// Inititate a list object versions operation based on the input params.
	// On success would return back ListObjectsInfo object to be
	// marshaled into S3 compatible XML header.
	listObjectVersionsInfo, err := listObjectVersions(ctx, bucket, prefix, marker, versionIDMarker, delimiter, maxkeys)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	concurrentDecryptETag(ctx, listObjectVersionsInfo.Objects)

	response := generateListVersionsResponse(bucket, prefix, marker, versionIDMarker, delimiter, encodingType, maxkeys, listObjectVersionsInfo)

	// Write success response.
	writeSuccessResponseXML(w, encodeResponse(response))
}

// ListObjectsV2MHandler - GET Bucket (List Objects) Version 2 with metadata.
// --------------------------
// This implementation of the GET operation returns some or all (up to 1000)
// of the objects in a bucket. You can use the request parameters as selection
// criteria to return a subset of the objects in a bucket.
//
// NOTE: It is recommended that this API to be used for application development.
// MinIO continues to support ListObjectsV1 and V2 for supporting legacy tools.
func (api objectAPIHandlers) ListObjectsV2MHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListObjectsV2M")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.ListBucketAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	urlValues := r.Form

	// Extract all the listObjectsV2 query params to their native values.
	prefix, token, startAfter, delimiter, fetchOwner, maxKeys, encodingType, errCode := getListObjectsV2Args(urlValues)
	if errCode != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(errCode), r.URL)
		return
	}

	// Validate the query params before beginning to serve the request.
	// fetch-owner is not validated since it is a boolean
	if s3Error := validateListObjectsArgs(token, delimiter, encodingType, maxKeys); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	listObjectsV2 := objectAPI.ListObjectsV2

	// Inititate a list objects operation based on the input params.
	// On success would return back ListObjectsInfo object to be
	// marshaled into S3 compatible XML header.
	listObjectsV2Info, err := listObjectsV2(ctx, bucket, prefix, token, delimiter, maxKeys, fetchOwner, startAfter)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	concurrentDecryptETag(ctx, listObjectsV2Info.Objects)

	// The next continuation token has id@node_index format to optimize paginated listing
	nextContinuationToken := listObjectsV2Info.NextContinuationToken

	response := generateListObjectsV2Response(bucket, prefix, token, nextContinuationToken, startAfter,
		delimiter, encodingType, fetchOwner, listObjectsV2Info.IsTruncated,
		maxKeys, listObjectsV2Info.Objects, listObjectsV2Info.Prefixes, true)

	// Write success response.
	writeSuccessResponseXML(w, encodeResponse(response))
}

// ListObjectsV2Handler - GET Bucket (List Objects) Version 2.
// --------------------------
// This implementation of the GET operation returns some or all (up to 1000)
// of the objects in a bucket. You can use the request parameters as selection
// criteria to return a subset of the objects in a bucket.
//
// NOTE: It is recommended that this API to be used for application development.
// MinIO continues to support ListObjectsV1 for supporting legacy tools.
func (api objectAPIHandlers) ListObjectsV2Handler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("ListObjectsV2Handler")
	st := time.Now()
	defer func() {
		elapsed := time.Since(st).Milliseconds()
		fmt.Printf("ListObjectsV2Handler took %d ms\n", elapsed)
	}()
	ctx := newContext(r, w, "ListObjectsV2")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.ListBucketAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	urlValues := r.Form

	// Extract all the listObjectsV2 query params to their native values.
	prefix, token, startAfter, delimiter, fetchOwner, maxKeys, encodingType, errCode := getListObjectsV2Args(urlValues)
	if errCode != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(errCode), r.URL)
		return
	}

	// Validate the query params before beginning to serve the request.
	// fetch-owner is not validated since it is a boolean
	if s3Error := validateListObjectsArgs(token, delimiter, encodingType, maxKeys); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	var (
		listObjectsV2Info      ListObjectsV2Info
		listObjectsV2InfoCache ListObjectsV2Info
		err                    error
		errC                   error
	)
	listObjectsV2Cache := objectAPI.ListObjectsV2
	if api.CacheAPI() != nil {
		listObjectsV2Cache = api.CacheAPI().ListObjectsV2
	}

	if r.Header.Get(xMinIOExtract) == "true" && strings.Contains(prefix, archivePattern) {
		// Inititate a list objects operation inside a zip file based in the input params
		listObjectsV2Info, err = listObjectsV2InArchive(ctx, objectAPI, bucket, prefix, token, delimiter, maxKeys, fetchOwner, startAfter)
	} else {
		// Inititate a list objects operation based on the input params.
		// On success would return back ListObjectsInfo object to be
		// marshaled into S3 compatible XML header.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			listObjectsV2Info, err = objectAPI.ListObjectsV2(ctx, bucket, prefix, token, delimiter, maxKeys, fetchOwner, startAfter)
		}()
		go func() {
			defer wg.Done()
			stc := time.Now()
			listObjectsV2InfoCache, errC = listObjectsV2Cache(ctx, bucket, prefix, token, delimiter, maxKeys, fetchOwner, startAfter)
			elap := time.Since(stc)
			fmt.Println("List object cache time", elap)
		}()
		wg.Wait()
	}
	if err != nil || errC != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	mergeObjects := mergeListObjects(listObjectsV2Info.Objects, listObjectsV2InfoCache.Objects)
	mergePrefixes := mergePrefixes(listObjectsV2Info.Prefixes, listObjectsV2InfoCache.Prefixes)
	listObjectsV2Info.Objects = mergeObjects
	listObjectsV2Info.Prefixes = mergePrefixes

	concurrentDecryptETag(ctx, listObjectsV2Info.Objects)

	response := generateListObjectsV2Response(bucket, prefix, token, listObjectsV2Info.NextContinuationToken, startAfter,
		delimiter, encodingType, fetchOwner, listObjectsV2Info.IsTruncated,
		maxKeys, listObjectsV2Info.Objects, listObjectsV2Info.Prefixes, false)

	// Write success response.
	writeSuccessResponseXML(w, encodeResponse(response))
}

func parseRequestToken(token string) (subToken string, nodeIndex int) {
	if token == "" {
		return token, -1
	}
	i := strings.Index(token, "@")
	if i < 0 {
		return token, -1
	}
	nodeIndex, err := strconv.Atoi(token[i+1:])
	if err != nil {
		return token, -1
	}
	subToken = token[:i]
	return subToken, nodeIndex
}

func proxyRequestByToken(ctx context.Context, w http.ResponseWriter, r *http.Request, token string) (string, bool) {
	subToken, nodeIndex := parseRequestToken(token)
	if nodeIndex > 0 {
		return subToken, proxyRequestByNodeIndex(ctx, w, r, nodeIndex)
	}
	return subToken, false
}

func proxyRequestByNodeIndex(ctx context.Context, w http.ResponseWriter, r *http.Request, index int) (success bool) {
	if len(globalProxyEndpoints) == 0 {
		return false
	}
	if index < 0 || index >= len(globalProxyEndpoints) {
		return false
	}
	ep := globalProxyEndpoints[index]
	if ep.IsLocal {
		return false
	}
	return proxyRequest(ctx, w, r, ep)
}

// ListObjectsV1Handler - GET Bucket (List Objects) Version 1.
// --------------------------
// This implementation of the GET operation returns some or all (up to 1000)
// of the objects in a bucket. You can use the request parameters as selection
// criteria to return a subset of the objects in a bucket.
func (api objectAPIHandlers) ListObjectsV1Handler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("ListObjectsV1Handler")
	st := time.Now()
	defer func() {
		elapsed := time.Since(st).Milliseconds()
		fmt.Printf("ListObjectsV1Handler took %d ms\n", elapsed)
	}()
	ctx := newContext(r, w, "ListObjectsV1")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.ListBucketAction, bucket, ""); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Extract all the litsObjectsV1 query params to their native values.
	prefix, marker, delimiter, maxKeys, encodingType, s3Error := getListObjectsV1Args(r.Form)
	if s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Validate all the query params before beginning to serve the request.
	if s3Error := validateListObjectsArgs(marker, delimiter, encodingType, maxKeys); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	listObjects := objectAPI.ListObjects
	listObjectsCache := objectAPI.ListObjects
	if api.CacheAPI() != nil {
		listObjectsCache = api.CacheAPI().ListObjects
	}
	// Inititate a list objects operation based on the input params.
	// On success would return back ListObjectsInfo object to be
	// marshaled into S3 compatible XML header.
	var (
		listObjectsInfo      ListObjectsInfo
		listObjectsInfoCache ListObjectsInfo
		err                  error
		errC                 error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		listObjectsInfo, err = listObjects(ctx, bucket, prefix, marker, delimiter, maxKeys)
	}()
	go func() {
		defer wg.Done()
		stc := time.Now()
		listObjectsInfoCache, errC = listObjectsCache(ctx, bucket, prefix, marker, delimiter, maxKeys)
		elap := time.Since(stc)
		fmt.Println("ListV1 object cache time", elap)
	}()

	wg.Wait()

	if err != nil || errC != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	mergeObjects := mergeListObjects(listObjectsInfo.Objects, listObjectsInfoCache.Objects)
	mergePrefixes := mergePrefixes(listObjectsInfo.Prefixes, listObjectsInfoCache.Prefixes)

	listObjectsInfo.Objects = mergeObjects
	listObjectsInfo.Prefixes = mergePrefixes

	concurrentDecryptETag(ctx, listObjectsInfo.Objects)

	response := generateListObjectsV1Response(bucket, prefix, marker, delimiter, encodingType, maxKeys, listObjectsInfo)

	// Write success response.
	writeSuccessResponseXML(w, encodeResponse(response))
}
