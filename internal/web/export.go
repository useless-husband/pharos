package web

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
)

// Export writes the public status page as static files into dir:
// index.html, one history page per public monitor, status.json and the
// assets. The result can be served by any static host (GitHub Pages, S3,
// nginx) from any sub-path, because every link is relative.
func (s *Server) Export(ctx context.Context, dir string) ([]string, error) {
	cfg := s.cfg()
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o755); err != nil {
		return nil, err
	}
	var written []string
	write := func(rel string, b []byte) error {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		written = append(written, rel)
		return os.WriteFile(path, b, 0o644)
	}
	// A request from nowhere: no session, not local, so nothing private leaks.
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.RemoteAddr = "192.0.2.1:1"

	d, err := s.statusPageData(ctx, req)
	if err != nil {
		return nil, err
	}
	d.Static, d.HomeHref, d.root = true, "index.html", ""
	var buf bytes.Buffer
	if err := s.pages["status"].Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("render status page: %w", err)
	}
	if err := write("index.html", buf.Bytes()); err != nil {
		return nil, err
	}

	for _, id := range publicIDs(cfg) {
		rec := httptest.NewRecorder()
		hreq := req.Clone(ctx)
		hreq.URL.Path = "/history/" + id
		hreq.SetPathValue("id", id)
		s.renderHistory(rec, hreq, true)
		if rec.Code != http.StatusOK {
			return nil, fmt.Errorf("render history of %s: HTTP %d", id, rec.Code)
		}
		if err := write(filepath.Join("history", id+".html"), rec.Body.Bytes()); err != nil {
			return nil, err
		}
	}

	rec := httptest.NewRecorder()
	s.handleAPIStatus(rec, req)
	if err := write("status.json", rec.Body.Bytes()); err != nil {
		return nil, err
	}

	err = fs.WalkDir(assetFS, "assets", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		return write(p, b)
	})
	if err != nil {
		return nil, err
	}
	// GitHub Pages: do not run Jekyll over the output.
	if err := write(".nojekyll", nil); err != nil {
		return nil, err
	}
	for i := range written {
		written[i] = strings.ReplaceAll(written[i], string(filepath.Separator), "/")
	}
	return written, nil
}
