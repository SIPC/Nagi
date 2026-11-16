import type zh from "../zh/panelSecurity";

export default {
  session: {
    title: "Sessions",
    description: "How long sign-ins last, and whether network changes require verifying again.",
    help: "Session tokens change every 15 minutes, and only the browser that signed in can complete the change. A sign-in copied and used elsewhere is signed out and triggers an alert.",
    idle: "Idle timeout",
    idleDescription: "Sign out after this long without activity.",
    maxAge: "Session lifetime",
    maxAgeDescription: "The longest a sign-in lasts.",
    bind: "Administrator network binding",
    bindDescription: "Require administrators to verify their identity again when their network changes.",
    bindHelp: "Networks are IPv4 /24 and IPv6 /48 ranges. Switching between IPv4 and IPv6 doesn't count as a change.",
    bindSubnet: "Network changes",
    bindIP: "IP changes",
    bindOff: "Off",
    bindAll: "Apply to all members",
    bindAllDescription: "Bind operators and viewers to their network the same way.",
    minutes: (n) => (n % 60 === 0 ? `${n / 60} ${n === 60 ? "hour" : "hours"}` : `${n} minutes`),
    hours: (n) => (n % 24 === 0 ? `${n / 24} ${n === 24 ? "day" : "days"}` : `${n} hours`),
  },
} satisfies typeof zh;
