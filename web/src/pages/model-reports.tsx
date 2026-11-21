import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import { ChevronRight, FileText } from "lucide-react";
import { toast } from "sonner";
import type { ReportConfig } from "@/gen/nagi/v1/model_report_pb";
import { modelReportClient } from "@/lib/api";
import {
  Alert,
  Button,
  Check,
  Collapse,
  CopyButton,
  Dialog,
  DialogActions,
  EmptyRow,
  Field,
  FormBody,
  FormRow,
  Input,
  KeyValue,
  List,
  Notice,
  QueryError,
  Row,
  RowsSkeleton,
  Section,
  SettingRow,
  SettingRows,
  Spinner,
  Status,
} from "@/components/ui";
import { cn, formatDateTime } from "@/lib/utils";
import { locale, t } from "@/i18n";

const m = t.alerts.reports;
const date = (value: Timestamp | undefined) => formatDateTime(value ? timestampDate(value) : undefined) ?? "—";
const status = (value: string) => (m.statuses as Record<string, string>)[value] ?? m.statuses.unknown;
const failure = (value: string) => (m.errors as Record<string, string>)[value] ?? (/^http_\d{3}$/.test(value) ? m.httpError(value.slice(5)) : m.errors.unknown);
const error = (err: unknown) => failure(err instanceof ConnectError ? err.rawMessage : "unknown");

function ReportConfigDialog({ config, onClose }: { config?: ReportConfig; onClose: () => void }) {
  const client = useQueryClient();
  const [enabled, setEnabled] = useState(config?.enabled ?? false);
  const [endpoint, setEndpoint] = useState(config?.endpoint ?? "");
  const [model, setModel] = useState(config?.model ?? "");
  const [apiKey, setApiKey] = useState("");
  const [clearApiKey, setClear] = useState(!config);
  const save = useMutation({
    mutationFn: () =>
      modelReportClient.updateReportConfig({
        enabled,
        endpoint,
        model,
        apiKey,
        clearApiKey,
      }),
    onSuccess: (cfg) => {
      setApiKey("");
      client.setQueryData(["model-report-config"], cfg);
      toast.success(m.saved);
      onClose();
    },
    // Do not retain a submitted credential in mutation variables/cache.
    onSettled: () => setApiKey(""),
  });
  return (
    <Dialog open onClose={onClose} title={m.configure} description={m.configDescription} className="max-w-[560px]">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <FormBody>
          <Check checked={enabled} onChange={setEnabled}>
            {m.enable}
          </Check>
          <Field label={m.endpoint} htmlFor="report-endpoint">
            <Input id="report-endpoint" type="url" value={endpoint} onChange={(e) => setEndpoint(e.target.value)} required={enabled} maxLength={500} autoComplete="off" placeholder="https://…/v1/chat/completions" />
          </Field>
          <FormRow>
            <Field label={m.model} htmlFor="report-model">
              <Input id="report-model" value={model} onChange={(e) => setModel(e.target.value)} required={enabled} maxLength={120} autoComplete="off" />
            </Field>
            <Field label={m.key} htmlFor="report-key" help={m.keyHelp}>
              <Input id="report-key" type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} maxLength={4096} autoComplete="new-password" spellCheck={false} placeholder={config?.hasApiKey ? m.keyStored : m.keyEmpty} />
            </Field>
          </FormRow>
          {config?.hasApiKey && (
            <Check checked={clearApiKey} onChange={setClear}>
              {m.clearKey}
            </Check>
          )}
          <Notice>{m.privacy}</Notice>
          {save.error && <Alert>{error(save.error)}</Alert>}
          <DialogActions>
            <Button variant="outline" onClick={onClose}>
              {t.common.actions.cancel}
            </Button>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending && <Spinner />}
              {t.common.actions.save}
            </Button>
          </DialogActions>
        </FormBody>
      </form>
    </Dialog>
  );
}

