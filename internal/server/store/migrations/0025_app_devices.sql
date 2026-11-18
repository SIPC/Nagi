-- 安卓 App 添加的手机（见 docs/android.md#添加以后）。三把密钥存 SPKI（ECDSA P-256），设备 ID 是 16 字节随机数（base64url）。
CREATE TABLE app_devices (
    id                 TEXT    PRIMARY KEY,
    user_id            INTEGER NOT NULL,
    name               TEXT    NOT NULL,
    model              TEXT    NOT NULL DEFAULT '',
    android_version    TEXT    NOT NULL DEFAULT '',
    app_version        TEXT    NOT NULL DEFAULT '',
    device_key         BLOB    NOT NULL,
    confirm_key        BLOB    NOT NULL,
    push_key           BLOB    NOT NULL,
    -- strongbox、tee；开发模式下的软件密钥是 software。
    security_level     TEXT    NOT NULL,
    boot_locked        INTEGER NOT NULL DEFAULT 0,
    -- 系统安全补丁日期，例如 202510。
    patch_level        INTEGER NOT NULL DEFAULT 0,
    revocation_checked INTEGER NOT NULL DEFAULT 0,
    -- 用 --dev-app-keys 添加的。
    dev_keys           INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    last_used_at       INTEGER NOT NULL DEFAULT 0,
    last_ip            TEXT    NOT NULL DEFAULT '',
    last_unlock_at     INTEGER NOT NULL DEFAULT 0,
    -- 连续签名不对的次数，到 5 次要先解锁 App。
    sig_failures       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX app_devices_user ON app_devices (user_id);
CREATE TRIGGER app_devices_user_delete AFTER DELETE ON users BEGIN
    DELETE FROM app_devices WHERE user_id = OLD.id;
END;

-- 高权限确认用的方式：password（密码或验证码）。生成令牌、添加通行密钥、开启两步验证、添加手机只认它（见 docs/android.md#高权限确认和重新验证）。
ALTER TABLE sessions ADD COLUMN elevated_by TEXT NOT NULL DEFAULT '';
