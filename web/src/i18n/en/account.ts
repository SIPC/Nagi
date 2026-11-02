import type zh from "../zh/account";

export default {
  menu: "Account menu",
  signedInAs: (name) => `Signed in as ${name}`,
  profile: "Profile",
  security: "Account security",
  avatar: "Avatar",
  upload: "Upload avatar",
  remove: "Remove",
  avatarHint: "PNG, JPEG, WebP or GIF, up to 20 MB. It is cropped to a square.",
  notImage: "This file cannot be opened as an image",
  tooLarge: "The image must be 20 MB or smaller",
  displayName: "Display name",
  displayNameHint: (username) => `Shows ${username} when empty. You still sign in with your username.`,
  saved: "Profile saved",
} satisfies typeof zh;
