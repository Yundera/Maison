package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// netSize is transferred in each direction. Big enough that link speed
	// dominates over connection setup, small enough to finish in seconds on a
	// domestic uplink.
	netSize = 25 << 20 // 25 MiB

	netTarget  = "https://speed.cloudflare.com"
	netTimeout = 75 * time.Second
)

// NetworkResult is one link measurement.
type NetworkResult struct {
	DownloadBps     float64 `json:"download_bps"`
	UploadBps       float64 `json:"upload_bps"`
	DownloadSeconds float64 `json:"download_seconds"`
	UploadSeconds   float64 `json:"upload_seconds"`
	SizeBytes       int64   `json:"size_bytes"`
	Target          string  `json:"target"`
}

// RunNetwork measures the box's uplink against Cloudflare's speed endpoint.
//
// This reaches out to a third party and moves 50 MiB, so it is only ever run when
// a person asks for it — never on a timer, and never as a side effect of opening
// the page.
//
// The two directions run sequentially. In parallel they would compete for the same
// link and each would report roughly half the truth.
func RunNetwork(ctx context.Context) (NetworkResult, error) {
	res := NetworkResult{SizeBytes: netSize, Target: netTarget}
	client := &http.Client{Timeout: netTimeout}

	down, err := timeDownload(ctx, client)
	if err != nil {
		return res, fmt.Errorf("download: %w", err)
	}
	res.DownloadSeconds = down
	if down > 0 {
		res.DownloadBps = float64(netSize) / down
	}

	up, err := timeUpload(ctx, client)
	if err != nil {
		return res, fmt.Errorf("upload: %w", err)
	}
	res.UploadSeconds = up
	if up > 0 {
		res.UploadBps = float64(netSize) / up
	}
	return res, nil
}

func timeDownload(ctx context.Context, client *http.Client) (float64, error) {
	url := fmt.Sprintf("%s/__down?bytes=%d", netTarget, netSize)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %s", resp.Status)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}
	if n < netSize {
		return 0, fmt.Errorf("short read: %d of %d bytes", n, netSize)
	}
	return time.Since(start).Seconds(), nil
}

func timeUpload(ctx context.Context, client *http.Client) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, netTarget+"/__up", &filler{left: netSize})
	if err != nil {
		return 0, err
	}
	// Setting the length keeps the request out of chunked encoding, which would
	// add framing to every block and understate the result.
	req.ContentLength = netSize
	req.Header.Set("Content-Type", "application/octet-stream")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return time.Since(start).Seconds(), nil
}

// filler is the upload body: a fixed number of bytes from a reused block, so
// sending 25 MiB costs 32 KiB of memory and no entropy. The content is
// incompressible-enough for the endpoint's purposes, which does not compress
// octet-stream bodies anyway.
type filler struct {
	left  int64
	block [32 << 10]byte
	init  bool
}

func (f *filler) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, io.EOF
	}
	if !f.init {
		for i := range f.block {
			f.block[i] = byte(i * 31)
		}
		f.init = true
	}
	n := len(p)
	if int64(n) > f.left {
		n = int(f.left)
	}
	for copied := 0; copied < n; {
		copied += copy(p[copied:n], f.block[:])
	}
	f.left -= int64(n)
	return n, nil
}
