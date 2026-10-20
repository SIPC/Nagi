package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/server/projects"
)

// WebhookHandler 处理 POST /hooks/<令牌>：拉取 git 并部署对应的容器。
// 令牌是每个容器独立的随机数，不经过登录。请求立即返回 202，部署在后台进行。
// 同一个容器在部署中又收到 webhook 时，等这次部署完再部署一次。
func (a *API) WebhookHandler() http.Handler {
	h := &webhooks{api: a, running: map[string]bool{}, again: map[string]bool{}}
	return http.HandlerFunc(h.serve)
}

type webhooks struct {
	api *API

	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
}

func (h *webhooks) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	token := strings.TrimPrefix(r.URL.Path, "/hooks/")
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	projectID, container, err := h.api.Projects.ContainerByWebhook(r.Context(), token)
	if errors.Is(err, projects.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	hook := hookTarget{projectID, container}
	key := hook.projectID + "/" + hook.container
	h.mu.Lock()
	if h.running[key] {
		h.again[key] = true
		h.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "已排队：当前部署完成后再部署一次\n")
		return
	}
	h.running[key] = true
	h.mu.Unlock()

	go h.deployLoop(hook)
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, "已开始拉取并部署\n")
}

type hookTarget struct{ projectID, container string }

func (h *webhooks) deployLoop(hook hookTarget) {
	key := hook.projectID + "/" + hook.container
	for {
		h.deploy(hook)
		h.mu.Lock()
		if !h.again[key] {
			delete(h.running, key)
			h.mu.Unlock()
			return
		}
		delete(h.again, key)
		h.mu.Unlock()
	}
}

func (h *webhooks) deploy(hook hookTarget) {
	log := h.api.Log.With("project", hook.projectID, "container", hook.container)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	version, err := h.api.Projects.Deploy(ctx, hook.projectID, []projects.Target{{Container: hook.container}},
		projects.DeployOptions{Pull: true, Source: "git"}, func(string) {})
	if err != nil {
		log.Warn("webhook 部署失败", "err", err)
		return
	}
	log.Info("webhook 部署完成", "version", version)
}
