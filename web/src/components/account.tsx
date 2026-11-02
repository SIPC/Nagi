// 导航底部的用户卡片：头像、名称和角色，点开是个人资料、账号安全、主题、语言和退出。窄屏只显示头像。
import { useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Check, ChevronsUpDown, Languages, LogOut, Monitor, Moon, ShieldCheck, Sun, UserRound } from "lucide-react";
import { toast } from "sonner";
import { Alert, Button, Dialog, Field, Input, Spinner } from "@/components/ui";
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { nextLocale, switchLanguage } from "@/components/language";
import type { MeResponse } from "@/gen/nagi/v1/api_pb";
import { t } from "@/i18n";
import { authClient, errorMessage } from "@/lib/api";
import { cn } from "@/lib/utils";

export type ThemeMode = "light" | "dark" | "system";

const themeIcons = { light: Sun, dark: Moon, system: Monitor };

// 头像存成很小的正方形：显示最大 64px，留出高分屏的余量。
const avatarSize = 128;
const maxImageBytes = 20 << 20;

function avatarSrc(avatar: Uint8Array | undefined, type: string | undefined) {
  if (!avatar?.length || !type) return undefined;
  let binary = "";
  for (const b of avatar) binary += String.fromCharCode(b);
  return `data:${type};base64,${btoa(binary)}`;
}

// cropAvatar 从图片中间裁出正方形并缩小。不支持 WebP 编码的浏览器会得到 PNG，面板都接受。
async function cropAvatar(file: File) {
  const image = await createImageBitmap(file);
  const side = Math.min(image.width, image.height);
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = avatarSize;
  canvas.getContext("2d")!.drawImage(image, (image.width - side) / 2, (image.height - side) / 2, side, side, 0, 0, avatarSize, avatarSize);
  image.close();
  const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, "image/webp", 0.9));
  if (!blob) throw new Error(t.account.notImage);
  return { bytes: new Uint8Array(await blob.arrayBuffer()), type: blob.type };
}

function UserAvatar({ name, src, className }: { name: string; src?: string; className?: string }) {
  return (
    <Avatar className={className}>
      {src && <AvatarImage src={src} alt="" />}
      <AvatarFallback>{Array.from(name)[0]?.toUpperCase()}</AvatarFallback>
    </Avatar>
  );
}

