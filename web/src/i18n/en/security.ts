import type zh from "../zh/security";

export default {
  masterKey: {
    label: "Panel master key",
    description: "Encrypts config backup keys, 2FA secrets and alert tokens. Back it up separately.",
    help: "If the master key is lost, these secrets can't be decrypted, even if the panel database is restored on another machine.",
  },
  password: {
    label: "Sign-in password",
    description: "Changing it signs out all other sessions.",
    help: "If you forget the password, run nagi reset password on the panel machine.",
    current: "Current password",
    newPassword: "New password",
    hint: "At least 8 characters",
    changed: "Password changed. Other sessions have been signed out",
    submit: "Change password",
  },
  totp: {
    label: "Two-factor authentication",
    enabled: "On",
    disabled: "Off",
    description: "Require a 6-digit code from an authenticator app when signing in.",
    help: "If you lose your authenticator, run nagi reset 2fa on the panel machine.",
    enable: "Enable 2FA",
    qrCode: "Two-factor authentication QR code",
    scanHint: "Scan the QR code with an authenticator app (such as Google Authenticator or 1Password), or enter the secret manually.",
    code: "Verification code",
    codeHint: "6 digits",
    enabledMsg: "Two-factor authentication enabled. Other sessions have been signed out",
    confirmEnable: "Confirm and enable",
    disableLabel: "Code or sign-in password",
    disableHelp:
      "Verify your identity before disabling 2FA: enter the current 6-digit code from your authenticator app, or your sign-in password if the authenticator is lost.",
    disablePlaceholder: "000000 or password",
    disabledMsg: "Two-factor authentication disabled",
    disable: "Disable 2FA",
  },
  entrance: {
    label: "Secure entry",
    description: "When enabled, the sign-in page is only reachable through the entry URL.",
    help: "If you forget the entry URL, run nagi reset entrance on the panel machine.",
    disabledMsg: "Secure entry disabled",
    disable: "Disable secure entry",
    enabledMsg: "Secure entry enabled. Note down the entry URL",
    enable: "Enable secure entry",
  },
} satisfies typeof zh;
