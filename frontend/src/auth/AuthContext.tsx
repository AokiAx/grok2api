import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { adminApi, type SystemMeta } from "@/api/client";

type AuthState = {
  ready: boolean;
  authenticated: boolean;
  meta: SystemMeta | null;
  error: string | null;
  login: (password: string, remember: boolean) => Promise<void>;
  logout: () => Promise<void>;
  refreshMeta: () => Promise<void>;
};

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false);
  const [authenticated, setAuthenticated] = useState(false);
  const [meta, setMeta] = useState<SystemMeta | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refreshMeta = useCallback(async () => {
    setMeta(await adminApi.meta());
  }, []);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const nextMeta = await adminApi.meta();
        if (cancelled) return;
        setMeta(nextMeta);
        if (!nextMeta.auth_required) {
          setAuthenticated(true);
          setReady(true);
          return;
        }
        try {
          await adminApi.me();
          if (!cancelled) setAuthenticated(true);
        } catch {
          if (!cancelled) setAuthenticated(false);
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : "Failed to load meta");
      } finally {
        if (!cancelled) setReady(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(async (password: string, remember: boolean) => {
    setError(null);
    await adminApi.login(password, remember);
    setAuthenticated(true);
  }, []);

  const logout = useCallback(async () => {
    try {
      await adminApi.logout();
    } finally {
      setAuthenticated(false);
    }
  }, []);

  const value = useMemo(
    () => ({ ready, authenticated, meta, error, login, logout, refreshMeta }),
    [ready, authenticated, meta, error, login, logout, refreshMeta],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth outside AuthProvider");
  return ctx;
}

export function useIsAuthenticated(): boolean {
  const { ready, authenticated, meta } = useAuth();
  if (!ready) return false;
  if (meta && !meta.auth_required) return true;
  return authenticated;
}
