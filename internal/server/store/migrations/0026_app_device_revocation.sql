-- 安卓 App 手机的证明证书吊销（见 docs/android.md#吊销列表）。
-- attestation_serials：添加时证明证书链里叶子以外各张证书的序列号（小写十六进制，空格分隔），之后每次拿到新的吊销列表再查一遍。
-- revoked_reason：查到被吊销时的原因（例如 KEY_COMPROMISE），没有被吊销时为空。
ALTER TABLE app_devices ADD COLUMN attestation_serials TEXT NOT NULL DEFAULT '';
ALTER TABLE app_devices ADD COLUMN revoked_reason TEXT NOT NULL DEFAULT '';
