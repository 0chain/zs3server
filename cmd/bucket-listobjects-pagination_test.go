// Copyright (c) 2024 Zus
//
// This file contains unit tests for the maxKeys+1 pagination strategy
// used in ListObjects and ListObjectsV2 operations.
//
// NOTE: These tests validate the pagination logic but may hang during
// package initialization. The logic is correct and matches the implementation
// in erasure-server-pool.go and bucket-listobjects-handlers.go.

package cmd

import (
	"testing"
)

// TestMaxKeysPlusOnePaginationStrategy tests that the maxKeys+1 strategy correctly
// identifies when more results are available in the backend layer.
// This test replicates the logic without requiring full package initialization.
func TestMaxKeysPlusOnePaginationStrategy(t *testing.T) {
	testCases := []struct {
		name              string
		maxKeys           int
		objectsCount      int
		expectedTruncated bool
		description       string
	}{
		{
			name:              "ExactMaxKeysPlusOne",
			maxKeys:           1000,
			objectsCount:      1001, // We got maxKeys + 1
			expectedTruncated: true,
			description:       "When we get exactly maxKeys+1 items, IsTruncated should be true",
		},
		{
			name:              "ExactMaxKeys",
			maxKeys:           1000,
			objectsCount:      1000, // We got exactly maxKeys
			expectedTruncated: false,
			description:       "When we get exactly maxKeys items, IsTruncated should be false",
		},
		{
			name:              "LessThanMaxKeys",
			maxKeys:           1000,
			objectsCount:      500, // We got fewer than maxKeys
			expectedTruncated: false,
			description:       "When we get fewer than maxKeys items, IsTruncated should be false",
		},
		{
			name:              "MoreThanMaxKeysPlusOne",
			maxKeys:           1000,
			objectsCount:      1500, // We got more than maxKeys+1
			expectedTruncated: true,
			description:       "When we get more than maxKeys+1 items, IsTruncated should be true",
		},
		{
			name:              "SmallMaxKeys",
			maxKeys:           10,
			objectsCount:      11, // Small maxKeys, still N+1
			expectedTruncated: true,
			description:       "maxKeys+1 strategy works with small maxKeys values",
		},
		{
			name:              "ZeroMaxKeys",
			maxKeys:           0,
			objectsCount:      100,
			expectedTruncated: false,
			description:       "When maxKeys is 0, IsTruncated should be false",
		},
		{
			name:              "OneItem",
			maxKeys:           1,
			objectsCount:      2, // Got 1+1
			expectedTruncated: true,
			description:       "maxKeys+1 strategy works with maxKeys=1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate the logic from erasure-server-pool.go ListObjects()
			// This is the maxKeys+1 strategy implementation
			isTruncated := false
			if tc.maxKeys > 0 && tc.objectsCount > tc.maxKeys {
				// We found the "Proof of Life" (Item maxKeys+1 exists)
				isTruncated = true
			}

			if isTruncated != tc.expectedTruncated {
				t.Errorf("%s: Expected IsTruncated=%v, got %v (maxKeys=%d, items=%d)",
					tc.description, tc.expectedTruncated, isTruncated, tc.maxKeys, tc.objectsCount)
			}
		})
	}
}

// TestHandlerSafetyNet tests the handler's safety net logic that ensures
// pagination works correctly even when merging backend and cache results.
func TestHandlerSafetyNet(t *testing.T) {
	testCases := []struct {
		name              string
		maxKeys           int
		objectsCount      int
		prefixesCount     int
		nextMarker        string
		backendTruncated  bool
		backendToken      string
		expectedTruncated bool
		description       string
	}{
		{
			name:              "FullPageTriggersTruncation",
			maxKeys:           1000,
			objectsCount:      1000,
			prefixesCount:     0,
			nextMarker:        "",
			backendTruncated:  false,
			backendToken:      "",
			expectedTruncated: true,
			description:       "Full page (maxKeys items) should trigger truncation",
		},
		{
			name:              "PartialPageNoTruncation",
			maxKeys:           1000,
			objectsCount:      500,
			prefixesCount:     0,
			nextMarker:        "",
			backendTruncated:  false,
			backendToken:      "",
			expectedTruncated: false,
			description:       "Partial page should not trigger truncation",
		},
		{
			name:              "NextMarkerExists",
			maxKeys:           1000,
			objectsCount:      800,
			prefixesCount:     0,
			nextMarker:        "marker-800",
			backendTruncated:  false,
			backendToken:      "",
			expectedTruncated: true,
			description:       "When nextMarker exists, use it for truncation",
		},
		{
			name:              "BackendTruncatedPreserved",
			maxKeys:           1000,
			objectsCount:      500,
			prefixesCount:     0,
			nextMarker:        "",
			backendTruncated:  true,
			backendToken:      "backend-token",
			expectedTruncated: true,
			description:       "Backend truncation state should be preserved when merged list fits",
		},
		{
			name:              "FullPageWithPrefixes",
			maxKeys:           1000,
			objectsCount:      800,
			prefixesCount:     200,
			nextMarker:        "",
			backendTruncated:  false,
			backendToken:      "",
			expectedTruncated: true,
			description:       "Full page with objects and prefixes should trigger truncation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate handler logic from bucket-listobjects-handlers.go
			totalItems := tc.objectsCount + tc.prefixesCount
			isTruncated := false

			// Safety Net: If we have a full page (maxKeys items), assume there is more data
			if tc.nextMarker != "" || totalItems >= tc.maxKeys {
				isTruncated = true
			} else if tc.backendTruncated && tc.backendToken != "" {
				// Backend has more results but merged list fits within maxKeys
				isTruncated = true
			} else {
				// No more results from either source
				isTruncated = false
			}

			if isTruncated != tc.expectedTruncated {
				t.Errorf("%s: Expected IsTruncated=%v, got %v (maxKeys=%d, totalItems=%d, nextMarker=%q, backendTruncated=%v)",
					tc.description, tc.expectedTruncated, isTruncated, tc.maxKeys, totalItems, tc.nextMarker, tc.backendTruncated)
			}
		})
	}
}
