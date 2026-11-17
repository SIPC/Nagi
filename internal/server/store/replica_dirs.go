package store

// 容器在备份节点上单独指定的副本存放位置（见 docs/snapshot.md#副本的存放位置）。手写的，理由同 tokens.go。

import "context"

// ListBackupReplicaDirs 返回容器单独指定了副本存放位置的备份节点：节点 ID → 目录。
func (q *Queries) ListBackupReplicaDirs(ctx context.Context, projectID, container string) (map[string]string, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT node_id, dir FROM backup_replica_dirs WHERE project_id = ? AND container = ?`, projectID, container)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var node, dir string
		if err := rows.Scan(&node, &dir); err != nil {
			return nil, err
		}
		out[node] = dir
	}
	return out, rows.Err()
}

// SetBackupReplicaDir 记下容器在备份节点 nodeID 上的副本存放位置；dir 为空表示用节点的默认目录，删掉记录。
func (q *Queries) SetBackupReplicaDir(ctx context.Context, projectID, container, nodeID, dir string) error {
	if dir == "" {
		_, err := q.db.ExecContext(ctx, `DELETE FROM backup_replica_dirs WHERE project_id = ? AND container = ? AND node_id = ?`, projectID, container, nodeID)
		return err
	}
	_, err := q.db.ExecContext(ctx, `INSERT INTO backup_replica_dirs (project_id, container, node_id, dir) VALUES (?, ?, ?, ?)
ON CONFLICT (project_id, container, node_id) DO UPDATE SET dir = excluded.dir`, projectID, container, nodeID, dir)
	return err
}
