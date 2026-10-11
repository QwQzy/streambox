package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
)

//go:embed web
var webFS embed.FS

type Config struct {
	Host string
	Port int
	Root string
}

func parseFlags() Config {
	host := flag.String("host", "127.0.0.1", "监听地址")
	port := flag.Int("port", 8080, "监听端口")
	root := flag.String("root", "./data", "文件存储根目录")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("invalid root %q: %v", *root, err)
	}

	return Config{
		Host: *host,
		Port: *port,
		Root: filepath.Clean(abs), // 绝对 + 无尾部分隔符
	}
}

func main() {
	cfg := parseFlags()

	if err := prepareRoot(cfg.Root); err != nil {
		log.Fatalf("prepare root failed: %v", err)
	}
	log.Printf("streambox root=%s", cfg.Root)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// API 先注册
	srv := NewServer(cfg)
	srv.Routes(mux)

	// 静态页面兜底
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	log.Printf("streambox listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
