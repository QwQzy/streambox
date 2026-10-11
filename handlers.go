package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Server 持有 handler 需要的一切依赖
type Server struct {
	cfg Config
	// 以后加 logger、storage 接口都放这
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg}
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/list", s.handleList)
	mux.HandleFunc("/api/upload", s.handleUpload) // PUT: 上传；HEAD: 查询 offset
	mux.HandleFunc("/api/download", s.handleDownload)
	mux.HandleFunc("/api/mkdir", s.handleMkdir)
	mux.HandleFunc("/api/rename", s.handleRename)
}

// FileItem 是 /api/list 返回的目录项
type FileItem struct {
	Name  string `json:"name"`
	Type  string `json:"type"`  // "dir" | "file"
	Size  int64  `json:"size"`  // 目录为 0
	Mtime string `json:"mtime"` // "2006-01-02 15:04:05"
}

/* =========================================================
 * /api/list
 * ========================================================= */

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. 取 ?path=，拼到 root 下（SafeJoin 挡住 ../ 逃逸）
	rel := r.URL.Query().Get("path")
	abs, err := SafeJoin(s.cfg.Root, rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 2. 确认存在且是目录
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !info.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}

	// 3. 读目录（os.ReadDir 已按文件名排序）
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 4. 组装返回
	items := make([]FileItem, 0, len(entries))
	for _, e := range entries {
		item := FileItem{
			Name: e.Name(),
			Type: "file",
		}
		if e.IsDir() {
			item.Type = "dir"
		}

		if fi, err := e.Info(); err == nil {
			if !fi.IsDir() {
				item.Size = fi.Size()
			}
			item.Mtime = fi.ModTime().Format("2006-01-02 15:04:05")
		}

		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":  rel,
		"items": items,
	})
}