function ProfileDialog({ me, onClose }: { me: MeResponse; onClose: () => void }) {
  const queryClient = useQueryClient();
  const input = useRef<HTMLInputElement>(null);
  const [displayName, setDisplayName] = useState(me.displayName);
  const [avatar, setAvatar] = useState({ bytes: me.avatar, type: me.avatarType });
  const [error, setError] = useState("");
  const save = useMutation({
    mutationFn: () => authClient.updateProfile({ displayName, avatar: avatar.bytes }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      toast.success(t.account.saved);
      onClose();
    },
    onError: (err) => setError(errorMessage(err)),
  });
  const pick = async (file: File | undefined) => {
    if (!file) return;
    setError("");
    if (file.size > maxImageBytes) {
      setError(t.account.tooLarge);
      return;
    }
    try {
      setAvatar(await cropAvatar(file));
    } catch {
      setError(t.account.notImage);
    }
  };
  return (
    <Dialog open onClose={onClose} title={t.account.profile}>
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          setError("");
          save.mutate();
        }}
      >
        <Field label={t.account.avatar} hint={t.account.avatarHint}>
          <div className="flex items-center gap-3">
            <UserAvatar name={displayName.trim() || me.username} src={avatarSrc(avatar.bytes, avatar.type)} className="size-16" />
            <input
              ref={input}
              type="file"
              accept="image/png,image/jpeg,image/webp,image/gif"
              className="hidden"
              aria-label={t.account.upload}
              onChange={(e) => {
                const file = e.target.files?.[0];
                e.target.value = ""; // 换了图再选回同一个文件时也要触发。
                pick(file);
              }}
            />
            <Button size="sm" variant="outline" onClick={() => input.current?.click()}>
              {t.account.upload}
            </Button>
            {avatar.bytes.length > 0 && (
              <Button size="sm" variant="ghost" onClick={() => setAvatar({ bytes: new Uint8Array(), type: "" })}>
                {t.account.remove}
              </Button>
            )}
          </div>
        </Field>
        <Field label={t.account.displayName} htmlFor="profile-name" hint={t.account.displayNameHint(me.username)}>
          <Input id="profile-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={32} placeholder={me.username} autoComplete="off" />
        </Field>
        {error && <Alert>{error}</Alert>}
        <div className="flex justify-end gap-2">
          <Button size="sm" variant="ghost" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          <Button type="submit" size="sm" disabled={save.isPending}>
            {save.isPending && <Spinner />}
            {t.common.actions.save}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function UserMenu({
  me,
  theme,
  onTheme,
  onLogout,
  readOnly,
  compact = false,
}: {
  me: MeResponse | undefined;
  theme: ThemeMode;
  onTheme: (mode: ThemeMode) => void;
  onLogout: () => void;
  readOnly: boolean;
  compact?: boolean;
}) {
  const navigate = useNavigate();
  const [editing, setEditing] = useState(false);
  if (!me) return null;
  const name = me.displayName || me.username;
  const role = t.members.roles[me.role as keyof typeof t.members.roles];
  const src = avatarSrc(me.avatar, me.avatarType);
  const item = "flex items-center gap-2 [&_svg]:size-3.5";
  return (
    <>
      <DropdownMenu modal={false}>
        <DropdownMenuTrigger asChild>
          <button
            type="button"
            aria-label={t.account.menu}
            title={compact ? name : undefined}
            className={cn(
              "flex items-center gap-2.5 rounded-lg text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
              compact ? "p-1" : "w-full border bg-card p-2 hover:bg-accent data-[state=open]:bg-accent",
            )}
          >
            <UserAvatar name={name} src={src} className={compact ? "size-7" : undefined} />
            {!compact && (
              <>
                <span className="grid min-w-0 flex-1">
                  <span className="truncate text-[13px] font-medium">{name}</span>
                  <span className="truncate text-xs text-muted-foreground">{role}</span>
                </span>
                <ChevronsUpDown className="size-3.5 shrink-0 text-muted-foreground" />
              </>
            )}
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent side={compact ? "bottom" : "top"} align={compact ? "end" : "start"} className={cn("min-w-52", !compact && "w-[var(--radix-dropdown-menu-trigger-width)]")}>
          <DropdownMenuLabel className="truncate">{t.account.signedInAs(me.username)}</DropdownMenuLabel>
          <DropdownMenuItem className={item} disabled={readOnly} onSelect={() => setEditing(true)}>
            <UserRound />
            {t.account.profile}
          </DropdownMenuItem>
          <DropdownMenuItem className={item} onSelect={() => navigate({ to: "/settings", search: { tab: "security" } })}>
            <ShieldCheck />
            {t.account.security}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuLabel>{t.app.theme.label}</DropdownMenuLabel>
          {(["light", "dark", "system"] as const).map((mode) => {
            const Icon = themeIcons[mode];
            return (
              <DropdownMenuItem key={mode} className={item} onSelect={() => onTheme(mode)}>
                <Icon />
                {t.app.theme[mode]}
                {theme === mode && <Check className="ml-auto" />}
              </DropdownMenuItem>
            );
          })}
          <DropdownMenuSeparator />
          <DropdownMenuItem className={item} onSelect={() => switchLanguage(true)}>
            <Languages />
            {nextLocale.label}
          </DropdownMenuItem>
          <DropdownMenuItem className={item} onSelect={onLogout}>
            <LogOut />
            {t.app.logout}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {editing && <ProfileDialog me={me} onClose={() => setEditing(false)} />}
    </>
  );
}
