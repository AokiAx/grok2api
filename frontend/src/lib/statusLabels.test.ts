import { describe, expect, it } from "vitest";
import { poolLabel, statusCodeLabel, unavailableReasonLabel } from "@/lib/statusLabels";

describe("statusLabels", () => {
  it("localizes account pools", () => {
    expect(poolLabel("ready")).toBe("可用");
    expect(poolLabel("unavailable")).toBe("不可用");
    expect(poolLabel("all")).toBe("全部");
  });

  it("localizes known unavailable reasons and leaves unknown codes intact", () => {
    expect(unavailableReasonLabel("auth")).toBe("认证失败");
    expect(unavailableReasonLabel("quota")).toBe("额度耗尽");
    expect(unavailableReasonLabel("cooldown")).toBe("冷却中");
    expect(unavailableReasonLabel("validating")).toBe("校验中");
    expect(unavailableReasonLabel("disabled")).toBe("已禁用");
    expect(unavailableReasonLabel("local:quota-exhausted")).toBe("local:quota-exhausted");
  });

  it("maps both pools and reasons for dashboard reason maps", () => {
    expect(statusCodeLabel("ready")).toBe("可用");
    expect(statusCodeLabel("auth")).toBe("认证失败");
  });
});