function ReportDetail({ id, onClose }: { id: string; onClose: () => void }) {
  const [recordOpen, setRecordOpen] = useState(false);
  const [materialOpen, setMaterialOpen] = useState(false);
  const q = useQuery({
    queryKey: ["model-report", id],
    queryFn: () => modelReportClient.getReport({ id }),
    refetchInterval: (query) => (query.state.data?.status === "running" ? 5000 : false),
  });
  const r = q.data;
  return (
    <Dialog open onClose={onClose} title={m.detail} className="max-w-[760px]">
      {q.isPending ? (
        <RowsSkeleton />
      ) : !r ? (
        <QueryError error={error(q.error)} onRetry={() => q.refetch()} />
      ) : (
        <div className="grid min-w-0 gap-6">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <Status tone={r.status === "failed" || r.status === "timeout" ? "warning" : "neutral"}>{status(r.status)}</Status>
            <span className="text-[13px] text-muted-foreground tabular-nums">{date(r.started)}</span>
            <span className="min-w-0 break-all text-[13px] text-muted-foreground">{r.model}</span>
          </div>
          <Notice>{m.advice}</Notice>
          {r.failure && <Alert>{failure(r.failure)}</Alert>}
          <div className="grid gap-3">
            <div className="flex items-center justify-between gap-4">
              <h3 className="text-sm font-medium">{m.content}</h3>
              {r.content && <CopyButton text={r.content} />}
            </div>
            {r.content ? <pre className="whitespace-pre-wrap wrap-anywhere font-sans text-sm leading-relaxed">{r.content}</pre> : <p className="text-sm text-muted-foreground">{m.noContent}</p>}
          </div>
          <div className="grid gap-4">
            <Button readOnlyOk variant="ghost" className="-ml-3 w-fit" aria-expanded={recordOpen} aria-controls="report-record" onClick={() => setRecordOpen(!recordOpen)}>
              <ChevronRight data-icon="inline-start" className={cn("transition-transform duration-240 motion-reduce:transition-none", recordOpen && "rotate-90")} />
              {m.record}
            </Button>
            <Collapse open={recordOpen} gap={4}>
              <div id="report-record">
                <KeyValue
                  items={[
                    [m.started, <span className="tabular-nums">{date(r.started)}</span>],
                    [m.finished, <span className="tabular-nums">{date(r.finished)}</span>],
                    [m.template, <span className="font-mono text-[13px]">{r.templateVersion}</span>],
                    [m.actor, String(r.actor)],
                    [m.trigger, r.trigger === "manual" ? m.manual : m.statuses.unknown],
                  ]}
                />
              </div>
            </Collapse>
          </div>
          <div className="grid gap-4">
            <Button
              readOnlyOk
              variant="ghost"
              className="-ml-3 h-auto min-h-8 w-fit py-1 text-left whitespace-normal max-md:h-auto max-md:min-h-10"
              aria-expanded={materialOpen}
              aria-controls="report-material"
              onClick={() => setMaterialOpen(!materialOpen)}
            >
              <ChevronRight data-icon="inline-start" className={cn("transition-transform duration-240 motion-reduce:transition-none", materialOpen && "rotate-90")} />
              {m.material}
            </Button>
            <Collapse open={materialOpen} gap={4}>
              <div id="report-material" className="grid gap-3">
                <p className="break-all font-mono text-xs text-muted-foreground">SHA-256: {r.digest}</p>
                <pre className="max-h-72 overflow-auto rounded-lg bg-accent p-4 whitespace-pre-wrap break-all font-mono text-[13px] leading-relaxed">{r.materialJson}</pre>
              </div>
            </Collapse>
          </div>
        </div>
      )}
    </Dialog>
  );
}

