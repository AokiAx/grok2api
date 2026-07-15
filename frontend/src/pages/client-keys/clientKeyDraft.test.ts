import { describe, expect, it } from "vitest";
import {
  buildClientKeyInput,
  draftFromClientKey,
  emptyKeyDraft,
  parseModelScopes,
} from "@/pages/client-keys/clientKeyDraft";

describe("client-key draft tools", () => {
  it("normalizes and deduplicates selected models", () => {
    expect(parseModelScopes(" Grok-4, grok-code\ngrok-4 ")).toEqual(["grok-4", "grok-code"]);
  });

  it("requires at least one selected model and derives allowlist vs all", () => {
    const draft = {
      ...emptyKeyDraft(),
      name: "automation",
      modelScopes: "",
    };
    expect(buildClientKeyInput(draft, {
      unlimitedRPM: true,
      unlimitedConcurrent: true,
      catalogModelIds: ["grok-4.5", "grok-code"],
    })).toBe("请至少选择一个模型");

    expect(buildClientKeyInput({
      ...draft,
      modelScopes: "grok-4.5",
    }, {
      unlimitedRPM: true,
      unlimitedConcurrent: true,
      catalogModelIds: ["grok-4.5", "grok-code"],
    })).toEqual({
      name: "automation",
      model_policy: "allowlist",
      model_scopes: ["grok-4.5"],
      rpm_limit: 0,
      max_concurrent: 0,
      expires_at: null,
    });

    expect(buildClientKeyInput({
      ...draft,
      modelScopes: "grok-4.5, grok-code",
    }, {
      unlimitedRPM: true,
      unlimitedConcurrent: true,
      catalogModelIds: ["grok-4.5", "grok-code"],
    })).toEqual({
      name: "automation",
      model_policy: "all",
      model_scopes: [],
      rpm_limit: 0,
      max_concurrent: 0,
      expires_at: null,
    });
  });

  it("rehydrates all-policy keys as full catalog selection", () => {
    const draft = draftFromClientKey({
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
      last_used_at: null,
      created_at: "2026-07-15T00:00:00Z",
      updated_at: "2026-07-15T00:00:00Z",
    }, ["grok-4.5", "grok-code"]);
    expect(parseModelScopes(draft.modelScopes)).toEqual(["grok-4.5", "grok-code"]);
  });
});
