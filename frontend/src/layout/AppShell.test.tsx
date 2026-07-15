import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { AppShell } from "@/layout/AppShell";

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({
    meta: { version: "1.0.0" },
    logout: vi.fn(),
  }),
}));

describe("AppShell", () => {
  it("exposes the primary five-item navigation", () => {
    render(
      <MemoryRouter>
        <AppShell />
      </MemoryRouter>,
    );

    expect(screen.getByRole("link", { name: "总览" })).toHaveAttribute("href", "/");
    expect(screen.getByRole("link", { name: "账号" })).toHaveAttribute("href", "/accounts");
    expect(screen.getByRole("link", { name: "密钥" })).toHaveAttribute("href", "/client-keys");
    expect(screen.getByRole("link", { name: "模型" })).toHaveAttribute("href", "/models");
    expect(screen.getByRole("link", { name: "设置" })).toHaveAttribute("href", "/settings");
    expect(screen.queryByRole("link", { name: "导入" })).toBeNull();
    expect(screen.queryByRole("link", { name: "系统" })).toBeNull();
  });
});
