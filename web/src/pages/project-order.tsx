import { createContext, useContext, useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ChevronRight, GripVertical } from "lucide-react";
import { closestCenter, DndContext, KeyboardSensor, MouseSensor, TouchSensor, useSensor, useSensors } from "@dnd-kit/core";
import { arrayMove, SortableContext, sortableKeyboardCoordinates, useSortable, type SortingStrategy } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { Collapse } from "@/components/ui";
import { authClient, errorMessage } from "@/lib/api";
import { cn } from "@/lib/utils";
import { t } from "@/i18n";

// 项目页、节点页的排序：排序方式，以及「自定义」时分组和项目（节点）的顺序（拖动调整）。存在面板上，每个成员各自一份，
// 换浏览器、换设备都一样（见 docs/panel.md#分组和标签）。分组的标题行、拖动也在这里，两页共用。
// 项目（节点）的顺序是一份全局列表，分组内、筛选后都按它排，所以在哪一组里拖都只改这一份。
// sort 为 "created" 是默认顺序：项目是最近创建在前，节点是加入时间。
export type ProjectSort = "created" | "name" | "custom";

type Saved = { sort: ProjectSort; groups: string[]; items: string[] };

const orderQueryKey = ["projectOrder"];
const nodeOrderQueryKey = ["nodeOrder"];

const toSort = (s: string): ProjectSort => (s === "name" || s === "custom" ? s : "created");

// 以前只记在浏览器里（nagi.projects.order）：面板上还没有时搬过去，然后删掉。
const legacyKey = "nagi.projects.order";

function readLegacy(): Omit<Saved, "sort"> | undefined {
  try {
    const raw = localStorage.getItem(legacyKey);
    if (raw === null) return undefined;
    const v = JSON.parse(raw);
    const strings = (x: unknown) => (Array.isArray(x) ? x.filter((s): s is string => typeof s === "string" && s !== "") : []);
    return { groups: strings(v?.groups), items: strings(v?.projects) };
  } catch {
    return undefined;
  }
}

function dropLegacy() {
  try {
    localStorage.removeItem(legacyKey);
  } catch {
    // 删不了也没关系：面板上有了以后不再读它。
  }
}

// useOrder：一页的排序。freshFirst：没排过的项目（节点）放在排过的前面还是后面。
function useOrder({
  queryKey,
  load,
  store,
  freshFirst,
}: {
  queryKey: string[];
  load: () => Promise<Saved>;
  store: (o: Saved) => Promise<unknown>;
  freshFirst: boolean;
}) {
  const qc = useQueryClient();
  const query = useQuery({ queryKey, queryFn: load, staleTime: Infinity });
  const saved: Saved = query.data ?? { sort: "created", groups: [], items: [] };
  // 同一个 scope 的保存按顺序发出，连续拖动时后一次不会被前一次覆盖。
  const mutation = useMutation({
    scope: { id: queryKey[0] },
    mutationFn: store,
    onError: (e) => {
      toast.error(errorMessage(e));
      qc.invalidateQueries({ queryKey });
    },
  });
  const save = (next: Saved) => {
    qc.setQueryData(queryKey, next);
    mutation.mutate(next);
  };
  const setSort = (sort: ProjectSort) => {
    if (sort !== saved.sort) save({ ...saved, sort });
  };
  // 没排过的（例如新建的）按 freshFirst 放在最前或最后，保持传进来的顺序；排过的按记下的顺序。
  const sortItems = <T,>(list: T[], id: (x: T) => string) => {
    const pos = new Map(saved.items.map((p, i) => [p, i]));
    const fresh = list.filter((x) => !pos.has(id(x)));
    const known = list.filter((x) => pos.has(id(x))).sort((a, b) => pos.get(id(a))! - pos.get(id(b))!);
    return freshFirst ? [...fresh, ...known] : [...known, ...fresh];
  };
  // 没排过的分组放在排过的后面，保持传进来的顺序（按名称）。
  const sortGroups = <T,>(list: T[], name: (x: T) => string) => {
    const pos = new Map(saved.groups.map((g, i) => [g, i]));
    const known = list.filter((x) => pos.has(name(x))).sort((a, b) => pos.get(name(a))! - pos.get(name(b))!);
    return [...known, ...list.filter((x) => !pos.has(name(x)))];
  };
  // all：现在所有项目（节点）按自定义顺序排好的 ID；visible：这次拖动所在列表里的 ID（某一组或筛选后的）。
  // 只调换 visible 在 all 里占的位置，其余的不动。
  const moveItem = (all: string[], visible: string[], active: string, over: string) => {
    const moved = arrayMove(visible, visible.indexOf(active), visible.indexOf(over));
    const inList = new Set(visible);
    let i = 0;
    save({ ...saved, items: all.map((id) => (inList.has(id) ? moved[i++] : id)) });
  };
  const moveGroup = (names: string[], active: string, over: string) => {
    save({ ...saved, groups: arrayMove(names, names.indexOf(active), names.indexOf(over)) });
  };
  return { sort: saved.sort, loaded: query.isSuccess, data: query.data, save, setSort, sortItems, sortGroups, moveItem, moveGroup };
}

