// 基础组件：灰阶、细边框、紧凑的控制台风格（参照 shadcn/ui）。
import { cva, type VariantProps } from "class-variance-authority";
import { Children, cloneElement, createContext, isValidElement, useContext, useEffect, useRef, useState, useSyncExternalStore } from "react";
import type * as React from "react";
import { createLink } from "@tanstack/react-router";
import { Check as CheckIcon, CircleHelp, Copy, Ellipsis, X } from "lucide-react";
import { toast } from "sonner";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog as DialogRoot, DialogClose, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Fragment } from "react";
import { Label } from "@/components/ui/label";
import { Select as SelectRoot, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs as TabsRoot, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

export { Label };

// useIsMobile：手机宽度（< 768px，和 Tailwind 的 max-md 同一条线）。只给结构不同的地方用，样式差异写 max-md:。
const mobileQuery = "(max-width: 767.98px)";
export function useIsMobile() {
  return useSyncExternalStore(
    (notify) => {
      const media = matchMedia(mobileQuery);
      media.addEventListener("change", notify);
      return () => media.removeEventListener("change", notify);
    },
    () => matchMedia(mobileQuery).matches,
  );
}

// ---- 按钮与输入 ----

const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer items-center justify-center gap-1 rounded-md border border-transparent text-sm whitespace-nowrap transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:cursor-not-allowed disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "bg-primary font-medium text-primary-foreground not-disabled:hover:bg-primary/88",
        outline: "border-input bg-background not-disabled:hover:bg-accent",
        ghost: "not-disabled:hover:bg-accent",
        destructive: "bg-destructive text-white not-disabled:hover:bg-destructive/90",
        "destructive-outline": "border-input bg-background text-destructive not-disabled:hover:bg-accent",
      },
      size: {
        // 手机上控件高 40、字号 16（iOS 聚焦不缩放），其余 32。
        default: "h-8 px-3 max-md:h-10 max-md:text-base",
        sm: "h-8 px-3 max-md:h-10 max-md:text-base",
        xs: "h-7 px-2 text-[13px]",
        icon: "size-8 max-md:size-10",
        // row：列表行里的无框按钮。文字贴齐行尾，高度不撑开行。
        row: "-my-1.5 h-8 px-2 last:-mr-2 max-md:-my-2.5 max-md:h-10",
        rowIcon: "-my-1.5 size-8 text-muted-foreground last:-mr-2 max-md:-my-2.5 max-md:size-10 max-md:last:-mr-3",
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

// ButtonLink：外观是按钮的链接，不要在 Link 里再套 Button。只读时显示成置灰的按钮；只是跳转页面的加 readOnlyOk。
export const ButtonLink = createLink(function ButtonLink({
  className,
  variant,
  size,
  readOnlyOk,
  children,
  ...props
}: React.ComponentProps<"a"> & VariantProps<typeof buttonVariants> & { readOnlyOk?: boolean }) {
  const readOnly = useContext(ReadOnlyContext) && !readOnlyOk;
  if (readOnly) {
    return (
      <Button variant={variant} size={size} className={className}>
        {children}
      </Button>
    );
  }
  return (
    <a className={cn(buttonVariants({ variant, size }), className)} {...props}>
      {children}
    </a>
  );
});

const control =
  "w-full rounded-md border border-input bg-transparent px-3 text-sm outline-none transition-[border-color,box-shadow] placeholder:text-muted-foreground aria-[invalid=true]:border-destructive focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/15 disabled:cursor-not-allowed disabled:opacity-60 max-md:text-base";

export function Input({ className, ...props }: React.ComponentProps<"input">) {
  return <input className={cn(control, "h-8 max-md:h-10", className)} {...props} />;
}

export function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return <textarea className={cn(control, "min-h-8 py-1.5 leading-5", className)} {...props} />;
}

// Select 的空值：shadcn 的选项不能用空字符串作值，内部换成这个。
const emptyValue = "__empty__";

// 把调用方写的 <option> 展开成选项列表（可能包在 Fragment 或数组里）。
function selectOptions(children: React.ReactNode): { value: string; label: React.ReactNode; disabled?: boolean }[] {
  return Children.toArray(children).flatMap((c) => {
    if (!isValidElement<{ value?: string; children?: React.ReactNode; disabled?: boolean }>(c)) return [];
    if (c.type !== "option") return selectOptions(c.props.children);
    return [{ value: c.props.value ?? String(c.props.children), label: c.props.children, disabled: c.props.disabled }];
  });
}

