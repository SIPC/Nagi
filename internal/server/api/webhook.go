package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/auth"
	"github.com/SIPC/nagi/internal/server/store"
)

// WebhookHandler 处理 POST /hooks/<令牌>：拉取 git 并部署对应的项目。
// 令牌是每个项目独立的随机数，不经过登录。请求立即返回 202，部署在后台进行。
// 同一个项目在部署中又收到 webhook 时，等这次部署完再部署一次。
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
	hook, err := h.api.Q.GetWebhook(r.Context(), auth.HashToken(token))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	if !h.api.Nodes.Online(hook.NodeID) {
		http.Error(w, "节点不在线", http.StatusServiceUnavailable)
		return
	}
	key := hook.NodeID + "/" + hook.ProjectID + "/" + hook.Member
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

func (h *webhooks) deployLoop(hook store.Webhook) {
	key := hook.NodeID + "/" + hook.ProjectID + "/" + hook.Member
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

func (h *webhooks) deploy(hook store.Webhook) {
	log := h.api.Log.With("node", hook.NodeID, "project", hook.ProjectID, "member", hook.Member)
	conn, err := h.api.Nodes.Get(hook.NodeID)
	if err != nil {
		log.Warn("webhook 部署失败：节点不在线")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	stream, err := conn.Agent.DeployProject(ctx, &nagiv1.DeployProjectRequest{
		Id: hook.ProjectID, Pull: true, Source: "git", Targets: []*nagiv1.DeployTarget{{Member: hook.Member}},
	})
	if err != nil {
		log.Warn("webhook 部署失败", "err", err)
		return
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Warn("webhook 部署失败", "err", err)
			}
			return
		}
		if res := ev.GetResult(); res != nil {
			if res.GetSuccess() {
				log.Info("webhook 部署完成", "version", res.GetVersion())
			} else {
				log.Warn("webhook 部署失败", "err", res.GetError())
			}
		}
	}
}
