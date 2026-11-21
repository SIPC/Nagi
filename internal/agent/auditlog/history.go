package auditlog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SIPC/nagi/internal/pkg/logstore"
)

const historyCheckpoint = "legacy.node.audit"

// MigrateHistory copies the store's records up to through into the unified log
// (logs.db) for the log page, as redacted and size-capped copies; the audit
// store itself stays the evidence. Take through=Seq after ImportLegacy and
// before enabling live capture; run one importer per Log. The durable position
// survives restarts; a full destination stops without skipping anything.
func (l *Log) MigrateHistory(ctx context.Context, dest *logstore.Store, through int64) error {
	if dest == nil {
		return nil
	}
	l.historyPending.Store(true)
	b, err := dest.Checkpoint(ctx, historyCheckpoint)
	if err != nil {
		return err
	}
	var saved struct{ Seq int64 }
	if len(b) != 0 {
		if err := json.Unmarshal(b, &saved); err != nil {
			return err
		}
	}
	if saved.Seq < 0 || saved.Seq > through {
		return fmt.Errorf("旧节点审计序号已倒退，拒绝沿用迁移位置")
	}
	after := saved.Seq
	for after < through {
		lines, _, _ := l.After(after, 64)
		var batch []Event
		for _, line := range lines {
			var e Event
			if err := json.Unmarshal(line, &e); err != nil {
				return err
			}
			if e.Seq > through {
				break
			}
			batch = append(batch, e)
		}
		// Sequences missing from the store (rotated out before the upgrade, or
		// trimmed since) get an explicit record, not just a jump in position.
		gapTo := through
		if len(batch) > 0 {
			gapTo = batch[0].Seq - 1
		}
		if gapTo > after {
			if err := dest.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.audit.gap",
				Data: map[string]any{"from": after + 1, "to": gapTo, "source": "节点审计库中没有这些序号；被清理的序号在审计链上有墓碑"}}, historyCheckpoint, struct{ Seq int64 }{gapTo}); err != nil {
				return err
			}
			after = gapTo
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if e.Seq > after+1 {
				break // re-read from here so this gap is recorded too
			}
			if e.Action == legacyImport || e.Action == actionPruned {
				after = e.Seq
				continue
			}
			if err := dest.ImportCheckpoint(ctx, logstore.Event{Time: e.Time, Category: logstore.Audit,
				Kind: "node." + e.Action, Object: e.Target, Data: e}, historyCheckpoint, struct{ Seq int64 }{e.Seq}); err != nil {
				return err
			}
			after = e.Seq
		}
	}
	if err := dest.RecordCheckpoint(ctx, logstore.Event{Category: logstore.System, Kind: "legacy.audit.imported",
		Data: map[string]any{"through": through}}, historyCheckpoint, struct{ Seq int64 }{through}); err != nil {
		return err
	}
	l.historyPending.Store(false)
	return nil
}

// HistoryDone reports whether the unified-log copy reached through.
func HistoryDone(ctx context.Context, dest *logstore.Store, through int64) bool {
	b, err := dest.Checkpoint(ctx, historyCheckpoint)
	if err != nil || len(b) == 0 {
		return false
	}
	var saved struct{ Seq int64 }
	return json.Unmarshal(b, &saved) == nil && saved.Seq >= through
}
