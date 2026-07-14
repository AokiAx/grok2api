import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  adminApi,
  clearStoredToken,
  getStoredToken,
  setStoredToken,
  type SystemMeta,
} from "@/api/client";

type AuthState = {
  ready: boolean;
  token: string;
  meta: SystemMeta | null;
  error: string | null;
  login: (password: string, remember: boolean) => Promise<void>;
  logout: () => void;
  refreshMeta: () => Promise<void>;
};

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false);
  const [token, setToken] = useState("");
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
        const stored = getStoredToken();
        if (!nextMeta.auth_required) {
          setToken("");
          setReady(true);
          return;
        }
        if (!stored) {
          setReady(true);
          return;
        }
        try {
          await adminApi.me();
          if (!cancelled) setToken(stored);
        } catch {
          clearStoredToken();
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
    const result = await adminApi.login(password);
    const next = result.token || password;
    setStoredToken(next, remember);
    setToken(next);
  }, []);

  const logout = useCallback(() => {
    clearStoredToken();
    setToken("");
  }, []);

  const value = useMemo(
    () => ({ ready, token, meta, error, login, logout, refreshMeta }),
    [ready, token, meta, error, login, logout, refreshMeta],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth outside AuthProvider");
  return ctx;
}

export function useIsAuthenticated(): boolean {
  const { ready, token, meta } = useAuth();
  if (!ready) return false;
  if (meta && !meta.auth_required) return true;
  return token.length > 0;
}
