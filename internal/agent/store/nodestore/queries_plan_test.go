package nodestore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SIPC/nagi/internal/pkg/db"
)

// History queries must read each series through the (series, ts) primary key.
// With the ts index, a 24-hour query for one mapping reads every series sampled
// in that day (seconds on a busy node, beyond the panel's 10-second deadline).
func TestHistoryQueriesUsePrimaryKey(t *testing.T) {
	ctx := context.Background()
	d, err := db.OpenMigrated(ctx, filepath.Join(t.TempDir(), "node.db"), Migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for name, query := range map[string]string{"QueryRaw": queryRaw, "QueryM5": queryM5} {
		rows, err := d.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, "r:p/web:", 0, 100, "", -1, "r:p/web;")
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		got := strings.Join(plan, "\n")
		if strings.Contains(got, "_ts") || !strings.Contains(got, "PRIMARY KEY (series=? AND ts>? AND ts<?)") {
			t.Errorf("%s plan:\n%s", name, got)
		}
	}
}
