import type zh from "../zh/account";

export default {
  signedInAs: (name) => `Signed in as ${name}`,
  profile: "Profile",
  security: "Account security",
  upload: "Upload avatar",
  remove: "Remove",
  avatarHint: "PNG, JPEG, WebP or GIF, up to 20 MB. It is cropped to a square.",
  notImage: "Can't read this image. Use a PNG, JPEG, WebP or GIF file",
  tooLarge: "The image must be 20 MB or smaller",
  displayName: "Display name",
  displayNameHint: (username) => `Shows ${username} when empty. You still sign in with your username.`,
  saved: "Profile saved",
  reverify: {
    title: "Verify your identity",
    description: "Confirm it's you to continue.",
    reasons: {
      session_key: "This device couldn't be confirmed. Verify your identity to continue.",
      signature_invalid: "This device couldn't be confirmed. Verify your identity to continue.",
      tls_changed: "Your browser's connection characteristics changed. Verify your identity to continue.",
      network_flip: "Your network switched back and forth in a short time. Verify your identity to continue.",
      network_changed: "The network you signed in from changed. Verify your identity to continue.",
    },
    password: "Password",
    code: "2FA code",
    submit: "Verify",
    logout: "Sign out",
  },
} satisfies typeof zh;
