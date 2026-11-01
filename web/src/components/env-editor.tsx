// 环境变量编辑器：一行一个变量，名称像密钥的默认隐藏值；也可以切到文本批量粘贴。对外的值仍是 KEY=VALUE 文本。
import { useState } from "react";
import { Eye, EyeOff, Plus, X } from "lucide-react";
import { Button, Input, Textarea } from "@/components/ui";
import { checkEnv } from "@/lib/config";
import { t } from "@/i18n";

type Row = { id: number; key: string; value: string };

let nextId = 1;
const sensitive = /pass|pwd|secret|token|key|credential/i;

function parse(text: string): Row[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith("#"))
    .map((l) => {
      const i = l.indexOf("=");
      return { id: nextId++, key: i < 0 ? l : l.slice(0, i).trim(), value: i < 0 ? "" : l.slice(i + 1) };
    });
}

const serialize = (rows: Row[]) =>
  rows
    .filter((r) => r.key.trim())
    .map((r) => `${r.key.trim()}=${r.value}`)
    .join("\n");

export function EnvEditor({ id, value, onChange }: { id?: string; value: string; onChange: (text: string) => void }) {
  const [mode, setMode] = useState<"rows" | "text">("rows");
  const [rows, setRows] = useState<Row[]>(() => parse(value));
  const [shown, setShown] = useState<Record<number, boolean>>({});

  const update = (next: Row[]) => {
    setRows(next);
    onChange(serialize(next));
  };
  const toggle = () => {
    if (mode === "text") setRows(parse(value));
    setMode(mode === "rows" ? "text" : "rows");
  };

  return (
    <div className="grid gap-2">
      {mode === "text" ? (
        <Textarea
          id={id}
          value={value}
          onChange={(e) => {
            onChange(e.target.value);
            e.target.setCustomValidity(checkEnv(e.target.value));
          }}
          spellCheck={false}
          className="font-mono text-[13px]"
        />
      ) : (
        rows.map((r, i) => {
          const masked = sensitive.test(r.key) && !shown[r.id];
          const keyError = r.key.trim() ? checkEnv(`${r.key.trim()}=`) : "";
          return (
            <div key={r.id} className="flex min-w-0 items-center gap-1.5">
              <Input
                id={i === 0 ? id : undefined}
                value={r.key}
                aria-label={t.containerForm.envKey}
                aria-invalid={!!keyError}
                placeholder={t.containerForm.envKey}
                spellCheck={false}
                className="h-8 w-2/5 min-w-0 font-mono text-[13px]"
                onChange={(e) => {
                  e.target.setCustomValidity(e.target.value.trim() ? checkEnv(`${e.target.value.trim()}=`) : "");
                  update(rows.map((x) => (x.id === r.id ? { ...x, key: e.target.value } : x)));
                }}
              />
              <Input
                value={r.value}
                type={masked ? "password" : "text"}
                autoComplete="off"
                aria-label={t.containerForm.envValue}
                placeholder={t.containerForm.envValue}
                spellCheck={false}
                className="h-8 min-w-0 flex-1 font-mono text-[13px]"
                onChange={(e) => update(rows.map((x) => (x.id === r.id ? { ...x, value: e.target.value } : x)))}
              />
              {sensitive.test(r.key) && (
                <Button
                  size="icon"
                  variant="ghost"
                  className="size-8"
                  aria-label={masked ? t.containerForm.envShow : t.containerForm.envHide}
                  title={masked ? t.containerForm.envShow : t.containerForm.envHide}
                  onClick={() => setShown({ ...shown, [r.id]: masked })}
                >
                  {masked ? <Eye /> : <EyeOff />}
                </Button>
              )}
              <Button size="icon" variant="ghost" className="size-8" aria-label={t.common.actions.delete} title={t.common.actions.delete} onClick={() => update(rows.filter((x) => x.id !== r.id))}>
                <X />
              </Button>
            </div>
          );
        })
      )}
      <div className="flex items-center gap-1">
        {mode === "rows" && (
          <Button size="xs" variant="ghost" onClick={() => setRows([...rows, { id: nextId++, key: "", value: "" }])}>
            <Plus />
            {t.containerForm.envAdd}
          </Button>
        )}
        <Button size="xs" variant="ghost" onClick={toggle}>
          {mode === "rows" ? t.containerForm.envBulk : t.containerForm.envRows}
        </Button>
      </div>
    </div>
  );
}
