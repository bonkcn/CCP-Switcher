package sync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// WebDAV implements Backend using WebDAV HTTP methods (PUT/GET/MKCOL).
type WebDAV struct {
	baseURL  string
	filePath string
	username string
	password string
	client   *http.Client
}

// NewWebDAV creates a new WebDAV backend.
func NewWebDAV(cfg Config) *WebDAV {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	filePath := strings.TrimSpace(cfg.Path)
	if filePath == "" {
		filePath = DefaultPath
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}

	return &WebDAV{
		baseURL:  baseURL,
		filePath: filePath,
		username: cfg.KeyID,
		password: cfg.Secret,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (w *WebDAV) Push(ctx context.Context, data []byte) error {
	// Ensure parent directories exist
	if err := w.ensureParentDirs(ctx); err != nil {
		return fmt.Errorf("webdav mkdir: %w", err)
	}

	url := w.baseURL + w.filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("webdav push: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	w.setAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav push: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("webdav push: HTTP %d %s", resp.StatusCode, resp.Status)
}

func (w *WebDAV) Pull(ctx context.Context) ([]byte, error) {
	url := w.baseURL + w.filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("webdav pull: %w", err)
	}
	w.setAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav pull: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("webdav pull: 远程文件不存在，请先推送")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webdav pull: HTTP %d %s", resp.StatusCode, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MiB limit
	if err != nil {
		return nil, fmt.Errorf("webdav pull: %w", err)
	}
	return data, nil
}

func (w *WebDAV) Test(ctx context.Context) error {
	// PROPFIND on the base URL to verify connection
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", w.baseURL+"/", nil)
	if err != nil {
		return fmt.Errorf("webdav test: %w", err)
	}
	req.Header.Set("Depth", "0")
	w.setAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav test: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 207 Multi-Status is the expected response for PROPFIND
	if resp.StatusCode == 207 || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("webdav test: 认证失败 (HTTP %d)", resp.StatusCode)
	}
	return fmt.Errorf("webdav test: HTTP %d %s", resp.StatusCode, resp.Status)
}

func (w *WebDAV) ensureParentDirs(ctx context.Context) error {
	dir := path.Dir(w.filePath)
	if dir == "/" || dir == "." {
		return nil
	}

	// Build list of directories to create
	var dirs []string
	for d := dir; d != "/" && d != "."; d = path.Dir(d) {
		dirs = append([]string{d}, dirs...)
	}

	for _, d := range dirs {
		url := w.baseURL + d + "/"
		req, err := http.NewRequestWithContext(ctx, "MKCOL", url, nil)
		if err != nil {
			return err
		}
		w.setAuth(req)
		resp, err := w.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		// 201 Created, 405 Already Exists are both OK
		if resp.StatusCode != 201 && resp.StatusCode != 405 && resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode != 405 {
				return fmt.Errorf("MKCOL %s: HTTP %d", d, resp.StatusCode)
			}
		}
	}
	return nil
}

func (w *WebDAV) setAuth(req *http.Request) {
	if w.username != "" || w.password != "" {
		req.SetBasicAuth(w.username, w.password)
	}
}
