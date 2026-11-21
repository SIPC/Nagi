package logstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// SaveAlertLedger commits the bounded current checkpoint and its history event
// together in logs.db. Checkpoints share the page/disk quota but cannot be evicted
// into a falsely healthy state. A full store fails closed, never using panel.db.
func (s *Store) SaveAlertLedger(ctx context.Context, node string, ledger []byte, event Event) error {
	if s == nil {
		return fmt.Errorf("告警日志库没有启动")
	}
	if len(node) > 100 || len(ledger) > 1<<20 || !json.Valid(ledger) {
		return fmt.Errorf("告警状态超出限制")
	}
	// The typed owner redacts free text before encoding. Do not run the generic
	// field-name redactor over protobuf Alert.key: it is an occurrence identity.
	event.Category = Alert
	return s.record(ctx, event, "alerts/state/"+node, ledger, false)
}