export function useProjectOrder() {
  const o = useOrder({
    queryKey: orderQueryKey,
    load: async () => {
      const r = await authClient.getProjectOrder({});
      return { sort: toSort(r.sort), groups: r.groups, items: r.projects };
    },
    store: (o) => authClient.setProjectOrder({ sort: o.sort === "created" ? "" : o.sort, groups: o.groups, projects: o.items }),
    freshFirst: true,
  });
  useEffect(() => {
    if (!o.data) return;
    const legacy = readLegacy();
    if (!legacy) return;
    if (o.data.groups.length === 0 && o.data.items.length === 0 && (legacy.groups.length > 0 || legacy.items.length > 0)) {
      // 拖动过就是想按自定义顺序看。
      o.save({ ...o.data, ...legacy, sort: o.data.sort === "created" ? "custom" : o.data.sort });
    }
    dropLegacy();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [o.data]);
  return o;
}

// useNodeOrder：节点页的排序。默认按加入时间，没排过的节点（新加入的）放在最后。
export function useNodeOrder() {
  return useOrder({
    queryKey: nodeOrderQueryKey,
    load: async () => {
      const r = await authClient.getNodeOrder({});
      return { sort: toSort(r.sort), groups: r.groups, items: r.nodes };
    },
    store: (o) => authClient.setNodeOrder({ sort: o.sort === "created" ? "" : o.sort, groups: o.groups, nodes: o.items }),
    freshFirst: false,
  });
}

const ReorderEnabled = createContext(false);

// Reorder：一组可以拖动排序的元素。enabled 为假时照常显示，不能拖。
// 鼠标移动 6 像素、触屏按住 250ms 才开始拖，所以点击和滚动不受影响；键盘用空格拿起和放下，方向键移动，Esc 取消
// （回车留给链接本身）。
export function Reorder({
  ids,
  name,
  enabled,
  strategy,
  onMove,
  children,
}: {
  ids: string[];
  name: (id: string) => string;
  enabled: boolean;
  strategy: SortingStrategy;
  onMove: (active: string, over: string) => void;
  children: React.ReactNode;
}) {
  const sensors = useSensors(
    useSensor(MouseSensor, { activationConstraint: { distance: 6 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 250, tolerance: 6 } }),
    useSensor(KeyboardSensor, {
      coordinateGetter: sortableKeyboardCoordinates,
      keyboardCodes: { start: ["Space"], cancel: ["Escape"], end: ["Space", "Enter"] },
    }),
  );
  const m = t.projects.reorder;
  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      accessibility={{
        screenReaderInstructions: { draggable: m.instructions },
        announcements: {
          onDragStart: ({ active }) => m.picked(name(String(active.id))),
          onDragOver: ({ active, over }) => (over ? m.over(name(String(active.id)), name(String(over.id))) : undefined),
          onDragEnd: ({ active }) => m.dropped(name(String(active.id))),
          onDragCancel: ({ active }) => m.cancelled(name(String(active.id))),
        },
      }}
      onDragStart={blockNextClick}
      onDragEnd={({ active, over }) => {
        if (over && active.id !== over.id) onMove(String(active.id), String(over.id));
      }}
    >
      <SortableContext items={ids} strategy={strategy} disabled={!enabled}>
        <ReorderEnabled.Provider value={enabled}>{children}</ReorderEnabled.Provider>
      </SortableContext>
    </DndContext>
  );
}

