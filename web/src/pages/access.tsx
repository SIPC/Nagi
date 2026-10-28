import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, settingsClient } from "@/lib/api";
import {
  Alert,
  Button,
  CopyButton,
  Input,
  SettingRow,
  SettingRows,
  Spinner,
} from "@/components/ui";
import { t } from "@/i18n";

// 「面板访问」区块：节点和浏览器怎么找到本面板——对外地址、公钥指纹、绑定的域名。
export function PanelAccessCard() {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ["settings"],
    queryFn: () => settingsClient.getSettings({}),
  });
  const [address, setAddress] = useState<string | null>(null);
  const [domain, setDomain] = useState<string | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  if (q.isPending) return null;
  if (q.error) return <Alert>{errorMessage(q.error)}</Alert>;
  const s = q.data.settings;
  if (!s) return null;

  const save = async (what: "address" | "domain") => {
    setBusy(what);
    setError("");
    try {
      if (what === "address") {
        await settingsClient.updateSettings({
          publicAddress: (address ?? "").trim(),
        });
        setAddress(null);
      } else {
        await settingsClient.setPanelDomain({ domain: (domain ?? "").trim() });
        setDomain(null);
      }
      queryClient.invalidateQueries({ queryKey: ["settings"] });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy("");
    }
  };

  return (
    <SettingRows>
      {error && (
        <div className="pb-5">
          <Alert>{error}</Alert>
        </div>
      )}
      <SettingRow
        label={t.access.address.label}
        htmlFor="panel-address"
        description={t.access.address.description}
        help={t.access.address.help}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="panel-address"
            className="w-64 font-mono"
            value={address ?? s.publicAddress}
            onChange={(e) => setAddress(e.target.value)}
          />
          <span className="font-mono text-[13px] text-muted-foreground">
            :{s.port}
          </span>
          {address !== null && address.trim() !== s.publicAddress && (
            <Button
              size="sm"
              onClick={() => save("address")}
              disabled={busy !== ""}
            >
              {busy === "address" && <Spinner />}
              {t.common.actions.save}
            </Button>
          )}
        </div>
      </SettingRow>
      <SettingRow
        label={t.access.domain.label}
        htmlFor="panel-domain"
        description={
          s.domain
            ? t.access.domain.bound(s.domain)
            : t.access.domain.unbound
        }
        help={t.access.domain.help}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="panel-domain"
            className="w-64 font-mono"
            placeholder="panel.example.com"
            value={domain ?? s.domain}
            onChange={(e) => setDomain(e.target.value)}
          />
          {domain !== null && domain.trim() !== s.domain && (
            <Button
              size="sm"
              onClick={() => save("domain")}
              disabled={busy !== ""}
            >
              {busy === "domain" && <Spinner />}
              {domain.trim() ? t.access.domain.bind : t.access.domain.unbind}
            </Button>
          )}
        </div>
      </SettingRow>
      <SettingRow
        label={t.access.fingerprint.label}
        description={t.access.fingerprint.description}
      >
        <div className="flex items-center gap-1">
          <code className="truncate font-mono text-[13px]">
            {s.fingerprint}
          </code>
          <CopyButton text={s.fingerprint} />
        </div>
      </SettingRow>
    </SettingRows>
  );
}
