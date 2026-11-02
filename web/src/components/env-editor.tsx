// 环境变量编辑器：一行一个变量，名称像密钥的默认隐藏值；也可以切到文本批量粘贴。对外的值仍是 KEY=VALUE 文本。
// 添加容器、项目共享变量用默认样式（两列等宽，切换按钮在下面）；容器配置页是 mode 受控的紧凑样式（名称 1 份、值 2 份、等宽字体，切换按钮在区块标题上）。
import { useEffect, useRef, useState } from "react";
import { Eye, EyeOff, Plus, X } from "lucide-react";
import { Button, Input, Textarea } from "@/components/ui";
import { checkEnv } from "@/lib/config";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

type Row = { id: number; key: string; value: string };
export type EnvMode = "rows" | "text";

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

export function EnvEditor({ id, value, onChange, mode: modeProp }: { id?: string; value: string; onChange: (text: string) => void; mode?: EnvMode }) {
  const [modeState, setModeState] = useState<EnvMode>("rows");
  const controlled = modeProp !== undefined;
  const mode = modeProp ?? modeState;
  const [rows, setRows] = useState<Row[]>(() => parse(value));
  const [shown, setShown] = useState<Record<number, boolean>>({});

  // 从文本切回按行编辑时重新解析。
  const prevMode = useRef(mode);
  useEffect(() => {
    if (prevMode.current === "text" && mode === "rows") setRows(parse(value));
    prevMode.current = mode;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode]);

  const update = (next: Row[]) => {
    setRows(next);
    onChange(serialize(next));
  };
  const add = (
    <Button size="sm" variant="ghost" className={cn("max-md:text-sm", !controlled && "-ml-2 px-2")} onClick={() => setRows([...rows, { id: nextId++, key: "", value: "" }])}>
      <Plus />
      {t.containerForm.envAdd}
    </Button>
  );

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
          className="min-h-32 font-mono text-[13px]"
        />
      ) : (
        rows.map((r, i) => {
          const secret = sensitive.test(r.key);
          const masked = secret && !shown[r.id];
          const keyError = r.key.trim() ? checkEnv(`${r.key.trim()}=`) : "";
          const mono = controlled && "font-mono text-[13px]";
          return (
            <div key={r.id} className={cn("grid min-w-0 items-center max-md:gap-2", controlled ? "grid-cols-[minmax(0,1fr)_minmax(0,2fr)_2rem] gap-2 max-md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2.5rem]" : "grid-cols-2 gap-4")}>
              <Input
                id={i === 0 ? id : undefined}
                value={r.key}
                aria-label={t.containerForm.envKey}
                aria-invalid={!!keyError}
                placeholder={t.containerForm.envKey}
                spellCheck={false}
                className={cn("min-w-0", mono)}
                onChange={(e) => {
                  e.target.setCustomValidity(e.target.value.trim() ? checkEnv(`${e.target.value.trim()}=`) : "");
                  update(rows.map((x) => (x.id === r.id ? { ...x, key: e.target.value } : x)));
                }}
              />
              <div className={cn("relative flex min-w-0 items-center", controlled && "contents")}>
                <div className="relative min-w-0 flex-1">
                  <Input
                    value={r.value}
                    type={masked ? "password" : "text"}
                    autoComplete="off"
                    aria-label={t.containerForm.envValue}
                    placeholder={t.containerForm.envValue}
                    spellCheck={false}
                    className={cn("min-w-0", mono, secret && "pr-8")}
                    onChange={(e) => update(rows.map((x) => (x.id === r.id ? { ...x, value: e.target.value } : x)))}
                  />
                  {secret && (
                    <Button
                      size="icon"
                      variant="ghost"
                      className="absolute top-0 right-0 size-8 text-muted-foreground max-md:size-10"
                      aria-label={masked ? t.containerForm.envShow : t.containerForm.envHide}
                      title={masked ? t.containerForm.envShow : t.containerForm.envHide}
                      onClick={() => setShown({ ...shown, [r.id]: masked })}
                    >
                      {masked ? <Eye /> : <EyeOff />}
                    </Button>
                  )}
                </div>
                <Button
                  size="icon"
                  variant="ghost"
                  className={cn("size-8 text-muted-foreground max-md:size-10", !controlled && "-mr-2 max-md:-mr-3")}
                  aria-label={t.common.actions.delete}
                  title={t.common.actions.delete}
                  onClick={() => update(rows.filter((x) => x.id !== r.id))}
                >
                  <X />
                </Button>
              </div>
            </div>
          );
        })
      )}
      {controlled ? (
        mode === "rows" && <div>{add}</div>
      ) : (
        <div className="-mt-1 flex items-center">
          {mode === "rows" && add}
          <Button
            size="sm"
            variant="ghost"
            className={cn("px-2 max-md:text-sm", mode === "text" && "-ml-2")}
            onClick={() => {
              if (mode === "text") setRows(parse(value));
              setModeState(mode === "rows" ? "text" : "rows");
            }}
          >
            {mode === "rows" ? t.containerForm.envBulk : t.containerForm.envRows}
          </Button>
        </div>
      )}
    </div>
  );
}
