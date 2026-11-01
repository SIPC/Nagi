// shadcn/ui AlertDialog：需要用户明确选择的确认框，点遮罩不会关闭。
import { AlertDialog as AlertDialogPrimitive } from "radix-ui";
import type * as React from "react";
import { cn } from "@/lib/utils";

export const AlertDialog = AlertDialogPrimitive.Root;
export const AlertDialogAction = AlertDialogPrimitive.Action;
export const AlertDialogCancel = AlertDialogPrimitive.Cancel;

export function AlertDialogContent({ className, children, ...props }: React.ComponentProps<typeof AlertDialogPrimitive.Content>) {
  return (
    <AlertDialogPrimitive.Portal>
      <AlertDialogPrimitive.Overlay className="fixed inset-0 z-50 bg-black/50 data-[state=open]:animate-fade-in" />
      <AlertDialogPrimitive.Content
        className={cn(
          "fixed top-[12vh] left-1/2 z-50 grid max-h-[80vh] w-[calc(100%-2rem)] max-w-md -translate-x-1/2 gap-4 overflow-y-auto rounded-lg border bg-card p-5 shadow-xl outline-none data-[state=open]:animate-fade-in",
          className,
        )}
        {...props}
      >
        {children}
      </AlertDialogPrimitive.Content>
    </AlertDialogPrimitive.Portal>
  );
}

export function AlertDialogTitle({ className, ...props }: React.ComponentProps<typeof AlertDialogPrimitive.Title>) {
  return <AlertDialogPrimitive.Title className={cn("text-sm font-semibold", className)} {...props} />;
}

export function AlertDialogDescription({ className, ...props }: React.ComponentProps<typeof AlertDialogPrimitive.Description>) {
  return <AlertDialogPrimitive.Description className={cn("text-[13px] text-pretty whitespace-pre-line text-muted-foreground", className)} {...props} />;
}
