import type zh from "../zh/security";

export default {
  sessions: { label: "Signed-in devices", help: "Devices signed in to this account. You can sign out the others." },
  masterKey: {
    label: "Panel master key",
    backup: "Back it up separately",
    help: "Encrypts config backup keys, 2FA secrets and alert tokens. If the master key is lost, these secrets can't be decrypted, even if the panel database is restored on another machine.",
  },
  password: {
    label: "Sign-in password",
    description: "Changing it signs out all other devices",
    help: "If you forget the password, run nagi reset password on the panel machine.",
    current: "Current password",
    newPassword: "New password",
    hint: "At least 8 characters",
    changed: "Password changed. Other devices have been signed out",
    submit: "Change password",
  },
  totp: {
    label: "Two-factor authentication",
    onStatus: "On",
    offStatus: "Off. Requires a 6-digit code from an authenticator app when signing in",
    actionOn: "Enable",
    actionOff: "Disable",
    help: "If you lose your authenticator, run nagi reset 2fa on the panel machine.",
    enable: "Enable 2FA",
    elevate: "Verify your identity to enable 2FA. You won't be asked again for 15 minutes.",
    qrCode: "Two-factor authentication QR code",
    scanHint: "Scan the QR code with an authenticator app such as Google Authenticator or 1Password, or enter the secret manually.",
    code: "Verification code",
    enabledMsg: "2FA enabled. Other devices have been signed out",
    disableLabel: "Code or sign-in password",
    disableHelp:
      "Verify your identity before disabling 2FA: enter the current 6-digit code from your authenticator app, or your sign-in password if the authenticator is lost.",
    disablePlaceholder: "000000 or password",
    disable: "Disable 2FA",
  },
  entrance: {
    label: "Secure entry",
    onStatus: "On. Note down the entry URL",
    offStatus: "Off. When turned on, only the entry URL opens the sign-in page",
    actionOn: "Enable",
    actionOff: "Disable",
    disableConfirm: "Disable secure entry? Anyone will be able to open the sign-in page.",
    help: "If you forget the entry URL, run nagi reset entrance on the panel machine.",
    disable: "Disable secure entry",
  },
} satisfies typeof zh;
