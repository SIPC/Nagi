// 基础组件：灰阶、细边框、紧凑的控制台风格（参照 shadcn/ui）。
import { cva, type VariantProps } from "class-variance-authority";
import { Children, cloneElement, createContext, isValidElement, useContext, useEffect, useRef, useState, useSyncExternalStore, useCallback, useLayoutEffect } from "react";
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

export const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer items-center justify-center gap-1 rounded-md border border-transparent text-sm whitespace-nowrap transition-colors outline-none not-disabled:active:opacity-80 focus-ring disabled:cursor-not-allowed disabled:opacity-50 [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default: "bg-primary font-medium text-primary-foreground not-disabled:hover:bg-primary/88",
        outline: "border-input bg-transparent not-disabled:hover:bg-accent",
        ghost: "not-disabled:hover:bg-accent",
        // 底色用 destructive-solid：深色下 destructive 是给文字用的浅红，白字放在上面对比度不够。
        destructive: "bg-destructive-solid text-white not-disabled:hover:bg-destructive-solid/90",
        "destructive-outline": "border-input bg-transparent text-destructive not-disabled:hover:bg-accent",
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

// ReadOnlyContext：备用面板上或角色只能查看时为 true，按钮默认置灰（只读的按钮加 readOnlyOk）。见 docs/standby.md。
export const ReadOnlyContext = createContext(false);
// ReadOnlyReasonContext：只读的原因，决定置灰按钮的提示。
export const ReadOnlyReasonContext = createContext<"standby" | "role">("role");

function useReadOnlyHint() {
  return useContext(ReadOnlyReasonContext) === "standby" ? t.common.readOnly.standby : t.common.readOnly.role;
}

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
  const hint = useReadOnlyHint();
  return (
    <button
      type={type}
      className={cn(buttonVariants({ variant, size }), className)}
      disabled={disabled || readOnly}
      title={readOnly ? hint : title}
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
  "w-full rounded-md border border-input bg-transparent px-3 text-sm outline-none transition-[border-color,box-shadow] placeholder:text-muted-foreground focus-ring aria-[invalid=true]:border-destructive disabled:cursor-not-allowed disabled:opacity-60 max-md:text-base";

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
  "aria-invalid": ariaInvalid,
  "aria-describedby": ariaDescribedby,
}: {
  id?: string;
  "aria-label"?: string;
  // Field 出错时给子元素加这两项，传到触发器上，才有红框和读屏说明。
  "aria-invalid"?: boolean;
  "aria-describedby"?: string;
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
      <SelectTrigger id={id} className={className} aria-label={ariaLabel} aria-invalid={ariaInvalid} aria-describedby={ariaDescribedby}>
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
          aria-label={t.common.help}
          className={cn("inline-flex cursor-help rounded-full align-middle text-muted-foreground/70 outline-none hover:text-foreground focus-visible:text-foreground focus-ring", className)}
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
  children: React.ReactNode;
}) {
  const errorId = error && htmlFor ? `${htmlFor}-error` : undefined;
  return (
    <div className={cn("grid min-w-0 content-start gap-2", className)}>
      <div className="flex items-center gap-1">
        <Label htmlFor={htmlFor}>{label}</Label>
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
// 在对话框和确认框里和上面的内容多隔 8，靠留白分组，不加底栏和分隔线。
const inDialog = "in-data-[slot=dialog-body]:mt-2";
export function DialogActions({ className, ...props }: React.ComponentProps<"div">) {
  return <div data-slot="dialog-actions" className={cn("flex items-center justify-end gap-2 max-md:[&>*]:flex-1", inDialog, className)} {...props} />;
}

// Section：页面上的一组内容，没有外框。区块之间一条细线，线上下各 24，所以父级不要再加间距（Sections 排列）。
// 说明放进标题旁的问号（help），不占一行；description 只给确实需要一直看到的话。
// 高度和图表过渡共用的时长和曲线：下面的内容和图里的线一起走，才连得上。
export const layoutMotionMs = 360;
export const layoutMotionCurve = [0.25, 0.46, 0.3, 1] as const;

// RollCell：数字的一位（或单位这样的一段文字）。内容变了，旧的滚出去、新的滚进来，宽度也过渡，后面的内容平滑让位。
// 文字变成空就是这一位消失了（12 变成 8.5 这样位数变少）：滚出去、宽度收到 0。
function RollCell({ text, dir, delay, appear }: { text: string; dir: "up" | "down"; delay: number; appear: boolean }) {
  const [state, setState] = useState({ text, old: null as string | null, version: appear ? 1 : 0, dir });
  if (state.text !== text) setState({ text, old: state.text || null, version: state.version + 1, dir });
  const cell = useRef<HTMLSpanElement>(null);
  const inner = useRef<HTMLSpanElement>(null);
  const shown = useRef(appear ? 0 : -1);
  const running = useRef<Animation | null>(null);
  useLayoutEffect(() => {
    if (!cell.current || !inner.current) return;
    const width = inner.current.getBoundingClientRect().width;
    const from = running.current ? cell.current.getBoundingClientRect().width : shown.current;
    running.current?.cancel();
    running.current = null;
    shown.current = width;
    if (from < 0 || Math.abs(from - width) < 0.5 || matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    const animation = cell.current.animate([{ width: `${from}px` }, { width: `${width}px` }], { duration: 260, delay, fill: "backwards", easing: `cubic-bezier(${layoutMotionCurve.join(",")})` });
    running.current = animation;
    animation.onfinish = () => {
      if (running.current === animation) running.current = null;
    };
  }, [state.version, delay]);
  if (!state.text && state.old === null) return null;
  const style = { animationDelay: `${delay}ms` };
  return (
    <span ref={cell} className={cn("relative inline-flex justify-end whitespace-pre", state.old !== null && "[clip-path:inset(0_-1em)]")}>
      {state.old !== null && (
        <span
          key={`old-${state.version}`}
          style={style}
          className={cn("absolute inset-y-0 right-0", state.dir === "up" ? "animate-roll-out-up" : "animate-roll-out-down")}
          onAnimationEnd={() => setState((s) => ({ ...s, old: null }))}
        >
          {state.old}
        </span>
      )}
      <span ref={inner} key={state.version} style={style} className={cn("inline-block shrink-0", state.text && state.version > 0 && (state.dir === "up" ? "animate-roll-in-up" : "animate-roll-in-down"))}>
        {/* 空的时候放零宽字符：这一位收起的过程中还要保持基线。 */}
        {state.text || "\u200b"}
      </span>
    </span>
  );
}

// Roll：会变的数据。只有变了的那几位上下滚动：变大时从下面滚上来，变小时从上面滚下来。第一次显示不滚。
// n 是比较大小用的数值，单位不同时要传（1.2 MB/s 比 640 KB/s 大）；不传时取文字里第一个数字。
export function Roll({ value, n, className }: { value: string; n?: number; className?: string }) {
  const parts = value.match(/\d|[^\d]+/g) ?? [];
  const size = n ?? Number(value.match(/\d[\d,]*(\.\d+)?/)?.[0].replace(/,/g, "") ?? Number.NaN);
  const last = useRef({ value, size, dir: "up" as "up" | "down" });
  const mounted = useRef(false);
  // 出现过的位数都留着：位数变少时多出来的那一位还要播完退场。
  const cells = useRef(parts.length);
  cells.current = Math.max(cells.current, parts.length);
  if (last.current.value !== value) {
    last.current = { value, size, dir: size < last.current.size ? "down" : "up" };
  }
  useEffect(() => {
    mounted.current = true;
  }, []);
  const dir = last.current.dir;
  return (
    // relative：读屏文字是绝对定位的，不能跳出横向滚动的表格，否则会撑宽整页。
    <span className={cn("relative", className)}>
      <span className="sr-only">{value}</span>
      <span aria-hidden>
        {Array.from({ length: cells.current }, (_, i) => {
          // 从右往左对位：9.7 变成 10 时个位还对着个位。
          const fromRight = cells.current - 1 - i;
          return <RollCell key={fromRight} text={parts[parts.length - 1 - fromRight] ?? ""} dir={dir} delay={fromRight * 16} appear={mounted.current} />;
        })}
      </span>
    </span>
  );
}

// AutoHeight：内容高度变了（数据加载完、列表增减）不直接跳，外层过渡到新高度，下面的内容跟着平滑移动。
// 窗口变宽变窄引起的高度变化、拖动输入框时不过渡。可以嵌套：里面的组件自己过渡时外层自然跟着，组件新出现或消失时才由外层过渡。
export function AutoHeight({ className, children, slot }: { className?: string; children: React.ReactNode; slot?: string }) {
  const outer = useRef<HTMLDivElement>(null);
  const inner = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const box = outer.current;
    const content = inner.current;
    if (!box || !content) return;
    // 起点在第一次回报时才记：刚挂载时里面可能还没量好尺寸（例如流量图第一轮渲染没有高度）。
    let height = -1;
    let width = content.offsetWidth;
    const nested = () => [...content.querySelectorAll("[data-auto-height]")];
    let children = nested();
    let running: Animation | undefined;
    const stop = () => {
      running?.cancel();
      running = undefined;
      box.classList.remove("auto-height-running");
    };
    const observer = new ResizeObserver(() => {
      const nextHeight = content.offsetHeight;
      const resized = content.offsetWidth !== width;
      width = content.offsetWidth;
      if (height < 0) {
        height = nextHeight;
        children = nested();
        return;
      }
      if (nextHeight === height) return;
      // 还是原来那批子组件，变化是它们自己在过渡；换了一批（切换视图、骨架换成内容）才由这层过渡。
      const now = nested();
      const childDriven = now.length > 0 && now.length === children.length && now.every((el, i) => el === children[i]);
      children = now;
      // 外层没有固定高度，已经是新高度了：起点要取上一次的高度，正在过渡时取当前的高度。
      const from = running ? box.offsetHeight : height;
      height = nextHeight;
      stop();
      if (childDriven || resized || Math.abs(nextHeight - from) < 4 || !content.offsetParent || content.querySelector(":active") || matchMedia("(prefers-reduced-motion: reduce)").matches) return;
      // 变矮时内容比外框短，不用裁切。变高时下边缘渐隐，内容像被揭开，不是被一条边切开：
      // 渐隐只落在新露出的部分（--auto-height-from 以下），原有内容不变暗。收到 0 再结束，否则最后一段会突然变亮。
      if (nextHeight > from) {
        box.style.setProperty("--auto-height-from", `${from}px`);
        box.classList.add("auto-height-running");
      }
      const fade = `${Math.round(Math.min(200, Math.abs(nextHeight - from)))}px`;
      const animation = box.animate(
        [
          { height: `${from}px`, "--auto-height-fade": fade },
          { "--auto-height-fade": fade, offset: 0.75 },
          { height: `${nextHeight}px`, "--auto-height-fade": "0px" },
        ],
        { id: "auto-height", duration: layoutMotionMs, easing: `cubic-bezier(${layoutMotionCurve.join(",")})` },
      );
      running = animation;
      animation.onfinish = () => {
        if (running !== animation) return;
        running = undefined;
        box.classList.remove("auto-height-running");
      };
    });
    observer.observe(content);
    return () => observer.disconnect();
  }, []);
  return (
    <div ref={outer} className="min-w-0">
      <div ref={inner} data-auto-height="" data-slot={slot} className={className}>
        {children}
      </div>
    </div>
  );
}

// Collapse：切换选项后多出、少掉的一块（例如选「指定」后出现的勾选列表）。高度和透明度一起过渡，
// 收起时内容留到过渡结束才移除，不会先消失再收拢。带 data-auto-height：外层的 AutoHeight 当它是自己在过渡的子组件，不重复过渡。
export function Collapse({ open, children }: { open: boolean; children: React.ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const [closing, setClosing] = useState(false);
  const kept = useRef(children);
  if (open) kept.current = children;
  // 和上一次比较，不用「是否第一次」：开发模式下挂载时副作用会跑两遍。
  const last = useRef(open);
  useLayoutEffect(() => {
    const el = ref.current;
    if (last.current === open || !el) return;
    last.current = open;
    const running = el.getAnimations();
    // 正在过渡时从当前高度接着走。
    const from = running.length > 0 ? el.offsetHeight : open ? 0 : el.offsetHeight;
    running.forEach((a) => a.cancel());
    if (!open) setClosing(true);
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) {
      setClosing(false);
      return;
    }
    const to = open ? el.scrollHeight : 0;
    const animation = el.animate(
      [
        { height: `${from}px`, opacity: open ? 0 : 1, overflow: "clip" },
        { height: `${to}px`, opacity: open ? 1 : 0, overflow: "clip" },
      ],
      { duration: layoutMotionMs, easing: `cubic-bezier(${layoutMotionCurve.join(",")})`, fill: open ? "none" : "forwards" },
    );
    animation.onfinish = () => {
      if (!open) setClosing(false);
    };
  }, [open]);
  return (
    <div ref={ref} data-auto-height="" className="min-w-0">
      {/* last 还没更新时是刚收起的这一帧：内容先留着，副作用里才量得到收起前的高度。 */}
      {(open || closing || last.current) && kept.current}
    </div>
  );
}

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
          {actions && <div className="flex flex-wrap items-center gap-2 max-md:contents [&>span]:text-[13px] [&>span]:text-muted-foreground max-md:[&>span]:order-1 max-md:[&>span]:w-full max-md:[&>span]:leading-5">{actions}</div>}
        </header>
      )}
      {children != null && children !== false && <AutoHeight className="grid gap-4 max-md:gap-2">{children}</AutoHeight>}
    </section>
  );
}

