import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import { toast } from "sonner";
import type { ReportConfig } from "@/gen/nagi/v1/model_report_pb";
import { modelReportClient } from "@/lib/api";
import { Alert, Button, Check, Dialog, DialogActions, EmptyRow, Field, FormRow, Input, List, Notice, QueryError, Row, RowsSkeleton, Section, Spinner } from "@/components/ui";
import { locale, t } from "@/i18n";

const m = t.alerts.reports;
const date = (value: Timestamp | undefined) => (value ? timestampDate(value).toLocaleString(locale) : "—");
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
    <Dialog open onClose={onClose} title={m.configure} className="max-w-[560px]">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <Check checked={enabled} onChange={setEnabled}>
          {m.enable}
        </Check>
        <Notice>{m.privacy}</Notice>
        <Field label={m.endpoint} htmlFor="report-endpoint">
          <Input id="report-endpoint" type="url" value={endpoint} onChange={(e) => setEndpoint(e.target.value)} required={enabled} maxLength={500} autoComplete="off" placeholder="https://…/v1/chat/completions" />
        </Field>
        <FormRow>
          <Field label={m.model} htmlFor="report-model">
            <Input id="report-model" value={model} onChange={(e) => setModel(e.target.value)} required={enabled} maxLength={120} autoComplete="off" />
          </Field>
          <Field label={m.key} htmlFor="report-key">
            <Input id="report-key" type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} maxLength={4096} autoComplete="new-password" spellCheck={false} placeholder={config?.hasApiKey ? m.keyStored : m.keyEmpty} />
          </Field>
        </FormRow>
        <p className="text-sm text-muted-foreground">{m.keyHelp}</p>
        <Check checked={clearApiKey} onChange={setClear}>
          {m.clearKey}
        </Check>
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
      </form>
    </Dialog>
  );
}

function ReportDetail({ id, onClose }: { id: string; onClose: () => void }) {
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
        <div className="grid min-w-0 gap-4">
          <Notice>{m.advice}</Notice>
          <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm">
            <dt>{m.state}</dt>
            <dd>{status(r.status)}</dd>
            <dt>{m.started}</dt>
            <dd>{date(r.started)}</dd>
            <dt>{m.finished}</dt>
            <dd>{date(r.finished)}</dd>
            <dt>{m.model}</dt>
            <dd className="break-all">{r.model}</dd>
            <dt>{m.template}</dt>
            <dd className="break-all">{r.templateVersion}</dd>
            <dt>{m.actor}</dt>
            <dd>{String(r.actor)}</dd>
            <dt>{m.trigger}</dt>
            <dd>{r.trigger === "manual" ? m.manual : m.statuses.unknown}</dd>
          </dl>
          {r.failure && <Alert>{failure(r.failure)}</Alert>}
          {r.content ? <pre className="whitespace-pre-wrap break-words font-sans text-sm leading-relaxed">{r.content}</pre> : <p className="text-sm text-muted-foreground">{m.noContent}</p>}
          <details>
            <summary className="cursor-pointer text-sm">{m.material}</summary>
            <p className="my-2 break-all font-mono text-xs">SHA-256: {r.digest}</p>
            <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all text-xs">{r.materialJson}</pre>
          </details>
        </div>
      )}
    </Dialog>
  );
}

export function ModelReports() {
  const client = useQueryClient();
  const [configure, setConfigure] = useState(false);
  const [selected, setSelected] = useState("");
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
      <Section
        title={m.title}
        description={m.description}
        actions={
          <Button variant="outline" onClick={() => setConfigure(true)} disabled={cfg.isPending || generate.isPending}>
            {m.configure}
          </Button>
        }
      >
        <Notice title={cfg.data?.enabled ? m.enabled : m.disabled}>{m.policy}</Notice>
        {cfg.error && <Alert>{error(cfg.error)}</Alert>}
        {generate.error && <Alert>{error(generate.error)}</Alert>}
        <div className="flex flex-wrap items-center gap-3">
          <Button disabled={!cfg.data?.enabled || generate.isPending} onClick={() => generate.mutate()}>
            {generate.isPending && <Spinner />}
            {generate.isPending ? m.generating : m.generate}
          </Button>
          <p className="text-sm text-muted-foreground">{m.retention}</p>
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
                  title={date(r.started)}
                  sub={`${status(r.status)} · ${r.model}`}
                  actions={
                    <Button readOnlyOk variant="ghost" onClick={() => setSelected(r.id)}>
                      {m.view}
                    </Button>
                  }
                />
              ))
            ) : (
              <EmptyRow>{m.empty}</EmptyRow>
            )}
          </List>
        )}
      </Section>
      {configure && <ReportConfigDialog config={cfg.data} onClose={() => setConfigure(false)} />}
      {selected && <ReportDetail id={selected} onClose={() => setSelected("")} />}
    </>
  );
}
