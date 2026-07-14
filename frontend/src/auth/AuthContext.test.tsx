import { render, screen } from "@testing-library/react";
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
}));

vi.mock("@/api/client", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/api/client")>();
  return {
    ...original,
    getStoredToken: apiMocks.getStoredToken,
    setStoredToken: apiMocks.setStoredToken,
    clearStoredToken: apiMocks.clearStoredToken,
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
  const { login, logout } = useAuth();
  const authenticated = useIsAuthenticated();
  return (
    <div>
      <span>{authenticated ? "authenticated" : "anonymous"}</span>
      <button type="button" onClick={() => void login("secret", false)}>login</button>
      <button type="button" onClick={() => void logout()}>logout</button>
    </div>
  );
}

beforeEach(() => {
  apiMocks.meta.mockResolvedValue({ auth_required: true, version: "1", api_version: "v1" });
  apiMocks.me.mockResolvedValue({ id: "admin-1", username: "admin" });
  apiMocks.login.mockResolvedValue({
    admin: { id: "admin-1", username: "admin" },
    tokens: { accessToken: "memory-only" },
  });
  apiMocks.logout.mockResolvedValue({ loggedOut: true });
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

  it("waits for backend logout before clearing the authenticated UI state", async () => {
    let finishLogout!: () => void;
    apiMocks.logout.mockReturnValue(new Promise((resolve) => {
      finishLogout = () => resolve({ loggedOut: true });
    }));
    const user = userEvent.setup();
    render(<AuthProvider><AuthProbe /></AuthProvider>);
    await screen.findByText("authenticated");

    await user.click(screen.getByRole("button", { name: "logout" }));
    expect(screen.getByText("authenticated")).toBeInTheDocument();
    finishLogout();

    expect(await screen.findByText("anonymous")).toBeInTheDocument();
    expect(apiMocks.logout).toHaveBeenCalledOnce();
  });
});
