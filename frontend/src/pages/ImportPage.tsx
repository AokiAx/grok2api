import { useMemo, useState, type ChangeEvent } from "react";
import { FileJson, RotateCcw, Upload } from "lucide-react";
import {
  adminApi,
  AdminApiError,
  type ImportAccount,
  type ImportResult,
} from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  formatImportResult,
  normalizeImportAccounts,
  summarizeAccounts,
} from "@/lib/importNormalize";

export function ImportPage() {
  const [raw, setRaw] = useState("");
  const [fileMeta, setFileMeta] = useState("数组 / {accounts:[]} / auth.json map");
  const [output, setOutput] = useState("等待操作");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<"preview" | "import" | null>(null);
  const [last, setLast] = useState<ImportResult | null>(null);

  const parsed = useMemo(() => {
    if (!raw.trim()) return null;
    try {
      const accounts = normalizeImportAccounts(raw);
      return { accounts, summary: summarizeAccounts(accounts), error: null as string | null };
    } catch (e) {
      return {
        accounts: [] as ImportAccount[],
        summary: null,
        error: e instanceof Error ? e.message : String(e),
      };
    }
  }, [raw]);

  async function onFile(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    if (!file) return;
    try {
      const accounts = normalizeImportAccounts(await file.text());
      setRaw(JSON.stringify({ accounts }, null, 2));
      const s = summarizeAccounts(accounts);
      setFileMeta(`${file.name} · ${s.total} 条`);
      setError(null);
      setLast(null);
      setOutput(`已解析 ${s.total} 条 · refresh ${s.withRefresh}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function run(preview: boolean) {
    setBusy(preview ? "preview" : "import");
    setError(null);
    try {
      const accounts = normalizeImportAccounts(raw);
      const result = preview
        ? await adminApi.importPreview(accounts)
        : await adminApi.importCommit(accounts);
      setLast(result);
      setOutput(formatImportResult(result, preview));
    } catch (err) {
      setError(
        err instanceof AdminApiError
          ? err.message
          : err instanceof Error
            ? err.message
            : String(err),
      );
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-8">
      <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-end">
        <div>
          <h1 className="text-xl font-medium tracking-tight">导入账号</h1>
          <p className="mt-1.5 text-xs text-muted-foreground">校验凭证内容，预览变更后再写入账号池。</p>
        </div>
        <Badge>{fileMeta}</Badge>
      </div>
      <Card className="p-4 sm:p-5">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex items-center gap-3">
            <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-secondary"><FileJson className="h-4 w-4" /></div>
            <div><CardTitle>凭证文件</CardTitle><CardDescription className="mt-1">支持数组、accounts 对象及 auth.json map</CardDescription></div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <label className="inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-full border border-input bg-background px-3 text-xs font-medium hover:bg-secondary">
              <Upload className="h-3.5 w-3.5" />选择文件
              <input type="file" accept=".json,.txt,application/json" className="sr-only" onChange={(e) => void onFile(e)} />
            </label>
            <Button
              variant="outline"
              disabled={!!busy || !raw.trim()}
              onClick={() => void run(true)}
            >
              {busy === "preview" ? "预览中…" : "预览"}
            </Button>
            <Button
              size="sm"
              disabled={!!busy || !raw.trim()}
              onClick={() => {
                if (window.confirm("确认导入？")) void run(false);
              }}
            >
              {busy === "import" ? "导入中…" : "导入"}
            </Button>
            <Button variant="ghost"
              onClick={() => {
                setRaw("");
                setOutput("等待操作");
                setLast(null);
                setError(null);
              }}
            >
              <RotateCcw className="h-3.5 w-3.5" />清空
            </Button>
          </div>
        </div>
          {parsed?.summary ? <p className="mt-4 text-xs text-muted-foreground">已解析 {parsed.summary.total} 条，其中 {parsed.summary.withRefresh} 条包含 refresh token</p> : null}
          {parsed?.error ? <p className="text-sm text-destructive">{parsed.error}</p> : null}
          {error ? <p className="text-sm text-destructive">{error}</p> : null}
      </Card>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>输入</CardTitle>
          </CardHeader>
          <CardContent className="px-0 pb-0">
            <textarea
              className="mono min-h-[420px] w-full resize-y rounded-md border border-input bg-secondary/40 p-3 text-xs leading-5 outline-none focus:bg-background focus:ring-1 focus:ring-ring"
              value={raw}
              onChange={(e) => setRaw(e.target.value)}
              spellCheck={false}
            />
          </CardContent>
        </Card>
        <Card className="p-4 sm:p-5">
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <CardTitle>结果</CardTitle>
            {last ? (
              <div className="flex gap-1">
                <Badge tone="success">+{last.added}</Badge>
                <Badge tone="warning">~{last.updated}</Badge>
                <Badge tone={last.invalid ? "danger" : "success"}>!{last.invalid}</Badge>
              </div>
            ) : null}
          </CardHeader>
          <CardContent className="px-0 pb-0">
            <pre className="mono max-h-[462px] min-h-[420px] overflow-auto whitespace-pre-wrap break-words rounded-md bg-secondary/55 p-3 text-xs leading-5">
              {output}
            </pre>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
