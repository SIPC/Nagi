// shadcn/ui Tabs。这里只用作切换（内容由调用方按当前值显示），所以没有 TabsContent。
import { Tabs as TabsPrimitive } from "radix-ui";

export const Tabs = TabsPrimitive.Root;
export const TabsList = TabsPrimitive.List;
export const TabsTrigger = TabsPrimitive.Trigger;