// Select：下拉选择。写法和原生 select 一样（<option> 作子元素、onChange 读 e.target.value），外观和交互用 shadcn。
export function Select({
  id,
  value,
  onChange,
  required,
  disabled,
  className,
  children,
  "aria-label": ariaLabel,
}: {
  id?: string;
  "aria-label"?: string;
  value: string;
  onChange?: (e: { target: { value: string } }) => void;
  required?: boolean;
  disabled?: boolean;
  className?: string;
  children: React.ReactNode;
}) {
  const options = selectOptions(children);
  // 必填又没有值（例如没有在线节点）时不给内部值，表单校验才拦得住；空值选项的文字作为占位显示。
  const unset = required && value === "";
  return (
    <SelectRoot value={unset ? "" : value === "" ? emptyValue : value} onValueChange={(v) => v && onChange?.({ target: { value: v === emptyValue ? "" : v } })} required={required} disabled={disabled}>
      <SelectTrigger id={id} className={className} aria-label={ariaLabel}>
        <SelectValue placeholder={unset ? options.find((o) => o.value === "")?.label : undefined} />
      </SelectTrigger>
      <SelectContent>
        {options.map((o) => (
          <SelectItem key={o.value} value={o.value === "" ? emptyValue : o.value} disabled={o.disabled}>
            {o.label}
          </SelectItem>
        ))}
      </SelectContent>
    </SelectRoot>
  );
}

// HelpTip：字段或标题旁的问号，悬停或聚焦时显示详细说明；触屏上点一下显示。靠近窗口边缘时自动向内避让。
export function HelpTip({ children, className }: { children: React.ReactNode; className?: string }) {
  const [open, setOpen] = useState(false);
  return (
    <TooltipProvider delayDuration={100}>
      <Tooltip open={open} onOpenChange={setOpen}>
        <TooltipTrigger
          type="button"
          className={cn("inline-flex cursor-help rounded-full align-middle text-muted-foreground/70 outline-none hover:text-foreground focus-visible:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40", className)}
          onClick={(e) => {
            e.preventDefault();
            setOpen(true);
          }}
        >
          <CircleHelp className="size-3.5" />
        </TooltipTrigger>
        <TooltipContent>{children}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
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
  // required：必填，标签后面显示星号。
  required?: boolean;
  children: React.ReactNode;
}) {
  const errorId = error && htmlFor ? `${htmlFor}-error` : undefined;
  return (
    <div className={cn("grid min-w-0 content-start gap-2", className)}>
      <div className="flex items-center gap-1">
        <Label htmlFor={htmlFor}>
          {label}
          
        </Label>
        {help && <HelpTip>{help}</HelpTip>}
      </div>
      {error && isValidElement<{ "aria-invalid"?: boolean; "aria-describedby"?: string }>(children)
        ? cloneElement(children, { "aria-invalid": true, "aria-describedby": errorId })
        : children}
      {error ? <p id={errorId} role="alert" className="leading-5 text-destructive">{error}</p> : hint && <p className="leading-5 text-muted-foreground">{hint}</p>}
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
    <span className={cn("inline-flex items-center gap-1", className)}>
      <label className={cn("inline-flex cursor-pointer items-center gap-2 text-sm", disabled && "cursor-not-allowed opacity-60")}>
        <Checkbox checked={checked} disabled={disabled} onCheckedChange={(v) => onChange(v === true)} />
        {children}
      </label>
      {help && <HelpTip>{help}</HelpTip>}
    </span>
  );
}

