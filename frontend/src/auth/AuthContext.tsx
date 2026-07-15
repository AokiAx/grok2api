import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  adminApi,
  subscribeAdminSessionInvalidated,
  type SystemMeta,
} from "@/api/client";

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
  const probeGeneration = useRef(0);

  const refreshMeta = useCallback(async () => {
    const generation = probeGeneration.current + 1;
    probeGeneration.current = generation;
    setReady(false);
    setAuthenticated(false);
    setMeta(null);
    setError(null);
    try {
      const nextMeta = await adminApi.meta();
      if (generation !== probeGeneration.current) return;
      setMeta(nextMeta);
      if (!nextMeta.auth_required) {
        setAuthenticated(true);
        return;
      }
      if (nextMeta.setup_required) return;
      try {
        await adminApi.me();
        if (generation === probeGeneration.current) setAuthenticated(true);
      } catch {
        if (generation === probeGeneration.current) setAuthenticated(false);
      }
    } catch (err) {
      if (generation === probeGeneration.current) {
        setError(err instanceof Error ? err.message : "Failed to load meta");
      }
    } finally {
      if (generation === probeGeneration.current) setReady(true);
    }
  }, []);

  useEffect(() => {
    return subscribeAdminSessionInvalidated(() => {
      setAuthenticated(false);
      setError("登录会话已失效，请重新登录");
    });
  }, []);

  useEffect(() => {
    void refreshMeta();
    return () => {
      probeGeneration.current += 1;
    };
  }, [refreshMeta]);

  const login = useCallback(async (password: string, remember: boolean) => {
    setError(null);
    await adminApi.login(password, remember);
    setAuthenticated(true);
    setError(null);
  }, []);

  const logout = useCallback(async () => {
    probeGeneration.current += 1;
    setAuthenticated(false);
    setError(null);
    try {
      await adminApi.logout();
    } catch {
      // Local session invalidation is authoritative; server logout is best effort.
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