// blockNextClick：拖完松手时浏览器还会补发一次点击。dnd-kit 在 document 上拦下它的传播，但没有取消默认行为：
// 落在按钮上没事，落在项目卡片（链接）上，界面的处理函数收不到，浏览器却照常打开链接（整页刷新）。
// 所以拖动期间在 window 上（比 document 更早）取消这次点击的默认行为，松手后一小会儿撤掉。
function blockNextClick() {
  const block = (e: MouseEvent) => e.preventDefault();
  const stop = () => {
    window.removeEventListener("mouseup", stop, true);
    window.removeEventListener("touchend", stop, true);
    window.removeEventListener("keyup", stop, true);
    setTimeout(() => window.removeEventListener("click", block, true), 250);
  };
  window.addEventListener("click", block, true);
  window.addEventListener("mouseup", stop, true);
  window.addEventListener("touchend", stop, true);
  window.addEventListener("keyup", stop, true);
}

// useReorderItem：Reorder 里的一个元素。ref 给整块元素，handle 给按住拖动的部分（项目卡片是整张卡片，分组是标题行）。
// 不用 dnd-kit 加的 role 和 tabIndex：卡片本身是链接、标题本身是按钮，焦点和角色保持原样。
export function useReorderItem(id: string) {
  const enabled = useContext(ReorderEnabled);
  const s = useSortable({ id });
  const { role: _role, tabIndex: _tabIndex, ...attributes } = s.attributes;
  return {
    ref: s.setNodeRef,
    style: { transform: CSS.Translate.toString(s.transform), transition: s.transition } as React.CSSProperties,
    // handle 展开到按住拖动的元素上；它和 ref 不是同一个元素时，再把 handleRef 给它。
    handle: enabled ? { ...attributes, ...s.listeners } : {},
    handleRef: s.setActivatorNodeRef,
    enabled,
    dragging: s.isDragging,
  };
}

// useCollapsed：收起的分组，记在这个浏览器里（storageKey 每页一个）。
export function useCollapsed(storageKey: string) {
  const [collapsed, setCollapsed] = useState<string[]>(() => {
    try {
      const v = JSON.parse(localStorage.getItem(storageKey) ?? "[]");
      return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
    } catch {
      return [];
    }
  });
  const toggle = (key: string) => {
    const next = collapsed.includes(key) ? collapsed.filter((k) => k !== key) : [...collapsed, key];
    setCollapsed(next);
    try {
      localStorage.setItem(storageKey, JSON.stringify(next));
    } catch {
      // 存不了就只在这次有效。
    }
  };
  return { collapsed, toggle };
}

// GroupSection：一个分组：标题行和里面的卡片（行）。调整顺序时按住标题行上下拖动调整分组的顺序。
export function GroupSection({ id, name, count, open, onToggle, children }: { id: string; name: string; count: number; open: boolean; onToggle: () => void; children: React.ReactNode }) {
  const r = useReorderItem(id);
  return (
    <section ref={r.ref} style={r.style} className={cn("min-w-0", r.dragging && "relative z-10 rounded-lg bg-background shadow-lg")}>
      <GroupHeader name={name} count={count} open={open} onToggle={onToggle} handle={r.handle} handleRef={r.handleRef} sortable={r.enabled} />
      {/* 展开、收起时高度和透明度一起过渡，下面的分组跟着移动（ui.md 的 Collapse）。间距放在里面，收起后不留空。 */}
      <Collapse open={open}>
        <div className="pt-2">{children}</div>
      </Collapse>
    </section>
  );
}

// GroupHeader：分组的标题行，点击展开或收起。参考 Grafana 的文件夹行：箭头、名称、数量。
function GroupHeader({
  name,
  count,
  open,
  onToggle,
  handle,
  handleRef,
  sortable,
}: {
  name: string;
  count: number;
  open: boolean;
  onToggle: () => void;
  handle?: React.HTMLAttributes<HTMLElement>;
  handleRef?: (el: HTMLElement | null) => void;
  sortable?: boolean;
}) {
  return (
    <button
      type="button"
      ref={handleRef}
      {...handle}
      aria-expanded={open}
      aria-label={open ? t.labels.collapse(name) : t.labels.expand(name)}
      className={cn(
        "-mx-2 flex h-8 w-fit max-w-full cursor-pointer items-center gap-2 rounded-md px-2 text-left outline-none hover:bg-accent focus-ring",
        sortable && "cursor-grab touch-manipulation active:cursor-grabbing",
      )}
      onClick={onToggle}
    >
      {sortable && <GripVertical className="size-4 shrink-0 text-muted-foreground" aria-hidden />}
      <ChevronRight className={cn("size-4 shrink-0 text-muted-foreground transition-transform duration-240", open && "rotate-90")} />
      <span className="truncate font-medium">{name}</span>
      <span className="shrink-0 text-[13px] text-muted-foreground tabular-nums">{count}</span>
    </button>
  );
}
