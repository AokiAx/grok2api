import { render, screen, waitFor, within } from "@testing-library/react";
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
  models: vi.fn(),
  settings: vi.fn(),
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
  apiMocks.models.mockResolvedValue({
    count: 2,
    enabled: 2,
    models: [
      { id: "grok-4.5", name: "Grok 4.5", enabled: true, aliases: [] },
      { id: "grok-code", name: "Grok Code", enabled: true, aliases: [] },
    ],
  });
  apiMocks.settings.mockResolvedValue({
    revision: 1,
    updated_at: "2026-07-15T00:00:00Z",
    pool: {},
    timeouts: {},
    audit: {},
    proxy: {},
    client_keys: { default_rpm_limit: 120, default_max_concurrent: 4 },
  });
});

describe("ClientKeysPage", () => {
  it("creates a key from the model list without a separate policy selector", async () => {
    const copy = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: copy } });
    render(<ClientKeysPage />);
    await screen.findByText("automation");

    await user.click(screen.getByRole("button", { name: "创建密钥" }));
    const dialog = await screen.findByRole("dialog", { name: "创建密钥" });
    expect(await within(dialog).findByText(/默认来自设置/)).toBeInTheDocument();
    await within(dialog).findByText("grok-4.5");

    await user.click(within(dialog).getByRole("button", { name: "生成密钥" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("请输入密钥名称");

    await user.type(within(dialog).getByLabelText("名称"), "ci-agent");
    await user.click(within(dialog).getByRole("button", { name: "生成密钥" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("请至少选择一个模型");
    expect(apiMocks.createClientKey).not.toHaveBeenCalled();

    await user.click(within(dialog).getByRole("button", { name: "全选" }));
    await user.click(within(dialog).getByRole("checkbox", { name: "RPM 不限" }));
    await user.click(within(dialog).getByRole("checkbox", { name: "并发不限" }));
    await user.click(within(dialog).getByRole("button", { name: "生成密钥" }));

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

  it("loads details, edits selected models, and revokes a key", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<ClientKeysPage />);

    await user.click(await screen.findByRole("button", { name: "查看 automation" }));
    expect(apiMocks.clientKey).toHaveBeenCalledWith("ck_1");
    const dialog = await screen.findByRole("dialog", { name: "密钥详情" });

    await waitFor(() => {
      expect(within(dialog).getByRole("checkbox", { name: /grok-4\.5/i })).toBeChecked();
    });

    // Drop one model so backend becomes allowlist.
    await user.click(within(dialog).getByRole("checkbox", { name: /grok-code/i }));

    const name = within(dialog).getByLabelText("密钥名称");
    await user.clear(name);
    await user.type(name, "renamed");
    await user.clear(within(dialog).getByLabelText("每分钟请求数"));
    await user.type(within(dialog).getByLabelText("每分钟请求数"), "30");
    await user.clear(within(dialog).getByLabelText("最大并发"));
    await user.type(within(dialog).getByLabelText("最大并发"), "3");
    await user.click(within(dialog).getByRole("button", { name: "保存修改" }));

    expect(apiMocks.updateClientKey).toHaveBeenCalledWith("ck_1", expect.objectContaining({
      name: "renamed",
      model_policy: "allowlist",
      model_scopes: ["grok-4.5"],
      rpm_limit: 30,
      max_concurrent: 3,
    }));

    await user.click(within(dialog).getByRole("button", { name: "撤销密钥" }));
    expect(apiMocks.revokeClientKey).toHaveBeenCalledWith("ck_1");
  });

  it("requires explicit unlimited confirmation when editing zero limits", async () => {
    const user = userEvent.setup();
    render(<ClientKeysPage />);

    await user.click(await screen.findByRole("button", { name: "查看 automation" }));
    const dialog = await screen.findByRole("dialog", { name: "密钥详情" });
    await waitFor(() => {
      expect(within(dialog).getByRole("checkbox", { name: /grok-4\.5/i })).toBeChecked();
    });

    await user.clear(within(dialog).getByLabelText("每分钟请求数"));
    await user.type(within(dialog).getByLabelText("每分钟请求数"), "0");
    await user.clear(within(dialog).getByLabelText("最大并发"));
    await user.type(within(dialog).getByLabelText("最大并发"), "0");
    await user.click(within(dialog).getByRole("button", { name: "保存修改" }));

    expect(within(dialog).getByRole("alert")).toHaveTextContent(/RPM|并发/);
    expect(apiMocks.updateClientKey).not.toHaveBeenCalled();

    await user.click(within(dialog).getByRole("checkbox", { name: "RPM 不限" }));
    await user.click(within(dialog).getByRole("checkbox", { name: "并发不限" }));
    await user.click(within(dialog).getByRole("button", { name: "保存修改" }));

    expect(apiMocks.updateClientKey).toHaveBeenCalledWith("ck_1", expect.objectContaining({
      rpm_limit: 0,
      max_concurrent: 0,
    }));
  });

  it("preserves persisted unlimited decisions when editing other key fields", async () => {
    apiMocks.clientKey.mockResolvedValue({
      ...activeKey,
      rpm_limit: 0,
      max_concurrent: 0,
    });
    const user = userEvent.setup();
    render(<ClientKeysPage />);

    await user.click(await screen.findByRole("button", { name: "查看 automation" }));
    const dialog = await screen.findByRole("dialog", { name: "密钥详情" });
    await waitFor(() => {
      expect(within(dialog).getByRole("checkbox", { name: /grok-4\.5/i })).toBeChecked();
    });

    expect(within(dialog).getByRole("checkbox", { name: "RPM 不限" })).toBeChecked();
    expect(within(dialog).getByRole("checkbox", { name: "并发不限" })).toBeChecked();

    const name = within(dialog).getByLabelText("密钥名称");
    await user.clear(name);
    await user.type(name, "renamed-unlimited");
    await user.click(within(dialog).getByRole("button", { name: "保存修改" }));

    expect(apiMocks.updateClientKey).toHaveBeenCalledWith("ck_1", expect.objectContaining({
      name: "renamed-unlimited",
      rpm_limit: 0,
      max_concurrent: 0,
    }));
  });
});
