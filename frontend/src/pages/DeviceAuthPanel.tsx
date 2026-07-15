import { useEffect, useRef, useState } from "react";
import { adminApi, AdminApiError, type DeviceAuthSession } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

const terminal = new Set(["succeeded", "denied", "expired", "cancelled", "failed"]);

export function DeviceAuthPanel() {
  const [issuer, setIssuer] = useState("https://auth.x.ai");
  const [clientId, setClientId] = useState("grok-cli");
  const [scope, setScope] = useState("openid profile email offline_access");
  const [session, setSession] = useState<DeviceAuthSession | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const timer = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (timer.current) window.clearInterval(timer.current);
    };
  }, []);

  useEffect(() => {
    if (!session || terminal.has(session.status)) {
      if (timer.current) {
        window.clearInterval(timer.current);
        timer.current = null;
      }
      return;
    }
    const intervalMs = Math.max(2, session.interval_sec || 5) * 1000;
    if (timer.current) window.clearInterval(timer.current);
    timer.current = window.setInterval(() => {
      void poll(session.id);
    }, intervalMs);
    return () => {
      if (timer.current) window.clearInterval(timer.current);
    };
  }, [session?.id, session?.status, session?.interval_sec]);

  async function start() {
    setBusy(true);
    setError(null);
    try {
      const next = await adminApi.startDeviceAuth({
        issuer: issuer.trim() || undefined,
        client_id: clientId.trim() || undefined,
        scope: scope.trim() || undefined,
      });
      setSession(next);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "启动失败");
    } finally {
      setBusy(false);
    }
  }

  async function poll(id: string) {
    try {
      const next = await adminApi.pollDeviceAuth(id);
      setSession(next);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "轮询失败");
    }
  }

  async function cancel() {
    if (!session) return;
    setBusy(true);
    try {
      const next = await adminApi.cancelDeviceAuth(session.id);
      setSession(next);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "取消失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="p-4 sm:p-5">
      <CardHeader>
        <CardTitle>Build Device OAuth</CardTitle>
        <CardDescription>
          在浏览器打开 verification URL 并输入 user code。device_code 与 token 不会返回前端。
        </CardDescription>
      </CardHeader>
      <CardContent className="mt-3 space-y-4">
        {error ? <p className="text-sm text-destructive">{error}</p> : null}
        <div className="grid gap-3 sm:grid-cols-3">
          <div className="space-y-1.5">
            <Label className="text-xs text-muted-foreground">Issuer</Label>
            <Input value={issuer} onChange={(e) => setIssuer(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs text-muted-foreground">Client ID</Label>
            <Input value={clientId} onChange={(e) => setClientId(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs text-muted-foreground">Scope</Label>
            <Input value={scope} onChange={(e) => setScope(e.target.value)} />
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button onClick={() => void start()} disabled={busy}>
            {busy ? "启动中…" : "开始授权"}
          </Button>
          {session && !terminal.has(session.status) ? (
            <>
              <Button variant="outline" onClick={() => void poll(session.id)} disabled={busy}>
                立即轮询
              </Button>
              <Button variant="outline" onClick={() => void cancel()} disabled={busy}>
                取消
              </Button>
            </>
          ) : null}
        </div>

        {session ? (
          <div className="rounded-lg border border-border/70 bg-background/60 p-4 text-sm space-y-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-muted-foreground">状态</span>
              <Badge tone={session.status === "succeeded" ? "success" : session.status === "pending" || session.status === "slow_down" ? "default" : "danger"}>
                {session.status}
              </Badge>
              <span className="font-mono text-[11px] text-muted-foreground">{session.id}</span>
            </div>
            <div className="grid gap-1 sm:grid-cols-[120px_1fr] text-xs">
              <span className="text-muted-foreground">User code</span>
              <span className="font-mono text-base font-semibold tracking-wider">{session.user_code}</span>
              <span className="text-muted-foreground">Verification</span>
              <a
                className="truncate text-foreground underline-offset-2 hover:underline"
                href={session.verification_uri_complete || session.verification_uri}
                target="_blank"
                rel="noreferrer"
              >
                {session.verification_uri_complete || session.verification_uri}
              </a>
              {session.account_id ? (
                <>
                  <span className="text-muted-foreground">账号</span>
                  <span className="font-mono">{session.account_id}</span>
                </>
              ) : null}
              {session.last_error ? (
                <>
                  <span className="text-muted-foreground">错误</span>
                  <span className="text-destructive">{session.last_error}</span>
                </>
              ) : null}
            </div>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
