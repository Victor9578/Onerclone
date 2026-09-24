// Package rclone 通过 HTTP 与 rclone 的 RC（Remote Control）API 通信，
// 覆盖 spike 所需的最小操作面：列举、按范围读取、单文件复制。
package rclone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是 rclone RC API 的极简客户端。
type Client struct {
	Base string // 形如 http://127.0.0.1:53842
	User string
	Pass string

	jsonHTTP *http.Client // 列举/复制等短请求：60s 超时
	rawHTTP  *http.Client // 大文件读取：不设超时
}

// NewClient 构造 RC 客户端。
func NewClient(base, user, pass string) *Client {
	return &Client{
		Base:     base,
		User:     user,
		Pass:     pass,
		jsonHTTP: &http.Client{Timeout: 60 * time.Second},
		rawHTTP:  &http.Client{}, // 大文件按需读取，不设总超时
	}
}

func (c *Client) do(client *http.Client, path string, params any) (*http.Response, error) {
	// rclone RC 要求请求体必须是合法 JSON，无参数时也要发 {}
	payload := []byte("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	body := bytes.NewReader(payload)
	req, err := http.NewRequest(http.MethodPost, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.User, c.Pass)
	return client.Do(req)
}

// rcError 是 rclone 返回的错误体。
type rcError struct {
	Error  string `json:"error"`
	Status string `json:"status"`
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e rcError
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("rclone rc %s: %s", resp.Status, e.Error)
	}
	return fmt.Errorf("rclone rc %s: %s", resp.Status, bytes.TrimSpace(b))
}

// postJSON 发送 JSON 请求并解析响应。
func (c *Client) postJSON(path string, params any, out any) error {
	resp, err := c.do(c.jsonHTTP, path, params)
	if err != nil {
		return fmt.Errorf("rclone rc %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Entry 是 operations/list 的目录项。
type Entry struct {
	Name    string    `json:"Name"`
	Path    string    `json:"Path"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

type listResp struct {
	List []Entry `json:"list"`
}

// List 列举 fs 下 remote 目录（remote 用 "/" 分隔，根为空串）。
func (c *Client) List(fs, remote string) ([]Entry, error) {
	var out listResp
	err := c.postJSON("/operations/list", map[string]any{
		"fs":     fs,
		"remote": remote,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.List, nil
}

// Cat 读取 [offset, end) 区间的内容。
func (c *Client) Cat(fs, remote string, offset, end int64) ([]byte, error) {
	params := map[string]any{
		"fs":     fs,
		"remote": remote,
		"offset": offset,
		"end":    end,
	}
	resp, err := c.do(c.rawHTTP, "/operations/cat", params)
	if err != nil {
		return nil, fmt.Errorf("rclone cat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rclone cat read: %w", err)
	}
	return b, nil
}

// RangeGet 通过 --rc-serve 的 HTTP GET 读取 [start, end) 区间内容。
// URL 形如 /[<fs>]/<remote>（fs 放方括号内，源码 fsMatch 正则要求）。
// 服务端支持 HTTP Range（206）；若退化为 200 全量响应则本地切片。
func (c *Client) RangeGet(fsPath, remote string, start, end int64) ([]byte, error) {
	if end <= start {
		return nil, nil
	}
	rel := strings.TrimPrefix(remote, "/")
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s) // 文件名可能含空格/中文
	}
	target := fmt.Sprintf("%s/[%s]/%s", c.Base, fsPath, strings.Join(segs, "/"))
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	req.SetBasicAuth(c.User, c.Pass)
	resp, err := c.rawHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rclone serve GET: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206：精确区间
		return io.ReadAll(resp.Body)
	case http.StatusOK: // 200：服务器忽略 Range，本地切片兜底
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if start >= int64(len(b)) {
			return nil, nil
		}
		if end > int64(len(b)) {
			end = int64(len(b))
		}
		return b[start:end], nil
	default:
		return nil, readError(resp)
	}
}

// CopyFile 把单个文件从 src 复制到 dst（服务端操作；local→local 即文件复制）。
func (c *Client) CopyFile(srcFs, srcRemote, dstFs, dstRemote string) error {
	return c.postJSON("/operations/copyfile", map[string]any{
		"srcFs":    srcFs,
		"srcRemote": srcRemote,
		"dstFs":    dstFs,
		"dstRemote": dstRemote,
	}, nil)
}

// Version 返回 rclone 版本信息，用于就绪探测。
func (c *Client) Version() (map[string]any, error) {
	var out map[string]any
	err := c.postJSON("/core/version", nil, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}
