package modelsvc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// downloadClient has no overall timeout (a multi-GB model download can take a
// while) but bails out if the server never sends response headers.
var downloadClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

// Download fetches url to dest, resuming an existing dest+".part" when the
// server supports range requests. Interrupted downloads keep their .part file
// and continue where they left off on the next run. The final file appears
// atomically via rename.
func Download(ctx context.Context, url, dest string, cb func(downloaded, total int64)) error {
	part := dest + ".part"
	var start int64
	if fi, err := os.Stat(part); err == nil {
		start = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// resume from start
	case http.StatusOK:
		start = 0 // server ignored the range; restart from scratch
	case http.StatusRequestedRangeNotSatisfiable:
		// bytes already fully present: promote and finish
		return os.Rename(part, dest)
	default:
		if resp.StatusCode >= 400 {
			return fmt.Errorf("download %s", resp.Status)
		}
	}

	var f *os.File
	if start > 0 {
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0644)
	} else {
		f, err = os.Create(part)
	}
	if err != nil {
		return err
	}
	total := resp.ContentLength
	if total > 0 {
		total += start
	}
	downloaded := start
	if cb != nil {
		cb(downloaded, total)
	}
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			downloaded += int64(n)
			if cb != nil {
				cb(downloaded, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(part, dest)
}
