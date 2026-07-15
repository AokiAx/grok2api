import { describe, expect, it } from "vitest";
import { buildClientKeyInput, emptyKeyDraft, parseModelScopes } from "@/pages/client-keys/clientKeyDraft";

describe("client-key draft tools", () => {
  it("normalizes and deduplicates allowlisted models", () => {
    expect(parseModelScopes(" Grok-4, grok-code\ngrok-4 ")).toEqual(["grok-4", "grok-code"]);
  });

  it("keeps unlimited access behind explicit decisions", () => {
    const draft = {
      ...emptyKeyDraft(),
      name: "automation",
      modelPolicy: "all" as const,
    };
    expect(buildClientKeyInput(draft, {
      allModelsConfirmed: false,
      unlimitedRPM: true,
      unlimitedConcurrent: true,
    })).toBe("请确认允许访问全部模型");
    expect(buildClientKeyInput(draft, {
      allModelsConfirmed: true,
      unlimitedRPM: true,
      unlimitedConcurrent: true,
    })).toEqual({
      name: "automation",
      model_policy: "all",
      model_scopes: [],
      rpm_limit: 0,
      max_concurrent: 0,
      expires_at: null,
    });
  });
});
