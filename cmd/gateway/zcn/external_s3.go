package zcn

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ExternalS3 implements the Router function: if an object is not in Züs
// (not in /nfs_export cache, not in blobbers), fetch it from external AWS S3
// and store it in Züs via the NFS cache path.
//
// Flow:
//   S3 GET arrives → CacheRouter checks /nfs_export → miss
//   → getFileReader checks blobbers → miss (not in Züs)
//   → FetchFromExternalS3 → download from AWS → write to /nfs_export
//   → blobber sync commits to Züs → return data to caller

var extS3Endpoint string

// InitExternalS3 sets up the external S3 endpoint for fetches.
func InitExternalS3() {
	if serverConfig.ExternalS3Endpoint == "" {
		return
	}
	extS3Endpoint = serverConfig.ExternalS3Endpoint
	log.Printf("[Router] External S3 enabled: %s (region: %s)",
		extS3Endpoint, serverConfig.ExternalS3Region)
}

// FetchFromExternalS3 downloads an object from external S3, stores it
// in /nfs_export (local cache), and returns a reader.
// Uses simple HTTP GET with AWS Signature v4 via pre-signed or IAM.
// Returns error if external S3 is not configured or object not found.
func FetchFromExternalS3(bucket, key string) (io.ReadCloser, int64, error) {
	if extS3Endpoint == "" {
		return nil, 0, fmt.Errorf("external S3 not configured")
	}

	// Build the S3 URL
	url := fmt.Sprintf("%s/%s/%s", extS3Endpoint, bucket, key)

	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}

	// If access key is configured, use basic auth as a simple mechanism.
	// For production, use IAM roles or pre-signed URLs.
	if serverConfig.ExternalS3AccessKey != "" {
		req.SetBasicAuth(serverConfig.ExternalS3AccessKey, serverConfig.ExternalS3SecretKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("external S3 GET %s: %w", url, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("not found on external S3: %s/%s", bucket, key)
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("external S3 status %d for %s/%s", resp.StatusCode, bucket, key)
	}

	size := resp.ContentLength

	// Write to /nfs_export so it's cached locally + blobber sync commits to Züs
	nfsDir := serverConfig.NFSGaneshaExportDir
	if nfsDir != "" {
		localPath := filepath.Join(nfsDir, bucket, key)
		if writeErr := writeToNFSCache(localPath, resp.Body, size); writeErr != nil {
			log.Printf("[Router] Cache write failed for %s/%s: %v", bucket, key, writeErr)
			// Data was consumed by writeToNFSCache, can't return resp.Body
			// Try to re-open from disk if write partially succeeded
			if f, openErr := os.Open(localPath); openErr == nil {
				fi, _ := f.Stat()
				return f, fi.Size(), nil
			}
			return nil, 0, fmt.Errorf("external S3 cache write failed: %w", writeErr)
		}
		resp.Body.Close()

		// Serve from local cache
		f, err := os.Open(localPath)
		if err != nil {
			return nil, 0, fmt.Errorf("reopen cached %s: %w", localPath, err)
		}
		fi, _ := f.Stat()
		log.Printf("[Router] Fetched from external S3 and cached: %s/%s (%d bytes)", bucket, key, fi.Size())
		return f, fi.Size(), nil
	}

	return resp.Body, size, nil
}

// writeToNFSCache writes data to the NFS export directory.
func writeToNFSCache(localPath string, data io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	written, err := io.Copy(f, data)
	if err != nil {
		os.Remove(localPath)
		return err
	}

	if size > 0 && written != size {
		os.Remove(localPath)
		return fmt.Errorf("short write: expected %d, wrote %d", size, written)
	}

	return nil
}

// IsExternalS3Configured returns true if external S3 fetching is enabled.
func IsExternalS3Configured() bool {
	return extS3Endpoint != ""
}
