import type zh from "../zh/auth";

export default {
  username: "Username",
  password: "Password",
  setup: {
    title: "Set up panel",
    description: "Create the administrator account used to sign in to the panel.",
    missingToken: "Setup token missing",
    missingTokenHint: "Open this page with the setup link printed in the panel startup log.",
    passwordMismatch: "Passwords don't match",
    passwordHint: "At least 8 characters",
    confirmPassword: "Confirm password",
    submit: "Complete setup",
  },
  login: {
    title: "Sign in to Nagi",
    back: "Back",
    totpTitle: "Two-factor authentication",
    totpDescription: "Enter the 6-digit code from your authenticator app.",
    notInitialized: "Panel not set up yet",
    notInitializedHint: (url) => (
      <>No account can sign in yet. Open the setup link printed in the panel startup log ({url}) to create the administrator account.</>
    ),
    setupUrl: "https://<address>:<port>/setup?token=…",
    code: "Verification code",
    submit: "Sign in",
    verifySubmit: "Verify and sign in",
  },
} satisfies typeof zh;
