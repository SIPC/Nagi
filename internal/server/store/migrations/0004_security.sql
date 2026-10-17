-- 两步验证：密钥用面板主密钥加密后存（base64），为空表示没开启；last_step 防止同一个验证码用两次。
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;
