package observe

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

//go:embed web/*
var webFiles embed.FS

func ListenAddress() (string, error) {
	addr := os.Getenv("OBSERVE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8097"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return "", fmt.Errorf("OBSERVE_ADDR 必须绑定本机回环地址")
	}
	return addr, nil
}

func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", health)
	mux.HandleFunc("GET /api/runs", listRuns)
	mux.HandleFunc("GET /api/runs/{id}", listSteps)
	mux.HandleFunc("GET /api/steps/{id}", readSnapshot)
	mux.HandleFunc("GET /observe", serveObserve)
	mux.HandleFunc("GET /observe/", serveObserve)
	root, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(root)))
	return localOnly(mux)
}

func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "source": "sample"})
}

func listRuns(w http.ResponseWriter, r *http.Request) {
	db, err := OpenRead(DBPath())
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []RunView{}, "gaps": []Gap{}, "source": "none"})
		return
	}
	if err != nil {
		http.Error(w, "观测记录暂时读不出来", http.StatusInternalServerError)
		return
	}
	defer db.Close()
	runs, gaps, err := ListRuns(db, 40)
	if err != nil {
		http.Error(w, "观测记录暂时读不出来", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "gaps": gaps, "source": "observe"})
}

func listSteps(w http.ResponseWriter, r *http.Request) {
	db, err := OpenRead(DBPath())
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{"steps": []StepView{}})
		return
	}
	if err != nil {
		http.Error(w, "观测记录暂时读不出来", http.StatusInternalServerError)
		return
	}
	defer db.Close()
	steps, err := ListSteps(db, r.PathValue("id"))
	if err != nil {
		http.Error(w, "观测记录暂时读不出来", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps})
}

func readSnapshot(w http.ResponseWriter, r *http.Request) {
	db, err := OpenRead(DBPath())
	if err != nil {
		http.Error(w, "没有这份快照", http.StatusNotFound)
		return
	}
	defer db.Close()
	input, output, err := Snapshot(db, r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "没有这份快照", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "观测记录暂时读不出来", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"input": input, "output": output})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func serveObserve(w http.ResponseWriter, r *http.Request) {
	b, err := webFiles.ReadFile("web/observe.html")
	if err != nil {
		http.Error(w, "测光页面不存在", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err = w.Write(b); err != nil {
		slog.Error("测光页面写出失败", "error", err)
	}
}

func allowedHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "www.dropby.ob.ai", "dropby.ob.ai":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !allowedHost(host) {
			http.Error(w, "仅允许本机访问", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "不允许跨站请求", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "需要 JSON 请求", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
