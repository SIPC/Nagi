// shadcn/ui Sheet（基于 Radix Dialog），从右侧滑出；颜色和动画按 docs/ui.md 调整。手机上从底部出现，撑满宽度。
import { Dialog as SheetPrimitive } from "radix-ui";
import { XIcon } from "lucide-react";
import type * as React from "react";
import { cn } from "@/lib/utils";

export const Sheet = SheetPrimitive.Root;
export const SheetClose = SheetPrimitive.Close;

export function SheetContent({ className, children, closeLabel, ...props }: React.ComponentProps<typeof SheetPrimitive.Content> & { closeLabel: string }) {
  return (
    <SheetPrimitive.Portal>
      <SheetPrimitive.Overlay className="fixed inset-0 z-50 bg-overlay data-[state=open]:animate-fade-in data-[state=closed]:animate-fade-out" />
      <SheetPrimitive.Content
        className={cn(
          "fixed inset-y-0 right-0 z-50 flex h-full w-full max-w-[560px] flex-col overflow-y-auto overscroll-contain border-l bg-popover shadow-popover outline-none data-[state=open]:animate-slide-from-right data-[state=closed]:animate-fade-out max-md:inset-x-0 max-md:top-auto max-md:bottom-0 max-md:h-auto max-md:max-h-[calc(100dvh-3rem)] max-md:max-w-none max-md:rounded-t-lg max-md:border-t max-md:border-l-0 max-md:data-[state=open]:animate-sheet-in max-md:data-[state=closed]:animate-sheet-out",
          className,
        )}
        {...props}
      >
        {children}
        <SheetPrimitive.Close
          aria-label={closeLabel}
          className="absolute top-5 right-5 flex size-8 items-center justify-center rounded-md text-muted-foreground transition-colors outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring max-md:top-3 max-md:right-3"
        >
          <XIcon className="size-4" />
        </SheetPrimitive.Close>
      </SheetPrimitive.Content>
    </SheetPrimitive.Portal>
  );
}

export function SheetHeader({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid gap-1 p-6 pr-14 pb-4 max-md:p-4 max-md:pr-12", className)} {...props} />;
}

export function SheetTitle({ className, ...props }: React.ComponentProps<typeof SheetPrimitive.Title>) {
  return <SheetPrimitive.Title className={cn("text-base leading-6 font-semibold", className)} {...props} />;
}

export function SheetDescription({ className, ...props }: React.ComponentProps<typeof SheetPrimitive.Description>) {
  return <SheetPrimitive.Description className={cn("text-[13px] leading-5 text-muted-foreground", className)} {...props} />;
}

export function SheetBody({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("grid gap-6 px-6 pb-6 max-md:px-4 max-md:pb-4", className)} {...props} />;
}
