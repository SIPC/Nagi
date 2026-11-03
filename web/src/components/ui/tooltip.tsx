// shadcn/ui Tooltip。
import { Tooltip as TooltipPrimitive } from "radix-ui";
import type * as React from "react";
import { cn } from "@/lib/utils";

export const TooltipProvider = TooltipPrimitive.Provider;
export const Tooltip = TooltipPrimitive.Root;
export const TooltipTrigger = TooltipPrimitive.Trigger;

export function TooltipContent({ className, sideOffset = 6, collisionPadding = 8, ...props }: React.ComponentProps<typeof TooltipPrimitive.Content>) {
  return (
    <TooltipPrimitive.Portal>
      <TooltipPrimitive.Content
        sideOffset={sideOffset}
        collisionPadding={collisionPadding}
        className={cn("z-50 w-max max-w-72 rounded-md bg-popover px-2.5 py-1.5 text-xs leading-relaxed font-normal text-foreground shadow-popover data-[state=delayed-open]:animate-fade-in", className)}
        {...props}
      />
    </TooltipPrimitive.Portal>
  );
}
