// 基础组件：灰阶、细边框、紧凑的控制台风格（参照 shadcn/ui）。
import { cva, type VariantProps } from "class-variance-authority";
import { createContext, useContext, useEffect, useRef, useState } from "react";
import type * as React from "react";
import { Check as CheckIcon, CircleHelp, Copy, Ellipsis, Inbox, X } from "lucide-react";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

// ---- 按钮与输入 ----

const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer items-center justify-center gap-1.5 rounded-md text-sm font-medium whitespace-nowrap transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground hover:bg-primary/88",
        outline: "border border-input bg-card hover:bg-accent",
        ghost: "text-muted-foreground hover:bg-accent hover:text-foreground",
        destructive: "bg-destructive text-white hover:bg-destructive/90",
        "destructive-outline": "border border-destructive/40 bg-card text-destructive hover:bg-destructive/8",
      },
      size: {
        default: "h-9 px-3.5",
        sm: "h-8 px-3 text-[13px] [&_svg]:size-3.5",
        xs: "h-7 px-2 text-xs [&_svg]:size-3.5",
        icon: "size-8",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

// ReadOnlyContext：备用面板上为 true，按钮默认置灰（只读的按钮加 readOnlyOk）。见 docs/standby.md。
export const ReadOnlyContext = createContext(false);

export function Button({
  className,
  variant,
  size,
  type = "button",
  readOnlyOk,
  disabled,
  title,
  ...props
}: React.ComponentProps<"button"> & VariantProps<typeof buttonVariants> & { readOnlyOk?: boolean }) {
  const readOnly = useContext(ReadOnlyContext) && !readOnlyOk;
  return (
    <button
      type={type}
      className={cn(buttonVariants({ variant, size }), className)}
      disabled={disabled || readOnly}
      title={readOnly ? t.common.readOnlyHint : title}
      {...props}
    />
  );
}

const control =
  "w-full rounded-md border border-input bg-card px-3 text-sm outline-none transition-[border-color,box-shadow] placeholder:text-muted-foreground/70 focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/15 disabled:cursor-not-allowed disabled:opacity-60";

export function Input({ className, ...props }: React.ComponentProps<"input">) {
  return <input className={cn(control, "h-9", className)} {...props} />;
}

export function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return <textarea className={cn(control, "min-h-20 py-2 leading-relaxed", className)} {...props} />;
}

export function Select({ className, ...props }: React.ComponentProps<"select">) {
  return <select className={cn(control, "h-9 pr-8", className)} {...props} />;
}

export function Label({ className, ...props }: React.ComponentProps<"label">) {
  return <label className={cn("text-[13px] font-medium", className)} {...props} />;
}

