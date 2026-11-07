import type * as React from "react";

// HelpParts：问号里的几段说明，一段一行。
export function HelpParts({ parts }: { parts: React.ReactNode[] }) {
  return (
    <span className="grid gap-1">
      {parts.map((p, i) => (
        <span key={i}>{p}</span>
      ))}
    </span>
  );
}
