import { Boxes, Github, KeyRound, LayoutDashboard, LogOut, Menu, Settings2, SlidersHorizontal, Upload, Users, X } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { NavLink, Outlet } from "react-router-dom";
import { useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/button";
import { ThemeToggle } from "@/components/ThemeToggle";
import { cn } from "@/lib/cn";

const navigation = [
  { to: "/", label: "总览", icon: LayoutDashboard, end: true },
  { to: "/accounts", label: "账号", icon: Users, end: false },
  { to: "/client-keys", label: "客户端密钥", icon: KeyRound, end: false },
  { to: "/import", label: "导入", icon: Upload, end: false },
  { to: "/models", label: "模型", icon: Boxes, end: false },
  { to: "/settings", label: "设置", icon: SlidersHorizontal, end: false },
  { to: "/system", label: "系统", icon: Settings2, end: false },
];

export function AppShell() {
  const { meta, logout } = useAuth();
  const [mobileOpen, setMobileOpen] = useState(false);

  useEffect(() => {
    if (!mobileOpen) return;
    const previousOverflow = document.body.style.overflow;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setMobileOpen(false);
    };
    document.body.style.overflow = "hidden";
    window.addEventListener("keydown", closeOnEscape);
    return () => {
      document.body.style.overflow = previousOverflow;
      window.removeEventListener("keydown", closeOnEscape);
    };
  }, [mobileOpen]);

  function navigationLinks(): ReactNode {
    return navigation.map((item) => {
      const Icon = item.icon;
      return (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          onClick={() => setMobileOpen(false)}
          className={({ isActive }) =>
            cn(
              "group flex h-8 items-center gap-2 rounded-md px-2.5 text-xs font-normal text-muted-foreground transition-[background-color,color] duration-150 hover:bg-secondary/55 hover:text-foreground",
              isActive && "bg-secondary/60 text-foreground",
            )
          }
        >
          {({ isActive }) => (
            <>
              <span className="flex size-5 shrink-0 items-center justify-center">
                <Icon
                  className={cn("size-4 text-muted-foreground", isActive && "text-foreground")}
                  fill={isActive ? "currentColor" : "none"}
                  fillOpacity={isActive ? 0.14 : 0}
                  strokeWidth={1.8}
                />
              </span>
              {item.label}
            </>
          )}
        </NavLink>
      );
    });
  }

  const sidebarContent = (
    <>
      <div className="flex h-7 shrink-0 items-center justify-between px-2.5">
        <NavLink to="/" onClick={() => setMobileOpen(false)} className="flex h-7 items-center text-base font-semibold text-foreground">
          Grok2API
        </NavLink>
        <a
          href="https://github.com/AokiAx/grok2api"
          target="_blank"
          rel="noreferrer"
          className="hidden size-10 items-center justify-center rounded-md text-muted-foreground transition-[background-color,color,transform] duration-150 hover:bg-secondary/55 hover:text-foreground active:scale-95 lg:flex"
          aria-label="打开 GitHub 仓库"
        >
          <Github className="size-[15px]" strokeWidth={1.8} />
        </a>
      </div>

      <nav className="mt-7 min-h-0 flex-1 space-y-1 overflow-y-auto overscroll-contain pr-2 pb-2" aria-label="主导航">
        {navigationLinks()}
      </nav>

      <div className="relative z-10 mt-4 shrink-0 bg-sidebar pt-4">
        <div className="flex h-10 items-center gap-1 px-2.5">
          <div className="min-w-0 flex-1">
            <div className="truncate text-xs text-foreground">管理面板</div>
            <div className="truncate font-mono text-[10px] text-muted-foreground">{meta?.version || "dev"}</div>
          </div>
          <ThemeToggle />
          <button
            type="button"
            className="flex size-10 items-center justify-center rounded-md text-muted-foreground transition-[background-color,color,transform] duration-150 hover:bg-secondary/55 hover:text-foreground active:scale-95"
            onClick={() => void logout()}
            aria-label="退出登录"
            title="退出登录"
          >
            <LogOut className="size-[15px]" strokeWidth={1.8} />
          </button>
        </div>
      </div>
    </>
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <aside className="fixed inset-y-0 left-0 z-30 hidden h-screen w-[288px] flex-col overflow-hidden bg-sidebar px-4 py-6 lg:flex">
        {sidebarContent}
      </aside>

      <div className="flex min-h-screen flex-col lg:pl-[288px]">
        <header className="flex h-12 shrink-0 items-center justify-between border-b px-4 lg:hidden">
          <Button variant="ghost" size="icon" className="size-10" onClick={() => setMobileOpen(true)} aria-label="打开导航">
            <Menu className="size-4" />
          </Button>
          <span className="text-sm font-semibold">Grok2API</span>
          <ThemeToggle />
        </header>

        {mobileOpen ? (
          <div className="fixed inset-0 z-50 lg:hidden" role="dialog" aria-modal="true" aria-label="导航菜单">
            <button
              type="button"
              className="absolute inset-0 bg-black/30 backdrop-blur-[1px]"
              onClick={() => setMobileOpen(false)}
              aria-label="关闭导航"
            />
            <aside className="relative flex h-full w-72 max-w-[calc(100vw-2rem)] flex-col bg-sidebar px-3 py-4 shadow-2xl">
              <button
                type="button"
                className="absolute top-3 right-2 flex size-10 items-center justify-center rounded-md text-muted-foreground transition-[background-color,color,transform] duration-150 hover:bg-secondary/55 hover:text-foreground active:scale-95"
                onClick={() => setMobileOpen(false)}
                aria-label="关闭导航"
              >
                <X className="size-4" />
              </button>
              {sidebarContent}
            </aside>
          </div>
        ) : null}

        <main className="mx-auto w-full max-w-[1280px] flex-1 px-5 py-8 sm:px-8 lg:py-20">
          <Outlet />
        </main>
        <footer className="flex h-10 shrink-0 items-center justify-end gap-1.5 whitespace-nowrap px-5 text-[11px] text-muted-foreground sm:px-8">
          <a className="transition-colors hover:text-foreground" href="https://github.com/AokiAx/grok2api" target="_blank" rel="noreferrer">Grok2API</a>
          <span>© 2026</span>
        </footer>
      </div>
    </div>
  );
}
