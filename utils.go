package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrPathEscape = errors.New("path escapes root")
	ErrBadName    = errors.New("invalid file name")
)

func prepareRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root is not a directory: %s", root)
	}
	return nil
}

// SafeJoin 把用户提供的相对路径拼到 root 下，并保证结果不会逃逸 root。
// rel 里的 ../、绝对路径、盘符、UNC 都会被拒绝或净化。
// root 必须是绝对路径（main 启动时已 Abs）。
func SafeJoin(root, rel string) (string, error) {
	// 1. 统一用 "/" 处理，先把用户输入里的反斜杠也当分隔符
	rel = strings.ReplaceAll(rel, "\\", "/")

	// 2. 拒绝绝对路径和盘符（Windows 的 C:、UNC 的 //server）
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return "", ErrPathEscape
	}

	// 3. 拼接后 Clean：a/../b → b，../../etc → ../../etc
	joined := filepath.Join(root, filepath.FromSlash(rel))

	// 4. 用 Rel 判断是否还在 root 内（比字符串前缀可靠）
	r, err := filepath.Rel(root, joined)
	if err != nil {
		return "", ErrPathEscape
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", ErrPathEscape
	}

	return joined, nil
}

// SafeName 净化用户上传时的文件名（不含路径）。
// 只保留最后一段，剔除分隔符和奇怪字符。
func SafeName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name) // 去掉任何目录前缀
	name = strings.TrimSpace(name)

	if name == "" || name == "." || name == ".." {
		return "", ErrBadName
	}
	// 可选：屏蔽控制字符
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", ErrBadName
		}
	}
	return name, nil
}
