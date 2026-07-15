import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AuthProvider, useAuth, useIsAuthenticated } from "@/auth/AuthContext";

const apiMocks = vi.hoisted(() => ({
  meta: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  me: vi.fn(),
  getStoredToken: vi.fn(() => ""),
  setStoredToken: vi.fn(),
  clearStoredToken: vi.fn(),
  invalidationListener: undefined as (() => void) | undefined,
  subscribeAdminSessionInvalidated: vi.fn((listener: () => void) => {
    apiMocks.invalidationListener = listener;
    return () => {
      if (apiMocks.invalidationListener === listener) apiMocks.invalidationListener = undefined;
    };
  }),
}));

vi.mock("@/api/client", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/api/client")>();
  return {
    ...original,
    getStoredToken: apiMocks.getStoredToken,
    setStoredToken: apiMocks.setStoredToken,
    clearStoredToken: apiMocks.clearStoredToken,
    subscribeAdminSessionInvalidated: apiMocks.subscribeAdminSessionInvalidated,
    adminApi: {
      ...original.adminApi,
      meta: apiMocks.meta,
      login: apiMocks.login,
      logout: apiMocks.logout,
      me: apiMocks.me,
    },
  };
});

function AuthProbe() {
  const { error, login, logout, refreshMeta } = useAuth();
  const authenticated = useIsAuthenticated();
  return (
    <div>
      <span>{authenticated ? "authenticated" : "anonymous"}</span>
      {error ? <span role="alert">{error}</span> : null}
      <button type="button" onClick={() => void login("secret", false)}>login</button>
      <button type="button" onClick={() => void logout()}>logout</button>
      <button type="button" onClick={() => void refreshMeta()}>retry meta</button>
    </div>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  apiMocks.meta.mockResolvedValue({ auth_required: true, version: "1", api_version: "v1" });
  apiMocks.me.mockResolvedValue({ id: "admin-1", username: "admin" });
  apiMocks.login.mockResolvedValue({
    admin: { id: "admin-1", username: "admin" },
    tokens: { accessToken: "memory-only" },
  });
  apiMocks.logout.mockResolvedValue({ loggedOut: true });
  apiMocks.invalidationListener = undefined;
});

describe("AuthProvider", () => {
  it("restores an existing HttpOnly-cookie session on startup without a stored token", async () => {
    render(<AuthProvider><AuthProbe /></AuthProvider>);

    expect(await screen.findByText("authenticated")).toBeInTheDocument();
    expect(apiMocks.me).toHaveBeenCalledOnce();
    expect(apiMocks.getStoredToken).not.toHaveBeenCalled();
  });

  it("passes remember to the backend login contract instead of browser storage", async () => {
    apiMocks.me.mockRejectedValueOnce(new Error("no session"));
    const user = userEvent.setup();
    render(<AuthProvider><AuthProbe /></AuthProvider>);
    await screen.findByText("anonymous");

    await user.click(screen.getByRole("button", { name: "login" }));

    expect(apiMocks.login).toHaveBeenCalledWith("secret", false);
    expect(apiMocks.setStoredToken).not.toHaveBeenCalled();
    expect(await screen.findByText("authenticated")).toBeInTheDocument();
  });

  it("clears protected UI state before a hanging backend logout completes", async () => {
    let finishLogout!: () => void;
    apiMocks.logout.mockReturnValue(new Promise((resolve) => {
      finishLogout = () => resolve({ loggedOut: true });
    }));
    const user = userEvent.setup();
    render(<AuthProvider><AuthProbe /></AuthProvider>);
    await screen.findByText("authenticated");

    await user.click(screen.getByRole("button", { name: "logout" }));
    expect(screen.getByText("anonymous")).toBeInTheDocument();
    finishLogout();

    expect(apiMocks.logout).toHaveBeenCalledOnce();
  });

  it("leaves protected state when the API transport reports final session invalidation", async () => {
    render(<AuthProvider><AuthProbe /></AuthProvider>);
    await screen.findByText("authenticated");

    act(() => apiMocks.invalidationListener?.());

    expect(await screen.findByText("anonymous")).toBeInTheDocument();
  });

  it("exposes meta loading failures and retries the complete session probe", async () => {
    apiMocks.meta.mockRejectedValueOnce(new Error("gateway unavailable"));
    const user = userEvent.setup();
    render(<AuthProvider><AuthProbe /></AuthProvider>);

    expect(await screen.findByRole("alert")).toHaveTextContent("gateway unavailable");
    await user.click(screen.getByRole("button", { name: "retry meta" }));

    expect(await screen.findByText("authenticated")).toBeInTheDocument();
    expect(apiMocks.meta).toHaveBeenCalledTimes(2);
  });
});
