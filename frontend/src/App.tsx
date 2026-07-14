import { Navigate, Route, Routes } from "react-router-dom";
import { useAuth, useIsAuthenticated } from "@/auth/AuthContext";
import { AppShell } from "@/layout/AppShell";
import { AccountsPage } from "@/pages/AccountsPage";
import { DashboardPage } from "@/pages/DashboardPage";
import { ImportPage } from "@/pages/ImportPage";
import { LoginPage } from "@/pages/LoginPage";
import { SystemPage } from "@/pages/SystemPage";

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { ready } = useAuth();
  const authed = useIsAuthenticated();
  if (!ready) {
    return (
      <div className="flex min-h-screen items-center justify-center text-sm text-muted-foreground">
        加载中…
      </div>
    );
  }
  if (!authed) return <Navigate to="/login" replace />;
  return children;
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        }
      >
        <Route index element={<DashboardPage />} />
        <Route path="accounts" element={<AccountsPage />} />
        <Route path="import" element={<ImportPage />} />
        <Route path="system" element={<SystemPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
