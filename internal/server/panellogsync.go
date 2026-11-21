package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/pkg/logstore"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/server/standby"
	"google.golang.org/protobuf/proto"
)

func (s *Server) panelLogsLoop(ctx context.Context) {
	// This cache is local identity state, never overwritten by panel.db snapshots.
	if data, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "pki", "panel-mesh.json")); err == nil && len(data) <= 1<<20 {
		var peers map[string]mesh.Peer
		if json.Unmarshal(data, &peers) == nil {
			for fp, p := range peers {
				if err := s.bindLogPanel(ctx, fp, p); err != nil {
					s.log.Warn("未采用过期或无效的主备 mesh 身份", "err", err)
				}
			}
		}
	}
	type worker struct {
		address string
		cancel  context.CancelFunc
	}
	workers := map[string]worker{}
	var done sync.WaitGroup
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		done.Wait()
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var warned time.Time
	for {
		members, err := s.logPanelMembers(ctx)
		if err == nil {
			for fp, address := range members {
				if old, ok := workers[fp]; ok && old.address == address {
					continue
				}
				if old, ok := workers[fp]; ok {
					old.cancel()
				}
				work, stop := context.WithCancel(ctx)
				workers[fp] = worker{address, stop}
				done.Add(2)
				go func() {
					defer done.Done()
					retry := time.NewTicker(3 * time.Second)
					defer retry.Stop()
					for {
						_ = s.panelMeshConnect(work, fp, address)
						select {
						case <-work.Done():
							return
						case <-retry.C:
						}
					}
				}()
				go func() {
					defer done.Done()
					poll := time.NewTicker(10 * time.Second)
					defer poll.Stop()
					var lastWarn time.Time
					for {
						call, cancel := context.WithTimeout(work, 5*time.Second)
						err := s.reconcileLogPanel(call, fp)
						cancel()
						if err != nil && work.Err() == nil && time.Since(lastWarn) >= time.Minute {
							s.log.Warn("主备近期日志尚未补齐，将经 mesh 重试", "panel", fp, "err", err)
							lastWarn = time.Now()
						}
						select {
						case <-work.Done():
							return
						case <-poll.C:
						}
					}
				}()
			}
			for fp, w := range workers {
				if _, ok := members[fp]; ok {
					continue
				}
				w.cancel()
				delete(workers, fp)
				s.panelMesh.mu.Lock()
				delete(s.panelMesh.peers, fp)
				_ = s.savePanelPeersLocked()
				s.panelMesh.mu.Unlock()
				s.nodes.SetPanelPeer(fp, mesh.Peer{})
			}
		}
		// Panels without an embedded node still publish their own local facts.
		if err := s.copyPanelLocalLogs(ctx); err != nil && ctx.Err() == nil && time.Since(warned) >= time.Minute {
			s.log.Warn("面板自身日志尚未加入近期副本", "err", err)
			warned = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) copyPanelLocalLogs(ctx context.Context) error {
	c := s.nodes.LogCopies
	st, err := s.cfg.Logs.State(ctx)
	if err != nil {
		return err
	}
	for _, category := range []string{logstore.Audit, logstore.Security, logstore.Alert, logstore.System, logstore.Task, logstore.Runtime, logstore.Access, logstore.Container, logstore.Metrics} {
		r, err := c.Receipt(ctx, "panel:"+s.fingerprint, st.Source, category)
		if err != nil {
			return err
		}
		page, err := s.cfg.Logs.ReadPage(ctx, logstore.Filter{Category: category, After: r.After, Limit: 128})
		if err != nil {
			return err
		}
		if page.Next == r.After {
			continue
		}
		b := logstore.CopyBatch{Source: st.Source, Category: category, After: r.After, Through: page.Next, WriteErrors: page.State.WriteErrors}
		size := 0
		for _, e := range page.Events {
			data, err := json.Marshal(e.Data)
			if err != nil {
				return err
			}
			size += len(data) + len(e.Kind) + len(e.Object) + len(e.Project) + 512
			if size > 256<<10 && len(b.Events) > 0 {
				b.Through = b.Events[len(b.Events)-1].Seq
				break
			}
			b.Events = append(b.Events, e)
		}
		for _, p := range page.State.Pruned {
			if p.Category == category && p.Last <= b.Through {
				b.Pruned = append(b.Pruned, p)
			}
		}
		if _, err := c.Receive(ctx, "panel:"+s.fingerprint, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) logOriginKnown(ctx context.Context, node string) error {
	rows, err := s.q.ListNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range rows {
		if n.ID == node && n.Pending == 0 {
			return nil
		}
	}
	if strings.HasPrefix(node, "panel:") {
		fp := strings.TrimPrefix(node, "panel:")
		if fp == s.fingerprint {
			return nil
		}
		members, err := s.logPanelMembers(ctx)
		if err != nil {
			return err
		}
		if _, ok := members[fp]; ok {
			return nil
		}
		// Former members' retained evidence is still history, not renewed trust.
		v, err := s.q.GetSetting(ctx, standby.SettingMembers)
		if err != nil {
			return err
		}
		var old []string
		if err := json.Unmarshal([]byte(v), &old); err != nil {
			return err
		}
		for _, member := range old {
			if member == fp {
				return nil
			}
		}
	}
	return errors.New("补缺来源不属于本面板组")
}

// Only the missing bodies are read. Positions advance after local durable
// writes, separately from each producer's direct-upload acknowledgement.
func (s *Server) reconcileLogPanel(ctx context.Context, fp string) error {
	s.panelMesh.mu.Lock()
	peer, ok := s.panelMesh.peers[fp]
	s.panelMesh.mu.Unlock()
	if !ok {
		return errors.New("主备 mesh 身份尚未绑定")
	}
	if err := s.authorizeLogPanel(ctx, peer.Key); err != nil {
		return err
	}
	cc, err := mesh.Client(s.mesh, peer.Key)
	if err != nil {
		return err
	}
	defer cc.Close()
	client := nagiv1.NewPanelLogServiceClient(cc)
	c := s.nodes.LogCopies
	pos, err := c.SyncPosition(ctx, fp)
	if err != nil {
		return err
	}
	index, err := client.ListLogCopies(ctx, &nagiv1.PanelLogPosition{Source: pos.Source, After: pos.After})
	if err != nil {
		return err
	}
	next := index.GetPosition()
	if next == nil || next.Source == "" || next.After < 0 || len(index.Entries) > 128 || pos.Source == next.Source && next.After < pos.After {
		return errors.New("无效的主备日志补缺位置")
	}
	var missing []int64
	requested := map[int64]*nagiv1.PanelLogIndex{}
	var last int64
	for _, v := range index.Entries {
		if v.Cursor <= last || v.Cursor > next.After || v.Received == nil || v.Received.CheckValid() != nil {
			return errors.New("无效的主备日志补缺索引")
		}
		last = v.Cursor
		if err := s.logOriginKnown(ctx, v.Node); err != nil {
			return err
		}
		needed, err := c.Needs(ctx, logstore.CopyIndex{Cursor: v.Cursor, Node: v.Node, Source: v.Source, Category: v.Category, Seq: v.Seq, Received: v.Received.AsTime(), Hash: v.Hash})
		if err != nil {
			return err
		}
		if needed {
			missing = append(missing, v.Cursor)
			requested[v.Cursor] = v
		}
	}
	for len(missing) > 0 {
		if err := s.authorizeLogPanel(ctx, peer.Key); err != nil {
			return err
		}
		n := min(8, len(missing))
		batch := map[int64]bool{}
		for _, cursor := range missing[:n] {
			batch[cursor] = true
		}
		bodies, err := client.ReadLogCopies(ctx, &nagiv1.PanelLogRead{Source: next.Source, Cursors: missing[:n]})
		if err != nil {
			return err
		}
		if len(bodies.Entries) > n || proto.Size(bodies) > 1<<20 {
			return errors.New("主备补缺批次过大")
		}
		for _, body := range bodies.Entries {
			v := requested[body.Cursor]
			e := body.GetEntry()
			if !batch[body.Cursor] || v == nil || body.Node != v.Node || body.Source != v.Source || e == nil || e.Seq != v.Seq || e.Category != v.Category || e.Time == nil || e.Time.CheckValid() != nil || e.Received == nil || e.Received.CheckValid() != nil || !e.Received.AsTime().Equal(v.Received.AsTime()) || !json.Valid(e.DataJson) {
				return errors.New("主备日志正文与索引不符")
			}
			event := logstore.Event{Source: body.Source, Seq: e.Seq, Time: e.Time.AsTime(), Received: e.Received.AsTime(), Category: e.Category, Kind: e.Kind, Project: e.Project, Object: e.Object, Data: json.RawMessage(e.DataJson)}
			item := logstore.CopyItem{Cursor: body.Cursor, Node: body.Node, Event: event}
			if !bytes.Equal(item.Index().Hash, v.Hash) {
				return errors.New("主备日志正文摘要不符")
			}
			if err := s.authorizeLogPanel(ctx, peer.Key); err != nil {
				return err
			}
			err := c.Merge(ctx, body.Node, logstore.CopyBatch{Source: body.Source, Category: e.Category, Through: e.Seq, Events: []logstore.Event{event}})
			if err != nil {
				return err
			}
			delete(requested, body.Cursor)
		}
		missing = missing[n:]
	}
	if next.Source == pos.Source && next.After == pos.After {
		return nil
	}
	return c.SaveSyncPosition(ctx, fp, logstore.CopyPosition{Source: next.Source, After: next.After})
}
