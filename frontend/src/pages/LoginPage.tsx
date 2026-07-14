import { useState, type FormEvent } from "react";
import { Navigate } from "react-router-dom";
import { AdminApiError } from "@/api/client";
import { useAuth, useIsAuthenticated } from "@/auth/AuthContext";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ThemeToggle } from "@/components/ThemeToggle";

export function LoginPage() {
  const { ready, meta, login } = useAuth();
  const authed = useIsAuthenticated();
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (ready && authed) return <Navigate to="/" replace />;

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(password, remember);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "登录失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen flex-col bg-background">
      <header className="mx-auto flex h-16 w-full max-w-[960px] items-center justify-between px-5 sm:px-8 lg:px-0">
        <span className="text-sm font-semibold text-foreground">grok2api</span>
        <div className="flex items-center gap-1">
          <span className="font-mono text-[10px] text-muted-foreground">{meta?.version || "dev"}</span>
          <ThemeToggle />
        </div>
      </header>

      <main className="mx-auto flex w-full max-w-[960px] flex-1 items-center justify-center px-5 py-12 sm:px-8 lg:px-0">
        <div className="grid w-full max-w-[840px] -translate-y-6 items-center lg:-translate-y-10 lg:grid-cols-[minmax(0,1fr)_1px_336px] lg:gap-14">
          <section className="hidden min-h-72 flex-col justify-center lg:flex">
            <p className="text-xs font-medium text-muted-foreground">grok2api</p>
            <h2 className="mt-3 max-w-sm text-3xl leading-tight font-medium text-balance">
              CLI 号池管理台
            </h2>
            <p className="mt-4 max-w-xs text-xs leading-6 text-muted-foreground text-pretty">
              集中管理账号凭证、运行状态与号池容量，让日常维护保持清晰、高效。
            </p>
          </section>

          <div className="hidden h-64 bg-border lg:block" aria-hidden="true" />

          <section className="w-full max-w-[336px] justify-self-center lg:justify-self-auto">
            <div className="mb-6">
              <h1 className="text-xl font-medium">登录</h1>
              <p className="mt-2 text-xs leading-5 text-muted-foreground text-pretty lg:hidden">
                使用管理员密码进入面板
              </p>
            </div>

            {!ready ? (
              <p className="text-sm text-muted-foreground" role="status">加载中…</p>
            ) : meta && !meta.auth_required ? (
              <p className="text-sm text-muted-foreground" role="status">当前实例未启用管理员认证，将直接进入。</p>
            ) : (
              <form className="space-y-4" onSubmit={onSubmit}>
                {meta?.setup_required ? (
                  <p className="rounded-md border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-xs leading-5 text-amber-700 dark:text-amber-300" role="status">
                    管理员账号尚未初始化，请先完成服务端初始化配置。
                  </p>
                ) : null}
                <div className="space-y-2">
                  <Label htmlFor="password">管理员密码</Label>
                  <Input
                    id="password"
                    className="bg-secondary/55"
                    type="password"
                    autoComplete="current-password"
                    autoFocus
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    placeholder="输入管理员密码"
                    required
                  />
                </div>
                <label className="flex items-center gap-2 text-xs text-muted-foreground">
                  <input
                    type="checkbox"
                    className="h-3.5 w-3.5 rounded border-input"
                    checked={remember}
                    onChange={(e) => setRemember(e.target.checked)}
                  />
                  记住登录
                </label>
                {error ? <p className="text-sm text-destructive" role="alert">{error}</p> : null}
                <Button type="submit" size="sm" className="w-full active:scale-[0.98]" disabled={busy}>
                  {busy ? "验证中…" : "进入面板"}
                </Button>
              </form>
            )}
          </section>
        </div>
      </main>
      <footer className="flex h-10 shrink-0 items-center justify-end gap-1.5 whitespace-nowrap px-5 text-[11px] text-muted-foreground sm:px-8 lg:mx-auto lg:w-full lg:max-w-[960px] lg:px-0">
        <a className="transition-colors hover:text-foreground" href="https://github.com/AokiAx/grok2api" target="_blank" rel="noreferrer">grok2api</a>
        <span>© 2026</span>
      </footer>
    </div>
  );
}
