import { useEffect, useRef, useState } from "react";
import { adminApi, AdminApiError, type DeviceAuthSession } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

const terminal = new Set(["succeeded", "denied", "expired", "cancelled", "failed"]);

const FALLBACK = {
  issuer: "https://auth.x.ai",
  client_id: "b1a00492-073a-47ea-816f-4c329264a828",
  scope:
    "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write",
};

export function DeviceAuthPanel() {
  const [config, setConfig] = useState(FALLBACK);
  const [configReady, setConfigReady] = useState(false);
  const [session, setSession] = useState<DeviceAuthSession | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const timer = useRef<number | null>(null);

  useEffect(() => {
    let active = true;
    void adminApi
      .settings()
      .then((settings) => {
        if (!active) return;
        setConfig({
          issuer: settings.device_auth?.issuer?.trim() || FALLBACK.issuer,
          client_id: settings.device_auth?.client_id?.trim() || FALLBACK.client_id,
          scope: settings.device_auth?.scope?.trim() || FALLBACK.scope,
        });
      })
      .catch(() => {
        /* keep product fallbacks */
      })
      .finally(() => {
        if (active) setConfigReady(true);
      });
    return () => {
      active = false;
    };
  }, []);

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
      // Prefer latest settings so operators don't need to reopen the panel after save.
      let nextConfig = config;
      try {
        const settings = await adminApi.settings();
        nextConfig = {
          issuer: settings.device_auth?.issuer?.trim() || FALLBACK.issuer,
          client_id: settings.device_auth?.client_id?.trim() || FALLBACK.client_id,
          scope: settings.device_auth?.scope?.trim() || FALLBACK.scope,
        };
        setConfig(nextConfig);
      } catch {
        /* use cached/fallback config */
      }
      const next = await adminApi.startDeviceAuth({
        issuer: nextConfig.issuer || undefined,
        client_id: nextConfig.client_id || undefined,
        scope: nextConfig.scope || undefined,
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
          使用设置中的 OIDC 参数发起 Device Flow。device_code 与 token 只在服务端；Issuer / Client ID / Scope 请到「设置」修改。
        </CardDescription>
      </CardHeader>
      <CardContent className="mt-3 space-y-4">
        {error ? <p className="text-sm text-destructive">{error}</p> : null}
        <div className="rounded-lg border border-border/70 bg-background/60 p-3 text-xs">
          <div className="grid gap-1 sm:grid-cols-[88px_1fr]">
            <span className="text-muted-foreground">Issuer</span>
            <span className="mono break-all">{config.issuer}</span>
            <span className="text-muted-foreground">Client ID</span>
            <span className="mono break-all">{config.client_id}</span>
            <span className="text-muted-foreground">Scope</span>
            <span className="break-all text-muted-foreground">{config.scope}</span>
          </div>
          {!configReady ? (
            <p className="mt-2 text-[11px] text-muted-foreground">正在读取设置…</p>
          ) : null}
        </div>
        <div className="flex flex-wrap gap-2">
          <Button onClick={() => void start()} disabled={busy || !configReady}>
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
              <div className="min-w-0 space-y-1">
                <a
                  className="block break-all text-foreground underline-offset-2 hover:underline"
                  href={session.verification_uri_complete || session.verification_uri}
                  target="_blank"
                  rel="noreferrer"
                >
                  {session.verification_uri_complete || session.verification_uri}
                </a>
                <a
                  className="inline-flex text-[11px] text-muted-foreground underline-offset-2 hover:underline"
                  href={session.verification_uri_complete || session.verification_uri}
                  target="_blank"
                  rel="noreferrer"
                >
                  在新标签打开授权页
                </a>
              </div>
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
