package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/netanelbi/colab-tui/internal/config"
)

// ContentEntry is one item in a directory listing.
type ContentEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // "file" | "directory" | "notebook"
	Size int64  `json:"size"`
}

// contentsModel is the Jupyter Contents API model.
type contentsModel struct {
	Name    string          `json:"name"`
	Path    string          `json:"path"`
	Type    string          `json:"type"`
	Format  string          `json:"format"`
	Size    int64           `json:"size"`
	Content json.RawMessage `json:"content"`
}

// contentsURL builds the /api/contents/{path} URL. Jupyter paths are relative to
// the server root, so the leading slash is stripped. A query string on the path
// (e.g. "?type=file") is preserved; spaces in the path are escaped.
func contentsURL(baseURL, path string) string {
	clean := strings.TrimPrefix(path, "/")
	clean = strings.ReplaceAll(clean, " ", "%20")
	return strings.TrimRight(baseURL, "/") + "/api/contents/" + clean
}

func contentsReq(method, baseURL, token, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, contentsURL(baseURL, path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(config.HProxyToken, token)
	req.Header.Set(config.HAgent, config.HAgentVal)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// ListDir returns the entries of a directory on the runtime.
func ListDir(baseURL, token, path string) ([]ContentEntry, error) {
	req, err := contentsReq(http.MethodGet, baseURL, token, path, nil)
	if err != nil {
		return nil, err
	}
	body, err := doContents(req)
	if err != nil {
		return nil, err
	}
	var m contentsModel
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse listing: %w", err)
	}
	if m.Type != "directory" {
		return nil, fmt.Errorf("%s is not a directory (type=%s)", path, m.Type)
	}
	var entries []ContentEntry
	if err := json.Unmarshal(m.Content, &entries); err != nil {
		return nil, fmt.Errorf("parse dir content: %w", err)
	}
	return entries, nil
}

// GetFile downloads a file's raw bytes from the runtime.
func GetFile(baseURL, token, path string) ([]byte, error) {
	req, err := contentsReq(http.MethodGet, baseURL, token, path+"?type=file&format=base64", nil)
	if err != nil {
		return nil, err
	}
	body, err := doContents(req)
	if err != nil {
		return nil, err
	}
	var m contentsModel
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse file model: %w", err)
	}
	var b64 string
	if err := json.Unmarshal(m.Content, &b64); err != nil {
		return nil, fmt.Errorf("parse file content: %w", err)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(b64, "\n", ""))
}

// PutFile writes data to a file on the runtime (creating or overwriting it).
func PutFile(baseURL, token, path string, data []byte) error {
	payload := map[string]interface{}{
		"type":    "file",
		"format":  "base64",
		"content": base64.StdEncoding.EncodeToString(data),
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := contentsReq(http.MethodPut, baseURL, token, path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = doContents(req)
	return err
}

// Delete removes a file or directory on the runtime.
func Delete(baseURL, token, path string) error {
	req, err := contentsReq(http.MethodDelete, baseURL, token, path, nil)
	if err != nil {
		return err
	}
	_, err = doContents(req)
	return err
}

func doContents(req *http.Request) ([]byte, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("contents API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
