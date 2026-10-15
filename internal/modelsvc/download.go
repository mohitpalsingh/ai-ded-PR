package modelsvc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Download fetches url to dest, reporting progress via cb.
func Download(ctx context.Context, url, dest string, cb func(downloaded, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download %s", resp.Status)
	}
	f, err := os.Create(dest + ".part")
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var downloaded int64
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
	f.Close()
	return os.Rename(dest+".part", dest)
}
