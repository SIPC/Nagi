// 导航底部的用户卡片：头像、名称和角色，点开是个人资料、账号安全、主题、语言和退出。窄屏只显示头像。
import { useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Languages, LogOut, Monitor, Moon, ShieldCheck, Sun, UserRound } from "lucide-react";
import { toast } from "sonner";
import { Alert, Button, Dialog, DialogActions, Field, FormActions, Input, QueryError, RowsSkeleton, Section, Spinner } from "@/components/ui";
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { nextLocale, switchLanguage } from "@/components/language";
import type { MeResponse } from "@/gen/nagi/v1/api_pb";
import { t } from "@/i18n";
import { authClient, errorMessage } from "@/lib/api";
import { useUnsaved } from "@/components/unsaved";
import { cn } from "@/lib/utils";

export type ThemeMode = "light" | "dark" | "system";

const themeIcons = { light: Sun, dark: Moon, system: Monitor };

// 头像存成很小的正方形：显示最大 64px，留出高分屏的余量。
const avatarSize = 128;
const maxImageBytes = 20 << 20;

export function avatarSrc(avatar: Uint8Array | undefined, type: string | undefined) {
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

// avatarGradient：没上传头像时的渐变，按用户名算颜色，同一个用户名永远是同一组颜色，改显示名称不变。
function avatarGradient(username: string) {
  let h = 2166136261;
  for (const c of username) h = Math.imul(h ^ c.codePointAt(0)!, 16777619);
  h >>>= 0;
  const from = h % 360;
  const to = (from + 50 + ((h >>> 9) % 90)) % 360;
  return `linear-gradient(135deg, oklch(0.84 0.12 ${from}), oklch(0.6 0.2 ${to}))`;
}

export function UserAvatar({ username, src, className }: { username: string; src?: string; className?: string }) {
  return (
    <Avatar className={className}>
      {src && <AvatarImage src={src} alt="" />}
      <AvatarFallback style={{ backgroundImage: avatarGradient(username) }} />
    </Avatar>
  );
}

// ProfileForm：个人资料表单。用户菜单的对话框和设置页的「个人资料」共用；传 onClose 就是对话框的样式。
// 设置页里有未保存的修改时离开前确认；保存后由上层换 key 重新挂载，确认的起点跟着更新。
function ProfileForm({ me, onClose, onSaved }: { me: MeResponse; onClose?: () => void; onSaved?: () => void }) {
  const queryClient = useQueryClient();
  const input = useRef<HTMLInputElement>(null);
  const [displayName, setDisplayName] = useState(me.displayName);
  const [avatar, setAvatar] = useState({ bytes: me.avatar, type: me.avatarType });
  const [error, setError] = useState("");
  useUnsaved({ displayName, avatar: Array.from(avatar.bytes) }, !onClose);
  const save = useMutation({
    mutationFn: () => authClient.updateProfile({ displayName, avatar: avatar.bytes }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      toast.success(t.account.saved);
      onSaved?.();
      onClose?.();
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
  const submit = (
    <Button type="submit" variant={onClose ? "default" : "outline"} disabled={save.isPending}>
      {save.isPending && <Spinner />}
      {t.common.actions.save}
    </Button>
  );
  return (
    <form
      className="grid max-w-[672px] gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        setError("");
        save.mutate();
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <UserAvatar username={me.username} src={avatarSrc(avatar.bytes, avatar.type)} className="mr-2 size-12" />
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
        <Button variant="outline" onClick={() => input.current?.click()}>
          {t.account.upload}
        </Button>
        <Button variant="ghost" disabled={avatar.bytes.length === 0} onClick={() => setAvatar({ bytes: new Uint8Array(), type: "" })}>
          {t.account.remove}
        </Button>
        <p className="w-full leading-5 text-muted-foreground">{t.account.avatarHint}</p>
      </div>
      <Field label={t.account.displayName} htmlFor="profile-name" help={t.account.displayNameHint(me.username)} className={onClose ? undefined : "sm:max-w-[328px]"}>
        <Input id="profile-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={32} placeholder={me.username} autoComplete="off" />
      </Field>
      {error && <Alert>{error}</Alert>}
      {onClose ? (
        <DialogActions>
          <Button variant="outline" onClick={onClose}>
            {t.common.actions.cancel}
          </Button>
          {submit}
        </DialogActions>
      ) : (
        <FormActions>{submit}</FormActions>
      )}
    </form>
  );
}

function ProfileDialog({ me, onClose }: { me: MeResponse; onClose: () => void }) {
  return (
    <Dialog open onClose={onClose} title={t.account.profile}>
      <ProfileForm me={me} onClose={onClose} />
    </Dialog>
  );
}

// ProfileCard：设置页的「个人资料」，所有角色都有。
export function ProfileCard() {
  const me = useQuery({ queryKey: ["me"], queryFn: () => authClient.me({}) });
  const [version, setVersion] = useState(0);
  return (
    <Section title={t.settings.tabs.profile.label}>
      {me.isPending ? (
        <RowsSkeleton count={2} />
      ) : !me.data ? (
        <QueryError error={errorMessage(me.error)} onRetry={() => me.refetch()} />
      ) : (
        <ProfileForm key={version} me={me.data} onSaved={() => setVersion((v) => v + 1)} />
      )}
    </Section>
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
            // 宽屏上按钮里有可见的名称，读屏直接念它；窄屏只有头像，才补上名称。
            aria-label={compact ? name : undefined}
            title={compact ? name : undefined}
            className={cn(
              "flex items-center gap-2 rounded-md text-left text-sm outline-none transition-colors focus-ring",
              compact ? "p-1" : "h-10 w-full px-2 hover:bg-accent/50 data-[state=open]:bg-accent",
            )}
          >
            <UserAvatar username={me.username} src={src} className={compact ? "size-7" : "size-6"} />
            {!compact && (
              <>
                <span className="min-w-0 flex-1 truncate">{name}</span>
                <span className="shrink-0 text-xs text-muted-foreground">{role}</span>
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
          <DropdownMenuRadioGroup value={theme} onValueChange={(v) => onTheme(v as ThemeMode)}>
            {(["light", "dark", "system"] as const).map((mode) => {
              const Icon = themeIcons[mode];
              return (
                <DropdownMenuRadioItem key={mode} value={mode} className={item}>
                  <Icon />
                  {t.app.theme[mode]}
                </DropdownMenuRadioItem>
              );
            })}
          </DropdownMenuRadioGroup>
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
