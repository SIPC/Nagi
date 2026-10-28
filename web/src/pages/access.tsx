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
        label="对外地址"
        htmlFor="panel-address"
        description="节点通过「对外地址:面板端口」连接面板，安装命令同样使用该地址。"
        help="须可从所有节点访问。修改后，在线节点接收新地址并在下次重连时使用；修改时离线的节点须在节点上执行 nagi panel add 更新。"
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
              保存
            </Button>
          )}
        </div>
      </SettingRow>
      <SettingRow
        label="绑定域名"
        htmlFor="panel-domain"
        description={
          s.domain
            ? `通过 https://${s.domain} 访问面板。`
            : "留空表示不绑定，通过 https://对外地址:端口 访问。"
        }
        help="域名须预先解析到本机。绑定后由本机 Caddy 经 80/443 端口反向代理面板并自动申请证书；节点仍通过对外地址和面板端口连接。"
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
              {domain.trim() ? "绑定" : "解除绑定"}
            </Button>
          )}
        </div>
      </SettingRow>
      <SettingRow
        label="公钥指纹"
        description="节点和备用面板连接时核对此指纹，以确认连接的是本面板。"
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