// HelpTip：字段或标题旁的问号，悬停或聚焦时显示详细说明。
// 说明框不显示时不占位置；靠近窗口边缘时改为向内对齐，不会撑出横向滚动条。
export function HelpTip({ children, className }: { children: React.ReactNode; className?: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  const [align, setAlign] = useState<"center" | "left" | "right">("center");
  const place = () => {
    const r = ref.current?.getBoundingClientRect();
    if (!r) return;
    const half = 150; // 说明框最宽 18rem 的一半左右
    const mid = r.left + r.width / 2;
    setAlign(mid - half < 8 ? "left" : mid + half > window.innerWidth - 8 ? "right" : "center");
  };
  const pos = align === "left" ? "left-0" : align === "right" ? "right-0" : "left-1/2 -translate-x-1/2";
  return (
    <span ref={ref} className={cn("group/help relative inline-flex align-middle", className)} onMouseEnter={place} onFocus={place}>
      <span tabIndex={0} className="inline-flex cursor-help text-muted-foreground/70 outline-none hover:text-foreground focus-visible:text-foreground">
        <CircleHelp className="size-3.5" />
      </span>
      <span
        role="tooltip"
        className={cn(
          "pointer-events-none absolute bottom-full z-50 mb-2 hidden w-max max-w-72 rounded-md border bg-card px-2.5 py-1.5 text-xs leading-relaxed font-normal text-foreground shadow-lg group-hover/help:block group-focus-within/help:block",
          pos,
        )}
      >
        {children}
      </span>
    </span>
  );
}

// Field：一个表单字段。label 下面是输入框；hint 是一行简短的格式说明；help 收进问号提示。
export function Field({
  label,
  hint,
  help,
  htmlFor,
  className,
  error,
  children,
}: {
  label: React.ReactNode;
  hint?: React.ReactNode;
  help?: React.ReactNode;
  htmlFor?: string;
  className?: string;
  // error：填写有误时的说明，显示在格式说明的位置。
  error?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("grid content-start gap-1.5", className)}>
      <div className="flex items-center gap-1.5">
        <Label htmlFor={htmlFor}>{label}</Label>
        {help && <HelpTip>{help}</HelpTip>}
      </div>
      {children}
      {error ? <p className="text-xs text-destructive">{error}</p> : hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

// Check：带文字的复选框。
export function Check({
  checked,
  onChange,
  disabled,
  children,
  help,
  className,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  children: React.ReactNode;
  help?: React.ReactNode;
  className?: string;
}) {
  return (
    <span className={cn("inline-flex items-center gap-1.5", className)}>
      <label className={cn("inline-flex cursor-pointer items-center gap-2 text-sm", disabled && "cursor-not-allowed opacity-60")}>
        <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
        {children}
      </label>
      {help && <HelpTip>{help}</HelpTip>}
    </span>
  );
}

// FormBody：区块里的表单，最宽 42rem，字段之间 16px。不要让字段撑满宽屏，按钮也就不会离字段太远。
export function FormBody({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid max-w-2xl gap-4", className)} {...props} />;
}

// FormRow：一行两个字段，等宽；窄屏上下排。
export function FormRow({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid gap-4 sm:grid-cols-2", className)} {...props} />;
}

// FormActions：表单底部的按钮，在表单宽度内靠右（和对话框一致）；主要操作放最后。提示（已保存……）放在按钮左边。
export function FormActions({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("flex flex-wrap items-center justify-end gap-2", className)} {...props} />;
}

// ---- 区块 ----

// Section：页面上的一组内容。不加外框：标题一行、可选的一句描述和右侧操作，组与组之间用一条细分隔线隔开
// （参照 OpenAI 平台后台：靠标题、留白和分隔线分组，不用卡片）。flush 时内容直接是表格。
export function Section({
  title,
  description,
  help,
  actions,
  flush,
  tone,
  className,
  children,
}: {
  title?: React.ReactNode;
  description?: React.ReactNode;
  help?: React.ReactNode;
  actions?: React.ReactNode;
  flush?: boolean;
  tone?: "danger";
  className?: string;
  children?: React.ReactNode;
}) {
  return (
    <section className={cn("border-t pt-6 first-of-type:border-t-0 first-of-type:pt-0", className)}>
      {(title || actions) && (
        <header className="mb-4 flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
          <div className="min-w-0">
            <h2 className={cn("flex items-center gap-1.5 text-[15px] font-semibold", tone === "danger" && "text-destructive")}>
              {title}
              {help && <HelpTip>{help}</HelpTip>}
            </h2>
            {description && <p className="mt-1 text-[13px] text-pretty text-muted-foreground">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </header>
      )}
      {children !== undefined && children !== null && children !== false && (
        <div
          className={cn(
            flush && "-mt-1",
            // 区块以表格结尾时，把区块之间的 24px 整个收回去：下一个区块的分隔线正好落在最后一行底部，
            // 和其他行线间隔一致（否则它看起来像最后一行的底线，只是离得更远）。表格可以包在一层 div 里。
            "[&>:last-child>[data-slot=table]:last-child]:-mb-6 [&>[data-slot=table]:last-child]:-mb-6",
          )}
        >
          {children}
        </div>
      )}
    </section>
  );
}

// SettingRow：设置里的一项，左边名称和说明，右边控件；行与行之间细分隔线。放在 SettingRows 里。
export function SettingRow({ label, description, help, htmlFor, children }: { label: React.ReactNode; description?: React.ReactNode; help?: React.ReactNode; htmlFor?: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-x-8 gap-y-2 border-b py-5 first:pt-0 last:border-b-0 last:pb-0 md:grid-cols-2">
      <div className="min-w-0">
        <div className="flex items-center gap-1.5 text-sm font-medium">
          {htmlFor ? <label htmlFor={htmlFor}>{label}</label> : label}
          {help && <HelpTip>{help}</HelpTip>}
        </div>
        {description && <p className="mt-1 text-[13px] text-pretty text-muted-foreground">{description}</p>}
      </div>
      <div className="min-w-0">{children}</div>
    </div>
  );
}

export function SettingRows({ children }: { children: React.ReactNode }) {
  return <div className="grid">{children}</div>;
}

// ---- 卡片 ----

// Card：概览网格里的一项（项目、节点），整张可以是链接。四边内边距 16px，头、内容、底栏之间 12px。
// 其他地方不用卡片（见 docs/ui.md）；监控数值块用 Tile。
export const cardClass =
  "group flex flex-col rounded-lg border bg-card outline-none transition-colors hover:border-foreground/25 hover:bg-accent/30 focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/15";

export function Card({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn(cardClass, className)} {...props} />;
}

// CardHeader：标题和副标题在左，状态在右。
export function CardHeader({ title, subtitle, aside }: { title: React.ReactNode; subtitle?: React.ReactNode; aside?: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-3 px-4 pt-4">
      <div className="grid min-w-0 gap-1">
        <div className="flex min-w-0 items-center gap-2 text-sm font-semibold">{title}</div>
        {subtitle && <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">{subtitle}</div>}
      </div>
      {aside && <div className="shrink-0">{aside}</div>}
    </div>
  );
}

export function CardBody({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid flex-1 content-start gap-2 px-4 pt-3 pb-4", className)} {...props} />;
}

export function CardFooter({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("flex items-center justify-between gap-3 border-t px-4 py-2.5 text-xs text-muted-foreground", className)} {...props} />;
}

// CardGrid：卡片的网格，每张至少 18rem，卡片之间 16px。
export function CardGrid({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid grid-cols-[repeat(auto-fill,minmax(min(100%,18rem),1fr))] gap-4", className)} {...props} />;
}

// Tile：监控数值块。浅底、无边框，不是卡片（不可点）。min-w-0：里面的图表不能撑破网格。
export function Tile({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid min-w-0 gap-1 rounded-md bg-subtle px-3 py-3", className)} {...props} />;
}

// TileGrid：按所在区域的宽度排列（不是窗口宽度：宽屏的项目页内容区只有窗口的一半多），每块至少 9.5rem。
export function TileGrid({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid grid-cols-[repeat(auto-fit,minmax(min(100%,9.5rem),1fr))] gap-3", className)} {...props} />;
}

// ---- 表格 ----

// 长列表（快照、版本、部署记录）用 compact，行高 40px；其余 44px。
const TableDensity = createContext(false);

// 最后一行没有底线，文字下面还有半行空白（44px 行 12px，40px 行 10px）。表格后面还有内容时（例如下面是表单），
// 用负外边距抵掉这段空白；表格在区块末尾时由 Section 处理（下一条分隔线直接当最后一行的底线）。
export function Table({ className, compact = false, ...props }: React.ComponentProps<"table"> & { compact?: boolean }) {
  return (
    <div data-slot="table" className={cn("overflow-x-auto", compact ? "last:-mb-2.5" : "last:-mb-3")}>
      <TableDensity.Provider value={compact}>
        <table className={cn("w-full text-sm", className)} {...props} />
      </TableDensity.Provider>
    </div>
  );
}

export function Th({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      className={cn("h-9 border-b px-3 text-left text-xs font-medium whitespace-nowrap text-muted-foreground first:pl-0 last:pr-0", className)}
      {...props}
    />
  );
}

export function Td({ className, ...props }: React.ComponentProps<"td">) {
  const compact = useContext(TableDensity);
  return (
    <td
      className={cn("border-b px-3 py-2 align-middle first:pl-0 last:pr-0 [tr:last-child>&]:border-b-0", compact ? "h-10 py-1.5" : "h-11", className)}
      {...props}
    />
  );
}

// ---- 状态与提示 ----

const badgeVariants = cva("inline-flex shrink-0 items-center gap-1 rounded-sm border px-1.5 py-px text-[11px] leading-4 font-medium whitespace-nowrap", {
  variants: {
    tone: {
      neutral: "border-border bg-muted text-muted-foreground",
      success: "border-success/25 bg-success/10 text-success",
      warning: "border-warning/35 bg-warning/12 text-[color-mix(in_oklab,var(--warning)_75%,var(--foreground))]",
      danger: "border-destructive/25 bg-destructive/10 text-destructive",
      info: "border-info/25 bg-info/10 text-info",
    },
  },
  defaultVariants: { tone: "neutral" },
});

export function Badge({
  className,
  tone,
  dot,
  ...props
}: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants> & { dot?: boolean }) {
  return (
    <span className={cn(badgeVariants({ tone }), className)} {...props}>
      {dot && <span className="size-1.5 rounded-full bg-current" />}
      {props.children}
    </span>
  );
}

export const dotColor = {
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-destructive",
  info: "bg-info",
  neutral: "bg-muted-foreground/50",
};

// Status：状态点加文字，用在表格和标题里。
export function Status({ tone = "neutral", children, pulse }: { tone?: keyof typeof dotColor; children: React.ReactNode; pulse?: boolean }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-[13px] whitespace-nowrap">
      <span className="relative flex size-2">
        {pulse && <span className={cn("absolute inline-flex size-full rounded-full opacity-60 motion-safe:animate-ping", dotColor[tone])} />}
        <span className={cn("relative inline-flex size-2 rounded-full", dotColor[tone])} />
      </span>
      {children}
    </span>
  );
}

const noticeTone = {
  danger: "border-destructive/30 bg-destructive/6 text-destructive",
  warning: "border-warning/40 bg-warning/8 text-foreground",
  info: "border-border bg-subtle text-foreground",
  success: "border-success/30 bg-success/6 text-foreground",
};

// Notice：页面或区块里的提示条。
export function Notice({
  tone = "info",
  title,
  className,
  children,
  actions,
}: {
  tone?: keyof typeof noticeTone;
  title?: React.ReactNode;
  className?: string;
  children?: React.ReactNode;
  actions?: React.ReactNode;
}) {
  return (
    <div role={tone === "danger" ? "alert" : "status"} className={cn("flex flex-wrap items-start gap-x-4 gap-y-2 rounded-md border px-3.5 py-2.5 text-[13px] leading-relaxed", noticeTone[tone], className)}>
      <div className="min-w-0 flex-1 text-pretty">
        {title && <div className="font-medium">{title}</div>}
        {children && <div className={cn(title && "mt-0.5 text-muted-foreground", tone === "danger" && "text-destructive")}>{children}</div>}
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
    </div>
  );
}

// Alert：错误提示（Notice 的简写）。
export function Alert({ className, children }: { className?: string; children?: React.ReactNode }) {
  return (
    <Notice tone="danger" className={className}>
      {children}
    </Notice>
  );
}

export function Spinner({ className }: { className?: string }) {
  return <span className={cn("inline-block size-4 animate-spin rounded-full border-2 border-current border-r-transparent", className)} aria-label={t.common.feedback.loading} />;
}

// Loading：整块区域加载中。
export function Loading() {
  return (
    <div className="flex items-center justify-center py-16 text-muted-foreground">
      <Spinner />
    </div>
  );
}

// Empty：空状态。
export function Empty({ title, children, action, icon }: { title: string; children?: React.ReactNode; action?: React.ReactNode; icon?: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-16 text-center">
      <div className="mb-1 flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground">{icon ?? <Inbox className="size-5" />}</div>
      <p className="text-sm font-medium">{title}</p>
      {children && <div className="max-w-md text-[13px] text-muted-foreground">{children}</div>}
      {action && <div className="mt-3">{action}</div>}
    </div>
  );
}

// EmptyRow：区块里没有内容时的一行说明。
export function EmptyRow({ children }: { children: React.ReactNode }) {
  return <p className="py-6 text-[13px] text-muted-foreground">{children}</p>;
}

// ---- 文本 ----

// KeyValue：两列的属性列表。
export function KeyValue({ items, className }: { items: [React.ReactNode, React.ReactNode][]; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[minmax(6rem,auto)_1fr] gap-x-6 gap-y-2 text-[13px]", className)}>
      {items.map(([k, v], i) => (
        <div key={i} className="contents">
          <dt className="text-muted-foreground">{k}</dt>
          <dd className="min-w-0 break-words">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

// CopyButton：复制文字到剪贴板。
export function CopyButton({ text, className }: { text: string; className?: string }) {
  const [done, setDone] = useState(false);
  return (
    <Button
      readOnlyOk
      size="icon"
      variant="ghost"
      className={cn("size-7", className)}
      aria-label={t.common.actions.copy}
      title={done ? t.common.feedback.copied : t.common.actions.copy}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } catch {
          // 非 HTTPS 等情况下浏览器不让写剪贴板，只能手动选中复制。
        }
      }}
    >
      {done ? <CheckIcon className="size-3.5" /> : <Copy className="size-3.5" />}
    </Button>
  );
}

// CodeBlock：一段等宽文字（命令、密钥、地址），右上角可复制。
export function CodeBlock({ children, copy, className }: { children: string; copy?: boolean; className?: string }) {
  return (
    <div className={cn("relative rounded-md border bg-subtle", className)}>
      <code className={cn("block px-3 py-2.5 font-mono text-xs leading-relaxed break-all", copy && "pr-10")}>{children}</code>
      {copy && <CopyButton text={children} className="absolute top-1.5 right-1.5" />}
    </div>
  );
}

// LogBox：日志和命令输出的深色区域。
export function LogBox({ className, ...props }: React.ComponentProps<"pre">) {
  return <pre className={cn("overflow-auto rounded-md bg-log p-3.5 font-mono text-xs leading-relaxed text-log-foreground", className)} {...props} />;
}

// ---- 标签页 ----

export function Tabs<T extends string>({
  value,
  onChange,
  items,
  className,
}: {
  value: T;
  onChange: (v: T) => void;
  items: { value: T; label: React.ReactNode; count?: number }[];
  className?: string;
}) {
  return (
    <div role="tablist" className={cn("flex gap-5 overflow-x-auto border-b [scrollbar-width:none]", className)}>
      {items.map((it) => (
        <button
          key={it.value}
          role="tab"
          type="button"
          aria-selected={value === it.value}
          onClick={() => onChange(it.value)}
          className={cn(
            "inline-flex h-10 shrink-0 cursor-pointer items-center gap-1.5 text-sm whitespace-nowrap text-muted-foreground transition-colors hover:text-foreground",
            value === it.value && "font-medium text-foreground shadow-[inset_0_-2px_0_var(--foreground)]",
          )}
        >
          {it.label}
          {it.count !== undefined && it.count > 0 && <span className="rounded-sm bg-muted px-1.5 text-[11px] text-muted-foreground">{it.count}</span>}
        </button>
      ))}
    </div>
  );
}

// SegmentedControl：两三个选项之间切换（例如"容器 / 容器组"）。
export function Segmented<T extends string>({ value, onChange, items }: { value: T; onChange: (v: T) => void; items: { value: T; label: React.ReactNode }[] }) {
  return (
    <div className="inline-flex rounded-md border bg-muted p-0.5">
      {items.map((it) => (
        <button
          key={it.value}
          type="button"
          onClick={() => onChange(it.value)}
          className={cn(
            "h-7 cursor-pointer rounded-[5px] px-3 text-[13px] text-muted-foreground transition-colors",
            value === it.value && "bg-card font-medium text-foreground shadow-xs",
          )}
        >
          {it.label}
        </button>
      ))}
    </div>
  );
}

// 品牌标识：两道平缓的波浪，和 public/favicon.svg 同一形状（去掉底板）。
// 下面一道用 currentColor（品牌色），上面一道用前景色、半透明，深浅色下都能看清。
// 放在另一个 SVG 里时用 x、y、width、height 定位（CSS 尺寸对嵌套的 svg 不生效）。
export function Waves({ className, ...props }: React.ComponentProps<"svg">) {
  return (
    <svg viewBox="4 9 24 14" fill="none" className={className} aria-hidden {...props}>
      <path d="M6 19c3-3 6-3 10 0s7 3 10 0" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" />
      <path d="M6 13c3-3 6-3 10 0s7 3 10 0" stroke="var(--foreground)" strokeWidth="2.4" strokeLinecap="round" opacity=".55" />
    </svg>
  );
}

// ---- 对话框、菜单、占用条 ----

// Dialog：居中的对话框。点遮罩或按 Esc 关闭。
export function Dialog({
  open,
  onClose,
  title,
  description,
  children,
  className,
}: {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/50 px-4 py-16" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div role="dialog" aria-modal className={cn("w-full max-w-lg rounded-lg border bg-card shadow-xl", className)}>
        <div className="flex items-start justify-between gap-4 px-5 pt-4 pb-3">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold">{title}</h2>
            {description && <p className="mt-0.5 text-[13px] text-muted-foreground">{description}</p>}
          </div>
          <Button size="icon" variant="ghost" className="-mr-2 -mt-1 size-7" onClick={onClose} aria-label={t.common.actions.close} readOnlyOk>
            <X />
          </Button>
        </div>
        <div className="px-5 pb-5">{children}</div>
      </div>
    </div>
  );
}

// Menu：「更多」按钮和下拉菜单。
export function Menu({ items, label = t.common.actions.more }: { items: { label: React.ReactNode; onSelect: () => void; danger?: boolean }[]; label?: string }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => ref.current && !ref.current.contains(e.target as Node) && setOpen(false);
    window.addEventListener("mousedown", onDown);
    return () => window.removeEventListener("mousedown", onDown);
  }, [open]);
  return (
    <div ref={ref} className="relative">
      <Button variant="outline" size="icon" aria-label={label} aria-expanded={open} onClick={() => setOpen(!open)}>
        <Ellipsis />
      </Button>
      {open && (
        <div role="menu" className="absolute right-0 z-40 mt-1 min-w-40 rounded-md border bg-card py-1 shadow-lg">
          {items.map((it, i) => (
            <button
              key={i}
              role="menuitem"
              className={cn("block w-full px-3 py-1.5 text-left text-[13px] hover:bg-accent", it.danger && "text-destructive")}
              onClick={() => {
                setOpen(false);
                it.onSelect();
              }}
            >
              {it.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// Meter：资源占用条。超过 90% 红色，超过 75% 琥珀色。
export function Meter({ label, value }: { label: string; value?: number }) {
  const v = value === undefined ? undefined : Math.max(0, Math.min(100, value));
  const tone = v === undefined ? "bg-muted-foreground/30" : v >= 90 ? "bg-destructive" : v >= 75 ? "bg-warning" : "bg-success";
  return (
    <div className="grid grid-cols-[2.5rem_1fr_2.75rem] items-center gap-2 text-xs">
      <span className="text-muted-foreground">{label}</span>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-[width]", tone)} style={{ width: `${v ?? 0}%` }} />
      </div>
      <span className="text-right font-mono tabular-nums text-muted-foreground">{v === undefined ? "—" : `${v.toFixed(0)}%`}</span>

    </div>
  );
}