// ---- 列表行 ----

// List / Row：细分页的基本单位（见 docs/ui.md「列表行」）。主信息在左，灰色信息是靠右的定宽列，最后是操作。
// 所有内容对齐第一行；悬停整行变底色，所以 List 左右各外扩 8，让文字和页面边缘对齐。
export function List({ className, ...props }: React.ComponentProps<"div">) {
  // 列宽不超过容器：行里的长文字截断，不把整个区块撑宽。
  return <div className={cn("-mx-2 grid grid-cols-[minmax(0,1fr)] animate-fade-in", className)} {...props} />;
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

// RowsSkeleton：区块加载中。比大块留白的转圈更稳；矮一点，内容出来时由区块过渡到实际高度。
export function RowsSkeleton({ count = 2 }: { count?: number }) {
  return (
    <div role="status" aria-label={t.common.feedback.loading} className="grid grid-cols-[minmax(0,1fr)]">
      {Array.from({ length: count }, (_, i) => (
        <div key={i} className="flex min-h-9 items-center justify-between gap-4">
          <Skeleton className="h-3 w-56 max-w-full min-w-0" />
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

// cardClass：项目页网格里的卡片，整张是链接。其他地方不用卡片（见 docs/ui.md）。
export const cardClass =
  "group flex flex-col rounded-lg border bg-background outline-none transition-colors hover:border-ring focus-ring";

// CardGrid：卡片的网格，每张至少 240px，卡片之间 16px。
export function CardGrid({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid animate-fade-in grid-cols-[repeat(auto-fill,minmax(min(100%,240px),1fr))] gap-4", className)} {...props} />;
}


// ---- 表格 ----

// 表格：表头 36、13 号灰字，表头下一条线；行高 36，行之间没有线，悬停整行浅底、首尾圆角 6。
// 左右各外扩 8 只是为了悬停底色，文字和区块标题对齐；表头线不超出内容区。只给字段多、需要横向比较的内容用，细分页优先 List。
const TableDensity = createContext(false);

export function Table({ className, compact = false, ...props }: React.ComponentProps<"table"> & { compact?: boolean }) {
  return (
    <div data-slot="table" className="-mx-2 overflow-x-auto">
      <TableDensity.Provider value={compact}>
        <table
          className={cn(
            "w-full text-sm [&_tbody_tr:hover>td]:bg-accent [&_tbody_tr>td:first-child]:rounded-l-md [&_tbody_tr>td:last-child]:rounded-r-md",
            className,
          )}
          {...props}
        />
      </TableDensity.Provider>
    </div>
  );
}

// 表头线画在背景上：首尾两格各让出外扩的 8，线和内容区一样宽。
const headLine = "bg-[linear-gradient(var(--border),var(--border))] bg-bottom bg-no-repeat bg-size-[100%_1px] first:bg-size-[calc(100%-8px)_1px] first:bg-right-bottom last:bg-size-[calc(100%-8px)_1px] last:bg-left-bottom";

export function Th({ className, ...props }: React.ComponentProps<"th">) {
  return <th className={cn("h-9 px-2 text-left text-[13px] font-normal whitespace-nowrap text-muted-foreground", headLine, className)} {...props} />;
}

export function Td({ className, ...props }: React.ComponentProps<"td">) {
  return <td className={cn("h-9 px-2 py-1 align-middle transition-colors", className)} {...props} />;
}

// ---- 状态与提示 ----

export const badgeVariants = cva("inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-px text-xs leading-4 whitespace-nowrap", {
  variants: {
    tone: {
      neutral: "border-border text-muted-foreground",
      success: "border-border text-success",
      warning: "border-border text-warning-text",
      danger: "border-transparent bg-destructive-solid text-white",
      info: "border-border text-info",
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
        <span className={cn("relative inline-flex size-1.5 rounded-full transition-colors duration-300", dotColor[tone])} />
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
    // 带按钮时一行高 32：文字上下留 6，按钮用负外边距抵掉，文字和按钮上下居中。
    <div role={tone === "danger" ? "alert" : "status"} className={cn("flex min-h-8 animate-fade-in items-start gap-2 leading-5 max-md:flex-wrap", actions && "md:py-1.5", className)}>
      <span className={cn("mt-[7px] size-1.5 shrink-0 rounded-full", noticeDot[tone])} />
      <div className="min-w-0 flex-1 text-pretty">
        {title && <span className={cn("font-medium", tone === "danger" && "text-destructive")}>{title}</span>}
        {title && children && " "}
        {children && <span className={cn(tone === "danger" && !title ? "font-medium text-destructive" : "text-muted-foreground")}>{children}</span>}
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2 max-md:w-full md:-my-1.5 max-md:[&>*]:flex-1">{actions}</div>}
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
    <div className="flex flex-col items-center gap-2 px-6 py-12 text-center">
      {icon && <div className="mb-1 text-muted-foreground [&_svg]:size-5">{icon}</div>}
      <p className="text-base font-semibold">{title}</p>
      {children && <div className="max-w-md text-muted-foreground">{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

// EmptyRow：区块里没有内容时的一行说明。action：下一步的按钮，例如「暂无访问令牌」下的「生成令牌」。
export function EmptyRow({ children, action }: { children: React.ReactNode; action?: React.ReactNode }) {
  if (!action) return <p className="py-4 text-center text-muted-foreground">{children}</p>;
  return (
    <div className="flex flex-col items-center gap-3 py-4 text-center">
      <p className="text-muted-foreground">{children}</p>
      {action}
    </div>
  );
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
      {done ? <CheckIcon className="size-3.5 animate-symbol-in" /> : <Copy className="size-3.5 animate-symbol-in" />}
    </Button>
  );
}

// CodeBlock：一段等宽文字（命令、密钥、地址），右上角可复制。
export function CodeBlock({ children, copy, nowrap, className }: { children: string; copy?: boolean; nowrap?: boolean; className?: string }) {
  // nowrap：一行命令或接入码，放不下时截断，完整内容在悬停提示里，复制按钮在右边。默认按需折行。
  if (nowrap)
    return (
      <div className={cn("flex min-h-10 min-w-0 items-center justify-between gap-2 rounded-lg bg-accent pr-1 pl-3", className)}>
        <code className="min-w-0 truncate font-mono text-[13px] leading-5" title={children}>
          {children}
        </code>
        {copy && <CopyButton text={children} className="shrink-0" />}
      </div>
    );
  return (
    <div className={cn("relative rounded-lg bg-accent", className)}>
      <code className={cn("block px-3 py-2.5 font-mono text-[13px] leading-relaxed break-all", copy && "pr-10")}>{children}</code>
      {copy && <CopyButton text={children} className="absolute top-1 right-1" />}
    </div>
  );
}

// LogBox：日志和命令输出的深色区域。
export function LogBox({ className, ...props }: React.ComponentProps<"pre">) {
  return <pre className={cn("overflow-auto rounded-lg bg-log px-4 py-3 font-mono text-[13px] leading-5 text-log-foreground", className)} {...props} />;
}

// ---- 标签页 ----

// 选中滑块的动画：朝移动方向的那条边先到，另一条边慢一点跟上，中途略微拉长。两条边是两个属性，才能分开走。
const sliderMotion = {
  right: "[transition:right_200ms_cubic-bezier(0.2,0.9,0.25,1),left_340ms_cubic-bezier(0.2,0.9,0.25,1)] motion-reduce:[transition:none]",
  left: "[transition:left_200ms_cubic-bezier(0.2,0.9,0.25,1),right_340ms_cubic-bezier(0.2,0.9,0.25,1)] motion-reduce:[transition:none]",
  down: "[transition:bottom_200ms_cubic-bezier(0.2,0.9,0.25,1),top_340ms_cubic-bezier(0.2,0.9,0.25,1)] motion-reduce:[transition:none]",
  up: "[transition:top_200ms_cubic-bezier(0.2,0.9,0.25,1),bottom_340ms_cubic-bezier(0.2,0.9,0.25,1)] motion-reduce:[transition:none]",
};

// useTabIndicator：量出选中项的位置，选中指示器从上一项滑过去，说明两项是同一组。第一次显示、窗口变化时不动画。
function useTabIndicator(value: string, listRef: React.RefObject<HTMLDivElement | null>) {
  const [box, setBox] = useState<{ left: number; right: number; value: string; motion?: string } | null>(null);
  const measure = useCallback(() => {
    const list = listRef.current;
    const el = list?.querySelector<HTMLElement>('[data-state="active"]');
    setBox((prev) => {
      if (!list || !el) return null;
      const left = el.offsetLeft;
      const right = list.clientWidth - left - el.offsetWidth;
      if (prev && prev.left === left && prev.right === right) return prev.value === value ? prev : { ...prev, value };
      return { left, right, value, motion: prev && prev.value !== value ? (left > prev.left ? sliderMotion.right : sliderMotion.left) : undefined };
    });
  }, [value, listRef]);
  useLayoutEffect(measure, [measure]);
  useEffect(() => {
    const list = listRef.current;
    if (!list) return;
    const observer = new ResizeObserver(measure);
    observer.observe(list);
    for (const child of list.children) observer.observe(child);
    return () => observer.disconnect();
  }, [measure, listRef]);
  return box;
}

// useActiveBox 量出列表里选中项（data-active）的位置，给滑动的背景用。第一次只放到位、不做动画。
export function useActiveBox() {
  const ref = useRef<HTMLElement>(null);
  const [box, setBox] = useState<{ top: number; bottom: number; motion?: string } | null>(null);
  useLayoutEffect(() => {
    const nav = ref.current;
    const el = nav?.querySelector<HTMLElement>("[data-active]");
    setBox((prev) => {
      if (!nav || !el) return null;
      const top = el.offsetTop;
      const bottom = nav.offsetHeight - top - el.offsetHeight;
      if (prev && prev.top === top && prev.bottom === bottom) return prev;
      return { top, bottom, motion: prev ? (top > prev.top ? sliderMotion.down : sliderMotion.up) : undefined };
    });
  });
  return { ref, box };
}

// ActiveBox：竖排导航里选中项的背景，换选中项时滑过去。放在导航最前面，导航项要 relative。
export function ActiveBox({ box, className }: { box: { top: number; bottom: number; motion?: string } | null; className?: string }) {
  if (!box) return null;
  return <div aria-hidden className={cn("pointer-events-none absolute inset-x-0 rounded-md bg-accent", box.motion, className)} style={{ top: box.top, bottom: box.bottom }} />;
}

// 内容跟着导航的方向滑入，三项依次是：选了前面的、位置不变（第一次显示、进下一级）、选了后面的。
// leave 是旧内容退场，和新内容同方向移动，连起来才是一次切换。
const slideClasses = {
  // 横排的标签和切换控件。
  tabs: {
    enter: ["animate-slide-from-left", "animate-fade-in", "animate-slide-from-right"],
    leave: ["animate-leave-right", "animate-fade-out", "animate-leave-left"],
  },
  // 主导航：手机上导航是全屏的，点了就关，只淡入。
  page: {
    enter: ["animate-fade-in md:animate-slide-from-above", "animate-fade-in", "animate-fade-in md:animate-slide-from-below"],
    leave: ["animate-fade-out md:animate-leave-down", "animate-fade-out", "animate-fade-out md:animate-leave-up"],
  },
  // 设置页：手机上二级菜单是横排的。
  settings: {
    enter: ["animate-slide-from-left md:animate-slide-from-above", "animate-fade-in", "animate-slide-from-right md:animate-slide-from-below"],
    leave: ["animate-leave-right md:animate-leave-down", "animate-fade-out", "animate-leave-left md:animate-leave-up"],
  },
  // 项目页：宽屏是项目栏（竖排），中等宽度是标签（横排），手机上是全屏导航。
  project: {
    enter: ["animate-fade-in md:animate-slide-from-left xl:animate-slide-from-above", "animate-fade-in", "animate-fade-in md:animate-slide-from-right xl:animate-slide-from-below"],
    leave: ["animate-fade-out md:animate-leave-right xl:animate-leave-down", "animate-fade-out", "animate-fade-out md:animate-leave-left xl:animate-leave-up"],
  },
};

// 元素太多时不做退场：拷贝要花时间，反而卡。
const ghostMaxElements = 1500;

// showGhost：把旧内容的拷贝盖在原位淡出。React 换内容时旧节点已经没了，所以换之前先拷贝。
function captureGhost(old: HTMLElement) {
  const rect = old.getBoundingClientRect();
  if (rect.height === 0 || old.getElementsByTagName("*").length > ghostMaxElements) return null;
  return { node: old.cloneNode(true) as HTMLElement, rect };
}

function showGhost({ node, rect }: { node: HTMLElement; rect: DOMRect }, className: string) {
  for (const el of node.querySelectorAll("[id]")) el.removeAttribute("id");
  node.className = className;
  node.inert = true;
  node.setAttribute("aria-hidden", "true");
  node.setAttribute("data-slide-ghost", "");
  // 层级低于吸顶栏和对话框。
  Object.assign(node.style, { position: "fixed", left: `${rect.left}px`, top: `${rect.top}px`, width: `${rect.width}px`, height: `${rect.height}px`, margin: "0", zIndex: "10", pointerEvents: "none" });
  document.body.append(node);
  const remove = () => node.remove();
  node.addEventListener("animationend", remove, { once: true });
  const timer = setTimeout(remove, 300);
  return () => {
    clearTimeout(timer);
    remove();
  };
}

// Slide：id 变了就换内容，按 index 比较新旧位置决定从哪边滑入。方向记在 ref 里：定时刷新引起的重新渲染不能改类名，改了动画会重播。
export function Slide({ id, index, variant = "tabs", className, inner, children }: { id: string; index: number; variant?: keyof typeof slideClasses; className?: string; inner?: string; children: React.ReactNode }) {
  const node = useRef<HTMLDivElement>(null);
  const last = useRef({ id, index, dir: 1 });
  const ghost = useRef<ReturnType<typeof captureGhost>>(null);
  if (last.current.id !== id) {
    ghost.current = node.current && captureGhost(node.current);
    last.current = { id, index, dir: index === last.current.index ? 1 : index > last.current.index ? 2 : 0 };
  }
  const classes = slideClasses[variant];
  const dir = last.current.dir;
  useLayoutEffect(() => {
    if (!ghost.current) return;
    const stop = showGhost(ghost.current, cn(classes.leave[dir], className));
    ghost.current = null;
    return stop;
    // 只在换内容时运行。
  }, [id]);
  // 主导航、设置页、项目页换的是整块内容，这一层不过渡高度（不过渡整页），只过渡里面的每个组件。
  if (variant !== "tabs") {
    return (
      <div ref={node} key={id} className={cn(classes.enter[dir], className)}>
        {variant === "page" ? children : <AutoHeight className={inner}>{children}</AutoHeight>}
      </div>
    );
  }
  // 页面里的标签和切换控件：高度过渡放在换内容的这一层外面。换内容时它不重建，从旧高度过渡到新高度，
  // 下面的内容跟着平滑移动，不会先跳过去再被揭开。
  return (
    <AutoHeight>
      <div ref={node} key={id} className={cn(classes.enter[dir], className, inner)}>
        {children}
      </div>
    </AutoHeight>
  );
}

// SlideKeep：和 Slide 配合，切换时不能卸载的内容（例如终端，卸载就断开了），放在 Slide 外面。
// active 时显示；进出的方向和 Slide 一致：index 是它自己的位置，current 是当前选中项的位置。
export function SlideKeep({ active, index, current, className, children }: { active: boolean; index: number; current: number; className?: string; children: React.ReactNode }) {
  const node = useRef<HTMLDivElement>(null);
  const last = useRef({ active, current, dir: 1 });
  const ghost = useRef<ReturnType<typeof captureGhost>>(null);
  if (last.current.active !== active) {
    const from = active ? last.current.current : index;
    const to = active ? index : current;
    if (!active) ghost.current = node.current && captureGhost(node.current);
    last.current = { active, current, dir: to === from ? 1 : to > from ? 2 : 0 };
  } else {
    last.current.current = current;
  }
  const dir = last.current.dir;
  useLayoutEffect(() => {
    if (!ghost.current) return;
    const stop = showGhost(ghost.current, cn(slideClasses.tabs.leave[dir], className));
    ghost.current = null;
    return stop;
    // 只在显示和隐藏切换时运行。
  }, [active]);
  // 从隐藏变成显示时动画会重新播放，不需要 key（加 key 会重建里面的内容）。
  return (
    <div ref={node} className={cn(active ? slideClasses.tabs.enter[dir] : "hidden", className)}>
      {children}
    </div>
  );
}

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
  const box = useTabIndicator(value, listRef);
  return (
    <TabsRoot value={value} onValueChange={(v) => onChange(v as T)} activationMode="manual">
      <TabsList ref={listRef} className={cn("relative flex gap-6 overflow-x-auto border-b [scrollbar-width:none] max-md:-mx-4 max-md:px-4", className)}>
        {items.map((it) => (
          <TabsTrigger
            key={it.value}
            value={it.value}
            className="-mb-px inline-flex h-10 shrink-0 cursor-pointer items-center gap-1 border-b-2 border-transparent text-sm whitespace-nowrap text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:ring-inset data-[state=active]:text-foreground"
          >
            {it.label}
            {it.count !== undefined && it.count > 0 && <Roll value={String(it.count)} className="text-xs text-muted-foreground" />}
          </TabsTrigger>
        ))}
        {box && <span aria-hidden className={cn("pointer-events-none absolute bottom-0 h-0.5 bg-foreground", box.motion)} style={{ left: box.left, right: box.right }} />}
      </TabsList>
    </TabsRoot>
  );
}

// SegmentedControl：两三个选项之间切换（例如"容器 / 容器组"）。
export function Segmented<T extends string>({ value, onChange, items, label }: { value: T; onChange: (v: T) => void; items: { value: T; label: React.ReactNode }[]; label?: string }) {
  const listRef = useRef<HTMLDivElement>(null);
  const box = useTabIndicator(value, listRef);
  return (
    <TabsRoot value={value} onValueChange={(v) => onChange(v as T)} activationMode="manual">
      <TabsList ref={listRef} aria-label={label} className="relative inline-flex h-8 rounded-lg bg-muted p-[3px] max-md:flex max-md:h-10 max-md:w-full">
        {box && <span aria-hidden className={cn("pointer-events-none absolute top-[3px] bottom-[3px] rounded-md bg-background shadow-[0_0_0_1px_var(--border)] dark:bg-input", box.motion)} style={{ left: box.left, right: box.right }} />}
        {items.map((it) => (
          <TabsTrigger
            key={it.value}
            value={it.value}
            className="relative h-full cursor-pointer rounded-md border border-transparent px-2 text-sm text-foreground/60 max-md:flex-1 max-md:whitespace-nowrap outline-none transition-colors focus-ring data-[state=active]:text-foreground"
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
// dismissible={false}：只能用框里的按钮关闭，用于只显示一次的内容（例如新令牌），免得误触关掉。
export function Dialog({
  open,
  onClose,
  title,
  description,
  dismissible = true,
  children,
  className,
}: {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  dismissible?: boolean;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <DialogRoot open={open} onOpenChange={(v) => !v && onClose()}>
      <DialogContent
        className={cn("group/dialog", className)}
        {...(description ? {} : { "aria-describedby": undefined })}
        onInteractOutside={dismissible ? undefined : (e) => e.preventDefault()}
        onEscapeKeyDown={dismissible ? undefined : (e) => e.preventDefault()}
        // 默认焦点给第一个输入框；没有输入框时给对话框本身。默认的第一个可聚焦元素常是问号，焦点一落上去说明就弹出来盖住标题。
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          const box = e.currentTarget as HTMLElement;
          if (box.contains(document.activeElement) && document.activeElement?.matches("input, textarea")) return;
          (box.querySelector<HTMLElement>("input:not([type=hidden]):not([disabled]):not([readonly]), textarea:not([disabled])") ?? box).focus();
        }}
      >
        <div className="relative px-6 pt-6 pb-4 max-md:px-4 max-md:pt-4">
          <div className="grid min-w-0 gap-1 [&>h2]:pr-6">
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </div>
          <DialogClose asChild>
            <Button size="icon" variant="ghost" className="absolute top-4 right-4 size-8 text-muted-foreground group-has-[[data-slot=dialog-actions]]/dialog:hidden hover:text-foreground max-md:top-2 max-md:right-2 max-md:size-10" aria-label={t.common.actions.close} readOnlyOk>
              <X />
            </Button>
          </DialogClose>
        </div>
        {/* 内容变了（切换选项多出一块、出现错误提示）时高度过渡，不直接跳。 */}
        {/* 列宽不超过对话框：长命令、长路径不会把对话框撑宽。 */}
        <AutoHeight className="grid grid-cols-[minmax(0,1fr)] gap-4 px-6 pb-6 max-md:px-4 max-md:pb-4" slot="dialog-body">{children}</AutoHeight>
      </DialogContent>
    </DialogRoot>
  );
}

// readOnlyOk：只读时仍可用的项（例如查看）。只读时其余项置灰，有一项可用菜单就能打开。
type MenuItem = { label: React.ReactNode; onSelect: () => void; danger?: boolean; disabled?: boolean; hint?: string; readOnlyOk?: boolean };

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
              className={cn("grid min-h-11 cursor-pointer content-center justify-items-center rounded-md px-2 text-base leading-6 outline-none transition-colors hover:bg-accent focus-ring disabled:cursor-not-allowed disabled:opacity-50", it.danger && "text-destructive")}
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
// 手机上是底部操作面板。title：面板的标题（灰字），例如文件名。ghost：无框按钮（手机标题栏的 ⋯）。className：按钮的样式。
export function Menu({ items: all, label = t.common.actions.more, rowStyle, ghost, title, className }: { items: MenuItem[]; label?: string; rowStyle?: boolean; ghost?: boolean; title?: React.ReactNode; className?: string }) {
  const mobile = useIsMobile();
  const [open, setOpen] = useState(false);
  const readOnly = useContext(ReadOnlyContext);
  const items = readOnly ? all.map((it) => (it.readOnlyOk ? it : { ...it, disabled: true })) : all;
  const trigger = (
    <Button variant={rowStyle || ghost ? "ghost" : "outline"} size={rowStyle ? "rowIcon" : "icon"} className={className} aria-label={label} readOnlyOk={all.some((it) => it.readOnlyOk)} onClick={mobile ? () => setOpen(true) : undefined}>
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


