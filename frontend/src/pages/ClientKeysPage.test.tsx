import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ClientKeysPage } from "@/pages/ClientKeysPage";

const activeKey = {
  id: "ck_1",
  name: "automation",
  origin: "managed",
  key_prefix: "g2a_abcd",
  model_policy: "all",
  model_scopes: [],
  rpm_limit: 60,
  max_concurrent: 2,
  expires_at: null,
  revoked_at: null,
  last_used_at: "2026-07-15T01:00:00Z",
  created_at: "2026-07-15T00:00:00Z",
  updated_at: "2026-07-15T00:00:00Z",
};

const apiMocks = vi.hoisted(() => ({
  clientKeys: vi.fn(),
  createClientKey: vi.fn(),
  clientKey: vi.fn(),
  updateClientKey: vi.fn(),
  revokeClientKey: vi.fn(),
}));

vi.mock("@/api/client", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/api/client")>();
  return {
    ...original,
    adminApi: {
      ...original.adminApi,
      ...apiMocks,
    },
  };
});

beforeEach(() => {
  vi.clearAllMocks();
  apiMocks.clientKeys.mockResolvedValue({ items: [activeKey], total: 1, page: 1, page_size: 20 });
  apiMocks.clientKey.mockResolvedValue(activeKey);
  apiMocks.createClientKey.mockResolvedValue({
    ...activeKey,
    id: "ck_created",
    name: "ci-agent",
    key_prefix: "g2a_new",
    rpm_limit: 0,
    max_concurrent: 0,
    secret: "g2a_once_only_secret",
  });
  apiMocks.updateClientKey.mockResolvedValue({ ...activeKey, name: "renamed" });
  apiMocks.revokeClientKey.mockResolvedValue({ ...activeKey, revoked_at: "2026-07-15T02:00:00Z" });
});

describe("ClientKeysPage", () => {
  it("requires explicit model and limit decisions before creating and reveals the secret once", async () => {
    const copy = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: copy } });
    render(<ClientKeysPage />);
    await screen.findByText("automation");

    await user.click(screen.getByRole("button", { name: "创建客户端密钥" }));
    await user.click(screen.getByRole("button", { name: "生成密钥" }));
    expect(screen.getByRole("alert")).toHaveTextContent("请选择模型权限");
    expect(apiMocks.createClientKey).not.toHaveBeenCalled();

    await user.type(screen.getByLabelText("名称"), "ci-agent");
    await user.selectOptions(screen.getByLabelText("模型权限"), "all");
    await user.click(screen.getByRole("checkbox", { name: "我确认此密钥可访问全部模型" }));
    await user.click(screen.getByRole("checkbox", { name: "RPM 不限" }));
    await user.click(screen.getByRole("checkbox", { name: "并发不限" }));
    await user.click(screen.getByRole("button", { name: "生成密钥" }));

    expect(apiMocks.createClientKey).toHaveBeenCalledWith({
      name: "ci-agent",
      model_policy: "all",
      model_scopes: [],
      rpm_limit: 0,
      max_concurrent: 0,
      expires_at: null,
    });
    expect(await screen.findByText("g2a_once_only_secret")).toBeInTheDocument();
    expect(screen.getByText(/关闭后无法再次查看/)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "复制密钥" }));
    expect(copy).toHaveBeenCalledWith("g2a_once_only_secret");
    await user.click(screen.getByRole("button", { name: "完成" }));
    expect(screen.queryByText("g2a_once_only_secret")).not.toBeInTheDocument();
  });

  it("loads details, edits policy settings, and revokes a key", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<ClientKeysPage />);

    await user.click(await screen.findByRole("button", { name: "查看 automation" }));
    expect(apiMocks.clientKey).toHaveBeenCalledWith("ck_1");
    expect(await screen.findByRole("heading", { name: "密钥详情" })).toBeInTheDocument();

    const name = screen.getByLabelText("密钥名称");
    await user.clear(name);
    await user.type(name, "renamed");
    await user.selectOptions(screen.getByLabelText("模型权限"), "allowlist");
    await user.type(screen.getByLabelText("允许的模型"), "grok-4, grok-code");
    await user.clear(screen.getByLabelText("每分钟请求数"));
    await user.type(screen.getByLabelText("每分钟请求数"), "30");
    await user.clear(screen.getByLabelText("最大并发"));
    await user.type(screen.getByLabelText("最大并发"), "3");
    await user.click(screen.getByRole("button", { name: "保存修改" }));

    expect(apiMocks.updateClientKey).toHaveBeenCalledWith("ck_1", expect.objectContaining({
      name: "renamed",
      model_policy: "allowlist",
      model_scopes: ["grok-4", "grok-code"],
      rpm_limit: 30,
      max_concurrent: 3,
    }));

    await user.click(screen.getByRole("button", { name: "撤销密钥" }));
    expect(apiMocks.revokeClientKey).toHaveBeenCalledWith("ck_1");
  });

  it("requires explicit unlimited confirmation when editing zero limits", async () => {
    const user = userEvent.setup();
    render(<ClientKeysPage />);

    await user.click(await screen.findByRole("button", { name: "查看 automation" }));
    await screen.findByRole("heading", { name: "密钥详情" });

    await user.clear(screen.getByLabelText("每分钟请求数"));
    await user.type(screen.getByLabelText("每分钟请求数"), "0");
    await user.clear(screen.getByLabelText("最大并发"));
    await user.type(screen.getByLabelText("最大并发"), "0");
    await user.click(screen.getByRole("button", { name: "保存修改" }));

    expect(screen.getByRole("alert")).toHaveTextContent("RPM 不限");
    expect(apiMocks.updateClientKey).not.toHaveBeenCalled();

    await user.click(screen.getByRole("checkbox", { name: "RPM 不限" }));
    await user.click(screen.getByRole("checkbox", { name: "并发不限" }));
    await user.click(screen.getByRole("button", { name: "保存修改" }));

    expect(apiMocks.updateClientKey).toHaveBeenCalledWith("ck_1", expect.objectContaining({
      rpm_limit: 0,
      max_concurrent: 0,
    }));
  });
});
