// Package web 把前端构建产物打包进二进制。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

const placeholder = `<!doctype html><meta charset="utf-8"><title>Nagi</title>
<p style="font-family:sans-serif;margin:3rem">前端尚未构建。请先在 web 目录执行 <code>pnpm install &amp;&amp; pnpm build</code>，再重新编译 nagi。</p>`

// Handler 返回静态文件服务。找不到的路径（前端路由）返回 index.html。
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		index = []byte(placeholder)
	}
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if f, err := root.Open(p); err == nil {
				st, _ := f.Stat()
				f.Close()
				if st != nil && !st.IsDir() {
					if strings.HasPrefix(p, "assets/") {
						// Vite 产物的文件名带内容哈希，可以长期缓存。
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					}
					files.ServeHTTP(w, r)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
