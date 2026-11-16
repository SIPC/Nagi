import { createContext, useContext, useState } from "react";
import { closestCenter, DndContext, KeyboardSensor, MouseSensor, TouchSensor, useSensor, useSensors } from "@dnd-kit/core";
import { arrayMove, SortableContext, sortableKeyboardCoordinates, useSortable, type SortingStrategy } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { t } from "@/i18n";

// 项目列表的「自定义」排序：分组的顺序和项目的顺序拖动调整，只记在这个浏览器里（见 docs/panel.md#分组和标签）。
// 项目的顺序是一份全局列表，分组内、筛选后都按它排，所以在哪一组里拖都只改这一份。
const orderKey = "nagi.projects.order";

type Saved = { groups: string[]; projects: string[] };

function read(): Saved {
  try {
    const v = JSON.parse(localStorage.getItem(orderKey) ?? "{}");
    const strings = (x: unknown) => (Array.isArray(x) ? x.filter((s): s is string => typeof s === "string") : []);
    return { groups: strings(v?.groups), projects: strings(v?.projects) };
  } catch {
    return { groups: [], projects: [] };
  }
}

export function useProjectOrder() {
  const [saved, setSaved] = useState<Saved>(read);
  const save = (next: Saved) => {
    setSaved(next);
    try {
      localStorage.setItem(orderKey, JSON.stringify(next));
    } catch {
      // 存不了就只在这次有效。
    }
  };
  // 没排过的项目（例如新建的）放最前面，保持传进来的顺序（最近创建在前）；排过的按记下的顺序。
  const sortProjects = <T,>(list: T[], id: (x: T) => string) => {
    const pos = new Map(saved.projects.map((p, i) => [p, i]));
    const fresh = list.filter((x) => !pos.has(id(x)));
    const known = list.filter((x) => pos.has(id(x))).sort((a, b) => pos.get(id(a))! - pos.get(id(b))!);
    return [...fresh, ...known];
  };
  // 没排过的分组放在排过的后面，保持传进来的顺序（按名称）。
  const sortGroups = <T,>(list: T[], name: (x: T) => string) => {
    const pos = new Map(saved.groups.map((g, i) => [g, i]));
    const known = list.filter((x) => pos.has(name(x))).sort((a, b) => pos.get(name(a))! - pos.get(name(b))!);
    return [...known, ...list.filter((x) => !pos.has(name(x)))];
  };
  // all：现在所有项目按自定义顺序排好的 ID；visible：这次拖动所在列表里的 ID（某一组或筛选后的）。
  // 只调换 visible 在 all 里占的位置，其余项目不动。
  const moveProject = (all: string[], visible: string[], active: string, over: string) => {
    const moved = arrayMove(visible, visible.indexOf(active), visible.indexOf(over));
    const inList = new Set(visible);
    let i = 0;
    save({ ...saved, projects: all.map((id) => (inList.has(id) ? moved[i++] : id)) });
  };
  const moveGroup = (names: string[], active: string, over: string) => {
    save({ ...saved, groups: arrayMove(names, names.indexOf(active), names.indexOf(over)) });
  };
  return { sortProjects, sortGroups, moveProject, moveGroup };
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
