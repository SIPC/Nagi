package store

import (
	"context"
)

// 安卓 App 添加的手机（见 docs/android.md#添加以后）。手写，不放进 queries.sql。

// AppDevice 是一台添加过的手机。
type AppDevice struct {
	ID                string
	UserID            int64
	Name              string
	Model             string
	AndroidVersion    string
	AppVersion        string
	DeviceKey         []byte
	ConfirmKey        []byte
	PushKey           []byte
	SecurityLevel     string
	BootLocked        bool
	PatchLevel        int64
	RevocationChecked bool
	DevKeys           bool
	CreatedAt         int64
	LastUsedAt        int64
	LastIP            string
	LastUnlockAt      int64
	SigFailures       int64
}

// AppDeviceUser 是手机连同它所属成员的当前角色和访问范围（换取和使用令牌时查）。
type AppDeviceUser struct {
	AppDevice
	Username    string
	DisplayName string
	Role        string
	AllProjects int64
	AllNodes    int64
	Avatar      []byte
}

const appDeviceColumns = `d.id, d.user_id, d.name, d.model, d.android_version, d.app_version, d.device_key, d.confirm_key, d.push_key,
	d.security_level, d.boot_locked, d.patch_level, d.revocation_checked, d.dev_keys, d.created_at, d.last_used_at, d.last_ip,
	d.last_unlock_at, d.sig_failures`

func appDeviceDest(d *AppDevice) []any {
	return []any{&d.ID, &d.UserID, &d.Name, &d.Model, &d.AndroidVersion, &d.AppVersion, &d.DeviceKey, &d.ConfirmKey, &d.PushKey,
		&d.SecurityLevel, &d.BootLocked, &d.PatchLevel, &d.RevocationChecked, &d.DevKeys, &d.CreatedAt, &d.LastUsedAt, &d.LastIP,
		&d.LastUnlockAt, &d.SigFailures}
}

// CreateAppDevice 保存新添加的手机。
func (q *Queries) CreateAppDevice(ctx context.Context, d AppDevice) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO app_devices (id, user_id, name, model, android_version, app_version, device_key,
		confirm_key, push_key, security_level, boot_locked, patch_level, revocation_checked, dev_keys, created_at, last_used_at, last_ip,
		last_unlock_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, d.Name, d.Model, d.AndroidVersion, d.AppVersion, d.DeviceKey, d.ConfirmKey, d.PushKey, d.SecurityLevel,
		d.BootLocked, d.PatchLevel, d.RevocationChecked, d.DevKeys, d.CreatedAt, d.LastUsedAt, d.LastIP, d.LastUnlockAt)
	return err
}

// AppDeviceByID 按设备 ID 找手机和它的成员。
func (q *Queries) AppDeviceByID(ctx context.Context, id string) (AppDeviceUser, error) {
	var d AppDeviceUser
	dest := append(appDeviceDest(&d.AppDevice), &d.Username, &d.DisplayName, &d.Role, &d.AllProjects, &d.AllNodes, &d.Avatar)
	err := q.db.QueryRowContext(ctx, `SELECT `+appDeviceColumns+`, u.username, u.display_name, u.role, u.all_projects, u.all_nodes, u.avatar
		FROM app_devices d JOIN users u ON u.id = d.user_id WHERE d.id = ?`, id).Scan(dest...)
	return d, err
}

// UserAppDevices 列出成员的手机，先添加的在前。
func (q *Queries) UserAppDevices(ctx context.Context, userID int64) ([]AppDevice, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+appDeviceColumns+` FROM app_devices d WHERE d.user_id = ? ORDER BY d.created_at, d.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppDevice{}
	for rows.Next() {
		var d AppDevice
		if err := rows.Scan(appDeviceDest(&d)...); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountUserAppDevices 是成员已经添加的手机数。
func (q *Queries) CountUserAppDevices(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_devices WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// DeleteAppDevice 删除成员的一台手机，返回删掉的行数。
func (q *Queries) DeleteAppDevice(ctx context.Context, userID int64, id string) (int64, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM app_devices WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteUserAppDevices 删除成员的全部手机（改密码、重置密码时）。
func (q *Queries) DeleteUserAppDevices(ctx context.Context, userID int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM app_devices WHERE user_id = ?`, userID)
	return err
}

// TouchAppDevice 记下一次成功的换取：最近活动、IP、解锁时间，清零签名失败的次数。
func (q *Queries) TouchAppDevice(ctx context.Context, id string, now int64, ip string, unlockAt int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE app_devices SET last_used_at = ?, last_ip = ?, last_unlock_at = ?, sig_failures = 0 WHERE id = ?`,
		now, ip, unlockAt, id)
	return err
}

// SetAppDeviceSigFailures 记下连续签名不对的次数；lockUnlock 时同时清掉解锁时间（要先解锁 App）。
func (q *Queries) SetAppDeviceSigFailures(ctx context.Context, id string, n int64, lockUnlock bool) error {
	if lockUnlock {
		_, err := q.db.ExecContext(ctx, `UPDATE app_devices SET sig_failures = ?, last_unlock_at = 0 WHERE id = ?`, n, id)
		return err
	}
	_, err := q.db.ExecContext(ctx, `UPDATE app_devices SET sig_failures = ? WHERE id = ?`, n, id)
	return err
}

// DeleteIdleAppDevices 删除 before 以前就不再换取令牌的手机，返回删掉的（记审计用）。
func (q *Queries) DeleteIdleAppDevices(ctx context.Context, before int64) ([]AppDevice, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+appDeviceColumns+` FROM app_devices d WHERE max(d.last_used_at, d.created_at) < ?`, before)
	if err != nil {
		return nil, err
	}
	var out []AppDevice
	for rows.Next() {
		var d AppDevice
		if err := rows.Scan(appDeviceDest(&d)...); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		if _, err := q.db.ExecContext(ctx, `DELETE FROM app_devices WHERE id = ?`, d.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