// FormBody：区块里的表单，最宽 672，靠左，字段之间 16。
export function FormBody({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid max-w-[672px] gap-4", className)} {...props} />;
}

// FormRow：一行两个字段，等宽；窄屏上下排。
export function FormRow({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid gap-4 md:grid-cols-2", className)} {...props} />;
}

// FormActions：区块表单底部的按钮，靠左和字段对齐，主要操作在最前。对话框里用 DialogActions。
export function FormActions({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("flex flex-wrap items-center gap-2", className)} {...props} />;
}

// ---- 区块 ----

// DialogActions：对话框底部的按钮，靠右，主要操作在最后；手机上等宽撑满。
export function DialogActions({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("flex items-center justify-end gap-2 max-md:[&>*]:flex-1", className)} {...props} />;
}

// Section：页面上的一组内容，没有外框。区块之间一条细线，线上下各 24，所以父级不要再加间距（Sections 排列）。
// 说明放进标题旁的问号（help），不占一行；description 只给确实需要一直看到的话。
export function Section({
  title,
  description,
  help,
  actions,
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
    <section className={cn("grid gap-4 border-t pt-6 mt-6 first:mt-0 first:border-t-0 first:pt-0 max-md:mt-4 max-md:gap-2 max-md:pt-4", className)}>
      {(title || actions) && (
        <header className="flex min-h-8 flex-wrap items-center justify-between gap-x-4 gap-y-2 max-md:min-h-10 max-md:gap-y-0">
          <div className="min-w-0 max-md:grow">
            <h2 className={cn("flex items-center gap-1 text-base font-semibold", tone === "danger" && "text-destructive")}>
              {title}
              {help && <HelpTip>{help}</HelpTip>}
            </h2>
            {description && <p className="mt-1 text-pretty text-muted-foreground">{description}</p>}
          </div>
          {/* 手机上灰字说明换到标题下面一行，按钮留在标题右边。 */}
          {actions && <div className="flex flex-wrap items-center gap-3 max-md:contents [&>span]:text-[13px] [&>span]:text-muted-foreground max-md:[&>span]:order-1 max-md:[&>span]:w-full max-md:[&>span]:leading-5">{actions}</div>}
        </header>
      )}
      {children}
    </section>
  );
}

// ---- 列表行 ----

// List / Row：细分页的基本单位（见 docs/ui.md「列表行」）。主信息在左，灰色信息是靠右的定宽列，最后是操作。
// 所有内容对齐第一行；悬停整行变底色，所以 List 左右各外扩 8，让文字和页面边缘对齐。
export function List({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("-mx-2 grid", className)} {...props} />;
}

export type RowCol = {
  content: React.ReactNode;
  // num：数字靠右。phone：手机上留在第一行（状态），其余并进第二行。
  num?: boolean;
  phone?: boolean;
  // width：内容较长的列换一个宽度，放不下时截断。
  width?: string;
};

export function Row({
  title,
  sub,
  cols,
  actions,
  icon,
  center,
  actionsWidth = "w-20",
  className,
}: {
  title: React.ReactNode;
  sub?: React.ReactNode;
  cols?: RowCol[];
  // actions：传 null 也占位，同一个列表里的列才对得齐；不传就没有这一列。
  actions?: React.ReactNode;
  icon?: React.ReactNode;
  // center：设置行，内容上下居中，操作是有框按钮。
  center?: boolean;
  actionsWidth?: string;
  className?: string;
}) {
  const merged = (cols ?? []).filter((c) => !c.phone && c.content !== undefined && c.content !== null && c.content !== "");
  return (
    <div className={cn("flex gap-4 rounded-md px-2 transition-colors hover:bg-accent max-md:gap-2 max-md:py-2.5", sub ? "min-h-13 py-1.5 max-md:min-h-[60px]" : merged.length > 0 ? "min-h-9 max-md:min-h-[60px]" : "min-h-9 max-md:min-h-11", center ? "items-center" : "items-start", !sub && !center && "py-2 max-md:py-3", !sub && center && "min-h-10 py-1", className)}>
      {icon && <span className="mt-0.5 flex shrink-0 text-muted-foreground [&_svg]:size-4">{icon}</span>}
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2 leading-5">{title}</div>
        {sub && <div className="text-[13px] leading-5 text-muted-foreground max-md:hidden">{sub}</div>}
        {(sub || merged.length > 0) && (
          <div className="text-[13px] leading-5 wrap-anywhere text-muted-foreground md:hidden">
            {sub}
            {merged.map((c, i) => (
              <span key={i}>
                {(sub || i > 0) && " · "}
                {c.content}
              </span>
            ))}
          </div>
        )}
      </div>
      {(cols ?? []).map((c, i) => (
        <span key={i} className={cn("shrink-0 leading-5 whitespace-nowrap text-muted-foreground", c.width ? cn(c.width, "truncate") : "w-24", c.num && "text-right", c.phone ? "max-md:w-auto" : "max-md:hidden")}>
          {c.content}
        </span>
      ))}
      {actions !== undefined && <span className={cn("flex shrink-0 items-center justify-end leading-5 whitespace-nowrap max-md:w-auto max-md:empty:hidden", !center && "h-5", actionsWidth)}>{actions}</span>}
    </div>
  );
}

// RowMenu：列表行末尾的 ⋮。
export function RowMenu(props: React.ComponentProps<typeof Menu>) {
  return <Menu {...props} rowStyle />;
}

// RowsSkeleton：区块加载中。比大块留白的转圈更稳，内容出来时版面不跳。
export function RowsSkeleton({ count = 3 }: { count?: number }) {
  return (
    <div role="status" aria-label={t.common.feedback.loading} className="grid grid-cols-[minmax(0,1fr)]">
      {Array.from({ length: count }, (_, i) => (
        <div key={i} className="flex min-h-13 items-center justify-between gap-4">
          <div className="grid min-w-0 flex-1 grid-cols-[minmax(0,1fr)] gap-2">
            <Skeleton className="h-3 w-40 max-w-full" />
            <Skeleton className="h-3 w-72 max-w-full" />
          </div>
          <Skeleton className="h-3 w-20 shrink-0" />
        </div>
      ))}
    </div>
  );
}

export function Skeleton({ className }: { className?: string }) {
  return <div className={cn("rounded-sm bg-accent motion-safe:animate-pulse", className)} />;
}

// QueryError：查询失败。一律红字加重试，不留空白。
export function QueryError({ error, onRetry, className }: { error: unknown; onRetry?: () => void; className?: string }) {
  const text = typeof error === "string" ? error : errorMessage(error);
  return (
    <Notice
      tone="danger"
      className={className}
      actions={
        onRetry && (
          <Button variant="outline" readOnlyOk onClick={onRetry}>
            {t.common.actions.retry}
          </Button>
        )
      }
    >
      {text}
    </Notice>
  );
}

// SettingRow：设置行，一行状态加一个按钮（有框），放在 List 里。详细说明放 help。
export function SettingRow({ label, description, help, children }: { label: React.ReactNode; description?: React.ReactNode; help?: React.ReactNode; children?: React.ReactNode }) {
  return (
    <Row
      center
      actionsWidth="w-auto"
      title={
        <span className="flex items-center gap-1">
          {label}
          {help && <HelpTip>{help}</HelpTip>}
        </span>
      }
      sub={description}
      actions={children ?? null}
    />
  );
}

export function SettingRows({ children }: { children: React.ReactNode }) {
  return <List>{children}</List>;
}

// ---- 卡片 ----

// Card：概览网格里的一项（项目、节点），整张可以是链接。四边内边距 16px，头、内容、底栏之间 12px。
// 其他地方不用卡片（见 docs/ui.md）；监控数值块用 Tile。
export const cardClass =
  "group flex flex-col rounded-lg border bg-card outline-none transition-colors hover:border-ring focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/15";

export function Card({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn(cardClass, className)} {...props} />;
}

// CardHeader：标题和副标题在左，状态在右。
export function CardHeader({ title, subtitle, aside }: { title: React.ReactNode; subtitle?: React.ReactNode; aside?: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-3 px-4 pt-4">
      <div className="grid min-w-0 gap-1">
        <div className="flex min-w-0 items-center gap-2 text-sm font-medium">{title}</div>
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
  return <div className={cn("grid grid-cols-[repeat(auto-fill,minmax(min(100%,16rem),1fr))] gap-4", className)} {...props} />;
}

// Tile：监控数值块。没有框，靠留白分组。min-w-0：里面的图表不能撑破网格。
export function Tile({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid min-w-0 gap-1", className)} {...props} />;
}

// TileGrid：按所在区域的宽度排列（不是窗口宽度：宽屏的项目页内容区只有窗口的一半多），每块至少 9.5rem。
export function TileGrid({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid grid-cols-[repeat(auto-fit,minmax(min(100%,9.5rem),1fr))] gap-6", className)} {...props} />;
}

// ---- 表格 ----

// 表格：表头 36、13 号灰字，表头下一条线；行高 40，行之间没有线。只给字段多、需要横向比较的内容用，细分页优先 List。
const TableDensity = createContext(false);

export function Table({ className, compact = false, ...props }: React.ComponentProps<"table"> & { compact?: boolean }) {
  return (
    <div data-slot="table" className="overflow-x-auto">
      <TableDensity.Provider value={compact}>
        <table className={cn("w-full text-sm", className)} {...props} />
      </TableDensity.Provider>
    </div>
  );
}

export function Th({ className, ...props }: React.ComponentProps<"th">) {
  return <th className={cn("h-9 border-b px-2 text-left text-[13px] font-normal whitespace-nowrap text-muted-foreground first:pl-0 last:pr-0", className)} {...props} />;
}

export function Td({ className, ...props }: React.ComponentProps<"td">) {
  return <td className={cn("h-9 px-2 py-1 align-middle first:pl-0 last:pr-0", className)} {...props} />;
}

// ---- 状态与提示 ----

const badgeVariants = cva("inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-px text-xs leading-4 whitespace-nowrap", {
  variants: {
    tone: {
      neutral: "border-border text-muted-foreground",
      success: "border-border text-success",
      warning: "border-border text-warning-text",
      danger: "border-transparent bg-destructive text-white",
      info: "border-transparent bg-info/10 text-info",
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
  neutral: "bg-muted-foreground",
};

// Status：状态点加文字，用在表格和标题里。
export function Status({ tone = "neutral", children, pulse }: { tone?: keyof typeof dotColor; children: React.ReactNode; pulse?: boolean }) {
  return (
    <span className="inline-flex items-center gap-2 text-sm whitespace-nowrap">
      <span className="relative flex size-1.5">
        {pulse && <span className={cn("absolute inline-flex size-full rounded-full opacity-60 motion-safe:animate-ping", dotColor[tone])} />}
        <span className={cn("relative inline-flex size-1.5 rounded-full", dotColor[tone])} />
      </span>
      {children}
    </span>
  );
}

const noticeDot = {
  danger: "bg-destructive",
  warning: "bg-warning",
  info: "bg-info",
  neutral: "bg-muted-foreground",
  success: "bg-success",
};

// Notice：提示。没有框：一个状态点、加粗的标题、灰色的说明，操作靠右。主按钮只放真正要处理的事。
export function Notice({
  tone = "neutral",
  title,
  className,
  children,
  actions,
}: {
  tone?: keyof typeof noticeDot;
  title?: React.ReactNode;
  className?: string;
  children?: React.ReactNode;
  actions?: React.ReactNode;
}) {
  return (
    <div role={tone === "danger" ? "alert" : "status"} className={cn("flex min-h-8 items-start gap-2 leading-5 max-md:flex-wrap", className)}>
      <span className={cn("mt-[7px] size-1.5 shrink-0 rounded-full", noticeDot[tone])} />
      <div className="min-w-0 flex-1 text-pretty">
        {title && <span className={cn("font-medium", tone === "danger" && "text-destructive")}>{title}</span>}
        {title && children && " "}
        {children && <span className={cn(tone === "danger" && !title ? "font-medium text-destructive" : "text-muted-foreground")}>{children}</span>}
      </div>
      {actions && <div className="-mt-1.5 flex shrink-0 items-center gap-2 max-md:mt-0 max-md:w-full max-md:[&>*]:flex-1">{actions}</div>}
    </div>
  );
}

// Alert：错误提示（Notice 的简写）。在表单里出现时把焦点移过来，表单长的时候也看得到；
// keepFocus：用户接着要改刚才的输入（例如登录失败重输密码）时不移动焦点。
export function Alert({ className, children, keepFocus }: { className?: string; children?: React.ReactNode; keepFocus?: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (keepFocus || !ref.current?.closest("form")) return;
    ref.current.focus({ preventScroll: true });
    ref.current.scrollIntoView({ block: "nearest" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <div ref={ref} tabIndex={-1} className={cn("outline-none", className)}>
      <Notice tone="danger">{children}</Notice>
    </div>
  );
}

export function Spinner({ className }: { className?: string }) {
  return <span className={cn("inline-block size-4 animate-spin rounded-full border-2 border-current border-r-transparent", className)} role="img" aria-label={t.common.feedback.loading} />;
}

// Loading：整块区域加载中。
export function Loading() {
  return <RowsSkeleton />;
}

// Empty：空状态。
export function Empty({ title, children, action, icon }: { title: string; children?: React.ReactNode; action?: React.ReactNode; icon?: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-16 text-center">
      {icon && <div className="mb-1 text-muted-foreground [&_svg]:size-5">{icon}</div>}
      <p className="text-base font-semibold">{title}</p>
      {children && <div className="max-w-md text-muted-foreground">{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

// EmptyRow：区块里没有内容时的一行说明。
export function EmptyRow({ children }: { children: React.ReactNode }) {
  return <p className="py-4 text-center text-muted-foreground">{children}</p>;
}

// ---- 文本 ----

// KeyValue：两列的属性列表。
export function KeyValue({ items, className }: { items: [React.ReactNode, React.ReactNode][]; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[6rem_1fr] gap-x-4 max-md:grid-cols-[5rem_1fr] [&>*>*]:flex [&>*>*]:min-h-8 [&>*>*]:items-center", className)}>
      {items.map(([k, v], i) => (
        <div key={i} className="contents">
          <dt className="text-muted-foreground">{k}</dt>
          <dd className="min-w-0 gap-2 wrap-anywhere">{v}</dd>
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
      className={cn("size-8 text-muted-foreground", className)}
      aria-label={t.common.actions.copy}
      title={done ? t.common.feedback.copied : t.common.actions.copy}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } catch {
          // 非 HTTPS 等情况下浏览器不让写剪贴板，只能手动选中复制。
          toast.error(t.common.feedback.copyFailed);
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
    <div className={cn("relative rounded-md bg-accent", className)}>
      <code className={cn("block px-3 py-2.5 font-mono text-xs leading-relaxed break-all", copy && "pr-10")}>{children}</code>
      {copy && <CopyButton text={children} className="absolute top-1 right-1" />}
    </div>
  );
}

// LogBox：日志和命令输出的深色区域。
export function LogBox({ className, ...props }: React.ComponentProps<"pre">) {
  return <pre className={cn("overflow-auto rounded-lg bg-log px-4 py-3 font-mono text-xs leading-5 text-log-foreground", className)} {...props} />;
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
  // 放不下时横向滑动，选中项滚到可见（手机上容器有 8 个标签）。
  const listRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const list = listRef.current;
    const active = list?.querySelector<HTMLElement>('[data-state="active"]');
    if (!list || !active) return;
    // 连带前后相邻的一项，看得出还能往哪边滑。
    const pad = 16;
    const box = list.getBoundingClientRect();
    const at = (el: Element) => el.getBoundingClientRect();
    const left = at(active.previousElementSibling ?? active).left - box.left + list.scrollLeft;
    const right = at(active.nextElementSibling ?? active).right - box.left + list.scrollLeft;
    if (left - pad < list.scrollLeft) list.scrollTo({ left: left - pad });
    else if (right + pad > list.scrollLeft + list.clientWidth) list.scrollTo({ left: right + pad - list.clientWidth });
  }, [value]);
  return (
    <TabsRoot value={value} onValueChange={(v) => onChange(v as T)} activationMode="manual">
      <TabsList ref={listRef} className={cn("flex gap-6 overflow-x-auto border-b [scrollbar-width:none] max-md:-mx-4 max-md:px-4", className)}>
        {items.map((it) => (
          <TabsTrigger
            key={it.value}
            value={it.value}
            className="-mb-px inline-flex h-10 shrink-0 cursor-pointer items-center gap-1 border-b-2 border-transparent text-sm whitespace-nowrap text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40 focus-visible:ring-inset data-[state=active]:border-foreground data-[state=active]:text-foreground"
          >
            {it.label}
            {it.count !== undefined && it.count > 0 && <span className="text-xs text-muted-foreground">{it.count}</span>}
          </TabsTrigger>
        ))}
      </TabsList>
    </TabsRoot>
  );
}

// SegmentedControl：两三个选项之间切换（例如"容器 / 容器组"）。
export function Segmented<T extends string>({ value, onChange, items }: { value: T; onChange: (v: T) => void; items: { value: T; label: React.ReactNode }[] }) {
  return (
    <TabsRoot value={value} onValueChange={(v) => onChange(v as T)} activationMode="manual">
      <TabsList className="inline-flex h-8 rounded-lg bg-muted p-[3px] max-md:flex max-md:h-10 max-md:w-full">
        {items.map((it) => (
          <TabsTrigger
            key={it.value}
            value={it.value}
            className="h-full cursor-pointer rounded-md border border-transparent px-2 text-sm text-foreground/60 max-md:flex-1 max-md:whitespace-nowrap outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/40 data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-[0_0_0_1px_var(--border)]"
          >
            {it.label}
          </TabsTrigger>
        ))}
      </TabsList>
    </TabsRoot>
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

// Dialog：对话框（shadcn）。点遮罩或按 Esc 关闭，焦点限制在框内。
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
  return (
    <DialogRoot open={open} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className={className} {...(description ? {} : { "aria-describedby": undefined })}>
        <div className="relative px-6 pt-6 pb-4 max-md:px-4 max-md:pt-4">
          <div className="grid min-w-0 gap-1 [&>h2]:pr-6">
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </div>
          <DialogClose asChild>
            <Button size="icon" variant="ghost" className="absolute top-4 right-4 size-8 max-md:top-2 max-md:right-2 max-md:size-10" aria-label={t.common.actions.close} readOnlyOk>
              <X />
            </Button>
          </DialogClose>
        </div>
        <div className="grid gap-4 px-6 pb-6 max-md:px-4 max-md:pb-4">{children}</div>
      </DialogContent>
    </DialogRoot>
  );
}

type MenuItem = { label: React.ReactNode; onSelect: () => void; danger?: boolean; disabled?: boolean; hint?: string };

// ActionSheet：手机的底部操作面板：灰字标题、一列项高 44 的操作、取消。
function ActionSheet({ open, onClose, items, label, title }: { open: boolean; onClose: () => void; items: MenuItem[]; label: string; title?: React.ReactNode }) {
  return (
    <DialogRoot open={open} onOpenChange={(v) => !v && onClose()}>
      <DialogContent
        aria-describedby={undefined}
        className="grid gap-2 p-4"
        // 焦点放在面板上，不高亮第一项。
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          (e.currentTarget as HTMLElement).focus();
        }}
      >
        <DialogTitle className={title ? "truncate py-1 text-center text-sm leading-5 font-medium text-muted-foreground" : "sr-only"}>{title ?? label}</DialogTitle>
        <div className="grid">
          {items.map((it, i) => (
            <button
              key={i}
              type="button"
              disabled={it.disabled}
              className={cn("grid min-h-11 cursor-pointer content-center justify-items-center rounded-md px-2 text-base leading-6 outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/40 disabled:cursor-not-allowed disabled:opacity-50", it.danger && "text-destructive")}
              onClick={() => {
                onClose();
                // 等面板关掉再执行：选中后常常接着打开确认框。
                setTimeout(it.onSelect);
              }}
            >
              {it.label}
              {it.hint && <span className="pb-1 text-xs leading-4 text-muted-foreground">{it.hint}</span>}
            </button>
          ))}
        </div>
        <DialogClose asChild>
          <Button variant="outline" readOnlyOk>
            {t.common.actions.cancel}
          </Button>
        </DialogClose>
      </DialogContent>
    </DialogRoot>
  );
}

// Menu：「更多」按钮和下拉菜单（shadcn）。不用模态模式：选中后常常接着打开确认框，模态菜单会和它争焦点。
// 手机上是底部操作面板。title：面板的标题（灰字），例如文件名。ghost：无框按钮（手机标题栏的 ⋯）。
export function Menu({ items, label = t.common.actions.more, rowStyle, ghost, title }: { items: MenuItem[]; label?: string; rowStyle?: boolean; ghost?: boolean; title?: React.ReactNode }) {
  const mobile = useIsMobile();
  const [open, setOpen] = useState(false);
  const trigger = (
    <Button variant={rowStyle || ghost ? "ghost" : "outline"} size={rowStyle ? "rowIcon" : "icon"} aria-label={label} onClick={mobile ? () => setOpen(true) : undefined}>
      <Ellipsis />
    </Button>
  );
  if (mobile) {
    return (
      <>
        {trigger}
        <ActionSheet open={open} onClose={() => setOpen(false)} items={items} label={label} title={title} />
      </>
    );
  }
  return (
    <DropdownMenu modal={false}>
      <DropdownMenuTrigger asChild>{trigger}</DropdownMenuTrigger>
      <DropdownMenuContent>
        {items.map((it, i) => (
          <Fragment key={i}>
            {/* 危险操作和其他操作之间一条分隔线。 */}
            {it.danger && i > 0 && !items[i - 1].danger && <DropdownMenuSeparator />}
            <DropdownMenuItem disabled={it.disabled} variant={it.danger ? "destructive" : undefined} onSelect={() => setTimeout(it.onSelect)}>
              {it.label}
              {it.hint && <span className="mt-0.5 block text-xs text-muted-foreground">{it.hint}</span>}
            </DropdownMenuItem>
          </Fragment>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
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

