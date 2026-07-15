import { useState, type FormEvent } from "react";
import { Navigate } from "react-router-dom";
import { AdminApiError } from "@/api/client";
import { useAuth, useIsAuthenticated } from "@/auth/AuthContext";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ThemeToggle } from "@/components/ThemeToggle";

const bootstrapCommand = 'printf \'%s\\n\' "$ADMIN_PASSWORD" | docker compose run --rm -T app bootstrap-admin --password-stdin --config /app/config.json';

function loginErrorMessage(error: unknown): string {
  if (!(error instanceof AdminApiError)) return "登录失败，请检查服务连接后重试";
  switch (error.code) {
    case "invalid_credentials":
      return "管理员密码不正确";
    case "login_rate_limited": {
      const seconds = Number(error.retryAfter);
      return Number.isFinite(seconds) && seconds > 0
        ? `登录尝试过多，请在 ${Math.ceil(seconds)} 秒后重试`
        : "登录尝试过多，请稍后重试";
    }
    case "setup_required":
      return "管理员账号尚未初始化，请先运行 bootstrap-admin";
    case "unauthorized":
      return "登录会话无效，请重新输入管理员密码";
    default:
      return error.message || "登录失败，请稍后重试";
  }
}

export function LoginPage() {
  const { ready, meta, error: metaError, login, refreshMeta } = useAuth();
  const authed = useIsAuthenticated();
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(false);
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
      setError(loginErrorMessage(err));
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
            ) : metaError && !meta ? (
              <div className="space-y-3 rounded-lg border border-destructive/20 bg-destructive/8 p-3">
                <p className="text-xs leading-5 text-destructive" role="alert">
                  无法加载服务状态：{metaError}
                </p>
                <Button type="button" variant="outline" size="sm" onClick={() => void refreshMeta()}>
                  重试
                </Button>
              </div>
            ) : meta && !meta.auth_required ? (
              <p className="text-sm text-muted-foreground" role="status">当前实例未启用管理员认证，将直接进入。</p>
            ) : meta?.setup_required ? (
              <div className="space-y-3" role="status">
                <div className="rounded-md border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-xs leading-5 text-amber-700 dark:text-amber-300">
                  管理员账号尚未初始化。请在服务端设置 <code className="font-mono">ADMIN_PASSWORD</code> 后运行 bootstrap-admin：
                </div>
                <code className="block overflow-x-auto rounded-md bg-secondary/60 p-3 font-mono text-[11px] leading-5 text-foreground">
                  {bootstrapCommand}
                </code>
                <Button type="button" variant="outline" size="sm" onClick={() => void refreshMeta()}>
                  初始化完成后重试
                </Button>
              </div>
            ) : (
              <form className="space-y-4" onSubmit={onSubmit}>
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