/* =========================================================
 * /api/mkdir
 * ========================================================= */

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dirRel := r.URL.Query().Get("path")  // 父目录（相对 root），可为空
	rawName := r.URL.Query().Get("name") // 新目录名，单段
	if rawName == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}

	// 1. 净化：必须恰好是一个 segment，不能有分隔符
	name, err := SafeName(rawName)
	if err != nil || name != rawName {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}

	// 2. 拼出相对路径
	rel := name
	if dirRel != "" {
		rel = dirRel + "/" + name
	}

	// 3. SafeJoin 挡住父目录里的逃逸
	abs, err := SafeJoin(s.cfg.Root, rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 4. 用 Mkdir 而不是 MkdirAll：父目录不存在时明确报错，不悄悄创建中间层
	if err := os.Mkdir(abs, 0o755); err != nil {
		switch {
		case os.IsExist(err):
			http.Error(w, "already exists", http.StatusConflict)
		case os.IsNotExist(err):
			http.Error(w, "parent not found", http.StatusNotFound)
		default:
			http.Error(w, "mkdir failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"path": rel})
}

/* =========================================================
 * /api/upload
 *   PUT  : 写入一个分片（或完整文件）
 *   HEAD : 查询目标文件已写字节数（用于断点续传）
 * ========================================================= */

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.uploadPut(w, r)
	case http.MethodHead:
		s.uploadHead(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// parseContentRange 解析 "bytes 1024-2047/5000" 或 "bytes 1024-2047/*"
// 返回 start 和 total（total 未知时为 -1）
func parseContentRange(h string) (start, total int64, err error) {
	h = strings.TrimSpace(h)
	const prefix = "bytes "
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, fmt.Errorf("missing bytes prefix")
	}
	h = h[len(prefix):]

	slash := strings.IndexByte(h, '/')
	if slash < 0 {
		return 0, 0, fmt.Errorf("missing slash")
	}
	rangePart, totalPart := h[:slash], h[slash+1:]

	dash := strings.IndexByte(rangePart, '-')
	if dash < 0 {
		return 0, 0, fmt.Errorf("missing dash")
	}
	start, err = strconv.ParseInt(rangePart[:dash], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad start: %w", err)
	}
	if totalPart == "*" {
		total = -1
	} else {
		total, err = strconv.ParseInt(totalPart, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("bad total: %w", err)
		}
	}
	if start < 0 {
		return 0, 0, fmt.Errorf("negative start")
	}
	return start, total, nil
}

// checkPathSegments 对 path 的每一段做 SafeName 校验，
// 挡住 \x00、控制字符、超长名字之类的脏东西。
func checkPathSegments(rel string) error {
	if rel == "" {
		return nil
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue // SafeJoin 会处理这些
		}
		if _, err := SafeName(seg); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) uploadPut(w http.ResponseWriter, r *http.Request) {
	// 1. 目标路径（相对 root）
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if err := checkPathSegments(rel); err != nil {
		http.Error(w, "invalid path segment", http.StatusBadRequest)
		return
	}
	abs, err := SafeJoin(s.cfg.Root, rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 2. 解析 Content-Range，缺省从 0 开始
	var offset, total int64 = 0, -1
	if cr := r.Header.Get("Content-Range"); cr != "" {
		offset, total, err = parseContentRange(cr)
		if err != nil {
			http.Error(w, "invalid content-range: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 3. 确保父目录存在
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		http.Error(w, "cannot create parent dir", http.StatusInternalServerError)
		return
	}

	// 4. 打开文件，offset==0 时从头开始（truncate），否则追加写
	flag := os.O_WRONLY | os.O_CREATE
	if offset == 0 {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		http.Error(w, "cannot open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// 5. 续传校验：offset 不能超过文件当前大小
	if offset > 0 {
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, "stat failed", http.StatusInternalServerError)
			return
		}
		if fi.Size() < offset {
			http.Error(w, "offset beyond file size", http.StatusConflict)
			return
		}
	}

	// 6. 从 offset 开始写（io.NewOffsetWriter 需 Go 1.20+）
	writer := io.NewOffsetWriter(f, offset)
	written, err := io.Copy(writer, r.Body)
	if err != nil {
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	newOffset := offset + written
	done := total >= 0 && newOffset >= total

	writeJSON(w, http.StatusOK, map[string]any{
		"offset": newOffset,
		"total":  total,
		"done":   done,
	})
}

// uploadHead 查询目标路径已上传多少字节。
//
//	200 + X-Upload-Offset: N   文件存在，已写 N 字节
//	404                        文件不存在（客户端从 0 开始）
//	400                        path 非法或目标是目录
func (s *Server) uploadHead(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if err := checkPathSegments(rel); err != nil {
		http.Error(w, "invalid path segment", http.StatusBadRequest)
		return
	}
	abs, err := SafeJoin(s.cfg.Root, rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if fi.IsDir() {
		http.Error(w, "is a directory", http.StatusBadRequest)
		return
	}
	w.Header().Set("X-Upload-Offset", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
}

/* =========================================================
 * /api/download
 * ========================================================= */

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. 取 path 并安全拼到 root 下
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	abs, err := SafeJoin(s.cfg.Root, rel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 2. 打开文件
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// 3. 必须是普通文件，不能是目录
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if fi.IsDir() {
		http.Error(w, "is a directory", http.StatusBadRequest)
		return
	}

	// 4. Content-Disposition：filename* 用 UTF-8，支持中文/特殊字符
	w.Header().Set("Content-Disposition",
		"attachment; filename*=UTF-8''"+url.PathEscape(fi.Name()))

	// 5. ServeContent 自动处理：
	//    - Range / 206 / Content-Range / Accept-Ranges
	//    - If-Range
	//    - If-Modified-Since / 304
	//    - HEAD 请求
	//    - Content-Length / Content-Type（按扩展名嗅探）
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

/* =========================================================
 * /api/rename
 * ========================================================= */

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	oldRel := r.URL.Query().Get("path")
	rawName := r.URL.Query().Get("name")
	if oldRel == "" || rawName == "" {
		http.Error(w, "missing path or name", http.StatusBadRequest)
		return
	}

	// 新名字必须单段
	name, err := SafeName(rawName)
	if err != nil || name != rawName {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}

	// 旧路径必须合法
	if err := checkPathSegments(oldRel); err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	oldAbs, err := SafeJoin(s.cfg.Root, oldRel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(oldAbs); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 新路径 = 同目录 + 新名字
	dir := filepath.Dir(oldRel)
	var newRel string
	if dir == "." || dir == "" {
		newRel = name
	} else {
		newRel = dir + "/" + name
	}
	newAbs, err := SafeJoin(s.cfg.Root, newRel)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 目标已存在就拒绝，不覆盖
	if _, err := os.Stat(newAbs); err == nil {
		http.Error(w, "target already exists", http.StatusConflict)
		return
	}

	if err := os.Rename(oldAbs, newAbs); err != nil {
		http.Error(w, "rename failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"path": newRel})
}

/* =========================================================
 * 小工具
 * ========================================================= */

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
