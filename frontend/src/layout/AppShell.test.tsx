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
  it("exposes client-key administration in the primary navigation", () => {
    render(<MemoryRouter><AppShell /></MemoryRouter>);

    expect(screen.getByRole("link", { name: "客户端密钥" })).toHaveAttribute("href", "/client-keys");
  });
});
