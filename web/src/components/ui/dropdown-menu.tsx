// shadcn/ui DropdownMenu。
import { DropdownMenu as DropdownMenuPrimitive } from "radix-ui";
import type * as React from "react";
import { cn } from "@/lib/utils";

export const DropdownMenu = DropdownMenuPrimitive.Root;
export const DropdownMenuTrigger = DropdownMenuPrimitive.Trigger;

export function DropdownMenuContent({ className, sideOffset = 4, align = "end", ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Content>) {
  return (
    <DropdownMenuPrimitive.Portal>
      <DropdownMenuPrimitive.Content
        sideOffset={sideOffset}
        align={align}
        className={cn("z-50 w-max max-w-64 min-w-32 rounded-md bg-popover p-1 shadow-popover origin-(--radix-dropdown-menu-content-transform-origin) data-[state=open]:animate-popover-in data-[state=closed]:animate-fade-out", className)}
        {...props}
      />
    </DropdownMenuPrimitive.Portal>
  );
}

export function DropdownMenuLabel({ className, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Label>) {
  return <DropdownMenuPrimitive.Label className={cn("px-2 py-1.5 text-xs text-muted-foreground", className)} {...props} />;
}

export function DropdownMenuSeparator({ className, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Separator>) {
  return <DropdownMenuPrimitive.Separator className={cn("-mx-1 my-1 h-px bg-border", className)} {...props} />;
}

export function DropdownMenuItem({ className, variant, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Item> & { variant?: "destructive" }) {
  return (
    <DropdownMenuPrimitive.Item
      className={cn(
        "cursor-pointer rounded-sm px-2 py-1.5 text-sm outline-none select-none data-[disabled]:pointer-events-none data-[disabled]:opacity-60 data-[highlighted]:bg-accent",
        variant === "destructive" && "text-destructive",
        className,
      )}
      {...props}
    />
  );
}
