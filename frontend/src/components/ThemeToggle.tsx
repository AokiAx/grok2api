import { Monitor, Moon, Sun } from "lucide-react";
import { useEffect, useState } from "react";
import { cn } from "@/lib/cn";

type Theme = "light" | "dark" | "system";

const storageKey = "grok2api-theme";
const themeEvent = "grok2api:theme-change";

function storedTheme(): Theme {
  if (typeof window === "undefined") return "system";
  const value = window.localStorage.getItem(storageKey);
  return value === "light" || value === "dark" || value === "system" ? value : "system";
}

function applyTheme(theme: Theme) {
  if (typeof window === "undefined") return;
  const dark = theme === "dark" || (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
}

applyTheme(storedTheme());

const nextTheme: Record<Theme, Theme> = {
  system: "light",
  light: "dark",
  dark: "system",
};

const themeLabel: Record<Theme, string> = {
  system: "跟随系统",
  light: "浅色模式",
  dark: "深色模式",
};

export function ThemeToggle({ className }: { className?: string }) {
  const [theme, setTheme] = useState<Theme>(storedTheme);

  useEffect(() => {
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const syncSystem = () => {
      if (storedTheme() === "system") applyTheme("system");
    };
    const syncTheme = () => {
      const current = storedTheme();
      setTheme(current);
      applyTheme(current);
    };

    media.addEventListener("change", syncSystem);
    window.addEventListener("storage", syncTheme);
    window.addEventListener(themeEvent, syncTheme);
    return () => {
      media.removeEventListener("change", syncSystem);
      window.removeEventListener("storage", syncTheme);
      window.removeEventListener(themeEvent, syncTheme);
    };
  }, []);

  const Icon = theme === "light" ? Sun : theme === "dark" ? Moon : Monitor;

  function cycleTheme() {
    const next = nextTheme[theme];
    window.localStorage.setItem(storageKey, next);
    applyTheme(next);
    setTheme(next);
    window.dispatchEvent(new Event(themeEvent));
  }

  return (
    <button
      type="button"
      className={cn(
        "flex size-10 items-center justify-center rounded-md text-muted-foreground transition-[background-color,color,transform] duration-150 ease-out hover:bg-secondary/55 hover:text-foreground active:scale-95",
        className,
      )}
      onClick={cycleTheme}
      aria-label={`${themeLabel[theme]}，点击切换主题`}
      title={`${themeLabel[theme]}，点击切换`}
    >
      <Icon className="size-[15px]" strokeWidth={1.8} />
    </button>
  );
}
