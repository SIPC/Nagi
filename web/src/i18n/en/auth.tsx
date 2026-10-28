import type zh from "../zh/auth";

export default {
  username: "Username",
  password: "Password",
  setup: {
    title: "Set up panel",
    description: "Create the admin account used to sign in to the panel.",
    missingToken: "Setup token missing",
    missingTokenHint: "Open this page with the setup link printed in the panel startup log.",
    passwordMismatch: "Passwords do not match",
    passwordHint: "At least 8 characters",
    confirmPassword: "Confirm password",
    submit: "Complete setup",
  },
  login: {
    title: "Sign in",
    totpTitle: "Two-factor authentication",
    description: "Sign in to the panel with the admin account.",
    totpDescription: "Enter the 6-digit code from your authenticator app.",
    notInitialized: "Panel not set up yet",
    notInitializedHint: (url) => (
      <>No account can sign in yet. Open the setup link printed in the panel startup log ({url}) to create the admin account.</>
    ),
    setupUrl: "https://address:port/setup?token=…",
    code: "Verification code",
    submit: "Sign in",
    verifySubmit: "Verify and sign in",
  },
} satisfies typeof zh;
