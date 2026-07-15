import { Navigate, Route, Routes } from "react-router-dom";
import { useAuth, useIsAuthenticated } from "@/auth/AuthContext";
import { AppShell } from "@/layout/AppShell";
import { AccountsPage } from "@/pages/AccountsPage";
import { ClientKeysPage } from "@/pages/ClientKeysPage";
import { DashboardPage } from "@/pages/DashboardPage";
import { LoginPage } from "@/pages/LoginPage";
import { ModelsPage } from "@/pages/ModelsPage";
import { SettingsPage } from "@/pages/SettingsPage";

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
        <Route path="client-keys" element={<ClientKeysPage />} />
        <Route path="models" element={<ModelsPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="import" element={<Navigate to="/accounts" replace />} />
        <Route path="system" element={<Navigate to="/settings" replace />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
