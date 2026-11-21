// Package sealer preserves the panel encryption API; nodes use pkg/sealer.
package sealer

import "github.com/SIPC/nagi/internal/pkg/sealer"

type Sealer = sealer.Sealer

func Load(path string) (*Sealer, error) { return sealer.Load(path) }
