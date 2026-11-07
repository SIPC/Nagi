// 全局搜索（Cmd/Ctrl + K）：跳转到项目、容器、节点、日志和设置。
import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Activity, Box, Boxes, Plus, ScrollText, Search, Server, Settings } from "lucide-react";
import { Dialog as DialogPrimitive } from "radix-ui";
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { DialogContent } from "@/components/ui/dialog";
import { Button } from "@/components/ui";
import { nodeClient, projectClient } from "@/lib/api";
import { settingsTabsFor } from "@/pages/settings";
import { useRole } from "@/components/role";
import { t } from "@/i18n";

const isMac = /mac/i.test(navigator.platform);
const openEvent = "nagi:command-palette";

// 打开搜索的按钮：宽屏在导航里显示一整条，窄屏在顶部只显示图标。
export function SearchButton({ compact }: { compact?: boolean }) {
  const open = () => window.dispatchEvent(new Event(openEvent));
  if (compact) {
    return (
      <Button readOnlyOk variant="ghost" size="icon" onClick={open} aria-label={t.app.search.button} title={t.app.search.button}>
        <Search />
      </Button>
    );
  }
  return (
    <Button readOnlyOk variant="ghost" size="sm" className="w-full justify-start gap-2 px-2 text-muted-foreground" onClick={open}>
      <Search />
      {t.app.search.button}
      <kbd className="ml-auto px-1 font-sans text-xs leading-5">{isMac ? "⌘K" : "Ctrl K"}</kbd>
    </Button>
  );
}

// canCreateProject：备用面板和只读角色不能新建项目，不显示这一项。
export function CommandPalette({ canCreateProject }: { canCreateProject: boolean }) {
  const [open, setOpen] = useState(false);
  const navigate = useNavigate();
  const { isAdmin } = useRole();
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => projectClient.listProjects({}), enabled: open });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: () => nodeClient.listNodes({}), enabled: open });

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((v) => !v);
      }
    };
    const onOpen = () => setOpen(true);
    window.addEventListener("keydown", onKey);
    window.addEventListener(openEvent, onOpen);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener(openEvent, onOpen);
    };
  }, []);

  const go = (to: () => void) => {
    setOpen(false);
    to();
  };

  const list = projects.data?.projects ?? [];
  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-xl" aria-describedby={undefined}>
          <DialogPrimitive.Title className="sr-only">{t.app.search.button}</DialogPrimitive.Title>
          <Command>
            <CommandInput placeholder={t.app.search.placeholder} autoFocus />
            <CommandList>
              <CommandEmpty>{t.app.search.empty}</CommandEmpty>
              <CommandGroup heading={t.app.search.pages}>
                <CommandItem value="overview" keywords={[t.app.nav.overview]} onSelect={() => go(() => navigate({ to: "/overview" }))}>
                  <Activity />
                  {t.app.nav.overview}
                </CommandItem>
                {canCreateProject && (
                  <CommandItem value="new project" keywords={[t.app.search.newProject]} onSelect={() => go(() => navigate({ to: "/projects/new" }))}>
                    <Plus />
                    {t.app.search.newProject}
                  </CommandItem>
                )}
                {isAdmin && (
                  <CommandItem value="activity" keywords={[t.app.nav.activity]} onSelect={() => go(() => navigate({ to: "/activity" }))}>
                    <ScrollText />
                    {t.app.nav.activity}
                  </CommandItem>
                )}
              </CommandGroup>
              {list.length > 0 && (
                <CommandGroup heading={t.app.nav.projects}>
                  {list.map((s) => (
                    <CommandItem
                      key={s.project!.id}
                      value={`project ${s.project!.id}`}
                      keywords={[s.project!.name, s.project!.networkName]}
                      onSelect={() => go(() => navigate({ to: "/projects/$projectId", params: { projectId: s.project!.id } }))}
                    >
                      <Boxes />
                      {s.project!.name}
                      <span className="ml-auto font-mono text-xs text-muted-foreground">{s.project!.networkName}</span>
                    </CommandItem>
                  ))}
                </CommandGroup>
              )}
              {list.some((s) => s.project!.containers.length > 0) && (
                <CommandGroup heading={t.app.search.containers}>
                  {list.flatMap((s) =>
                    s.project!.containers.map((c) => (
                      <CommandItem
                        key={`${s.project!.id}/${c.name}`}
                        value={`container ${s.project!.id} ${c.name}`}
                        keywords={[c.displayName, c.name, s.project!.name].filter(Boolean)}
                        onSelect={() => go(() => navigate({ to: "/projects/$projectId/c/$container", params: { projectId: s.project!.id, container: c.name } }))}
                      >
                        <Box />
                        {c.displayName || c.name}
                        <span className="ml-auto text-xs text-muted-foreground">{s.project!.name}</span>
                      </CommandItem>
                    )),
                  )}
                </CommandGroup>
              )}
              {(nodes.data?.nodes.length ?? 0) > 0 && (
                <CommandGroup heading={t.app.nav.nodes}>
                  {nodes.data!.nodes.map((n) => (
                    <CommandItem key={n.id} value={`node ${n.id}`} keywords={[n.name, n.address]} onSelect={() => go(() => navigate({ to: "/nodes/$nodeId", params: { nodeId: n.id } }))}>
                      <Server />
                      {n.name}
                      <span className="ml-auto font-mono text-xs text-muted-foreground">{n.address}</span>
                    </CommandItem>
                  ))}
                </CommandGroup>
              )}
              <CommandGroup heading={t.app.nav.settings}>
                {settingsTabsFor(isAdmin).map((tab) => (
                  <CommandItem key={tab.value} value={`settings ${tab.value}`} keywords={[tab.label, t.app.nav.settings]} onSelect={() => go(() => navigate({ to: "/settings", search: { tab: tab.value } }))}>
                    <Settings />
                    {tab.label}
                  </CommandItem>
                ))}
              </CommandGroup>
            </CommandList>
          </Command>
        </DialogContent>
    </DialogPrimitive.Root>
  );
}
