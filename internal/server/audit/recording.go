package audit

import "github.com/SIPC/nagi/internal/pkg/recording"

// Keep the audit API and old recordings compatible while the shared recorder
// is also used at the actual execution node.
type Recordings = recording.Recordings
type Recorder = recording.Recorder
type Summary = recording.Summary
type Sealer = recording.Sealer
