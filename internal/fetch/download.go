package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Get starts a GET request and returns the response only when its status is 200.
func Get(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "herdr-mobile-relay")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return response, nil
}

// DownloadFile writes the response body into out, closes out and returns the
// body's SHA-256. A body larger than maximum is an error.
func DownloadFile(ctx context.Context, client *http.Client, endpoint string, out *os.File, maximum int64) (string, error) {
	response, err := Get(ctx, client, endpoint)
	if err != nil {
		out.Close()
		return "", err
	}
	defer response.Body.Close()
	if response.ContentLength > maximum {
		out.Close()
		return "", errors.New("response exceeds size limit")
	}
	checksum := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, checksum), io.LimitReader(response.Body, maximum+1))
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > maximum {
		return "", errors.New("response exceeds size limit")
	}
	return hex.EncodeToString(checksum.Sum(nil)), nil
}