export function ModelReports() {
  const client = useQueryClient();
  const [configure, setConfigure] = useState(false);
  const [selected, setSelected] = useState("");
  const [policyOpen, setPolicyOpen] = useState(false);
  const cfg = useQuery({
    queryKey: ["model-report-config"],
    queryFn: () => modelReportClient.getReportConfig({}),
  });
  const reports = useQuery({
    queryKey: ["model-reports"],
    queryFn: () => modelReportClient.listReports({}),
    refetchInterval: (q) => (q.state.data?.reports.some((r) => r.status === "running") ? 5000 : false),
  });
  const generate = useMutation({
    mutationFn: () =>
      modelReportClient.generateReport({
        language: locale === "en" ? "en" : "zh",
      }),
    onSuccess: (r) => {
      client.setQueryData(["model-report", r.id], r);
      setSelected(r.id);
    },
    onSettled: () => client.invalidateQueries({ queryKey: ["model-reports"] }),
  });
  return (
    <>
      <Section title={m.title} help={m.description}>
        {cfg.error && <QueryError error={error(cfg.error)} onRetry={() => cfg.refetch()} />}
        {generate.error && <Alert>{error(generate.error)}</Alert>}
        {cfg.isPending ? (
          <RowsSkeleton count={1} />
        ) : (
          <div className="grid gap-4">
            <SettingRows>
              <SettingRow label={cfg.error ? m.statuses.unknown : cfg.data?.enabled ? m.enabled : m.disabled} description={cfg.data?.enabled ? cfg.data.model : m.setupHelp}>
                <Button variant="outline" onClick={() => setConfigure(true)} disabled={generate.isPending}>
                  {m.configureAction}
                </Button>
              </SettingRow>
            </SettingRows>
            {cfg.data?.enabled && (
              <Notice
                actions={
                  <Button disabled={generate.isPending} onClick={() => generate.mutate()}>
                    {generate.isPending ? <Spinner /> : <FileText data-icon="inline-start" />}
                    {generate.isPending ? m.generating : m.generate}
                  </Button>
                }
              >
                {m.readyHelp}
              </Notice>
            )}
          </div>
        )}
        <div className="grid gap-4">
          <Button readOnlyOk variant="ghost" className="-ml-3 w-fit" aria-expanded={policyOpen} aria-controls="report-policy" onClick={() => setPolicyOpen(!policyOpen)}>
            <ChevronRight data-icon="inline-start" className={cn("transition-transform duration-240 motion-reduce:transition-none", policyOpen && "rotate-90")} />
            {m.policyTitle}
          </Button>
          <Collapse open={policyOpen} gap={4}>
            <div id="report-policy" className="grid gap-3 text-sm leading-relaxed text-muted-foreground">
              <p>{m.policy}</p>
              <p>{m.privacy}</p>
            </div>
          </Collapse>
        </div>
        <div className="mt-2 grid gap-2">
          <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
            <h3 className="text-sm font-medium">{m.history}</h3>
            <p className="text-[13px] text-muted-foreground">{m.retention}</p>
          </div>
          {reports.isPending ? (
            <RowsSkeleton />
          ) : reports.error ? (
            <QueryError error={error(reports.error)} onRetry={() => reports.refetch()} />
          ) : (
            <List>
              {reports.data?.reports.length ? (
                reports.data.reports.map((r) => (
                  <Row
                    key={r.id}
                    title={<span className="tabular-nums">{date(r.started)}</span>}
                    sub={r.model}
                    cols={[{ content: status(r.status), width: "w-20" }]}
                    actions={
                      <Button readOnlyOk size="row" variant="ghost" aria-label={`${m.view} · ${date(r.started)}`} onClick={() => setSelected(r.id)}>
                        {m.view}
                        <ChevronRight data-icon="inline-end" />
                      </Button>
                    }
                  />
                ))
              ) : (
                <EmptyRow>{cfg.data?.enabled ? m.emptyReady : m.emptySetup}</EmptyRow>
              )}
            </List>
          )}
        </div>
      </Section>
      {configure && <ReportConfigDialog config={cfg.data} onClose={() => setConfigure(false)} />}
      {selected && <ReportDetail id={selected} onClose={() => setSelected("")} />}
    </>
  );
}
